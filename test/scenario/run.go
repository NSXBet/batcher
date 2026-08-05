package scenario

import (
	"errors"
	"math/rand"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/NSXBet/batcher/pkg/batcher"
)

// Item is the payload pushed through the batcher. It carries a flat index into
// the preallocated recording slots, so the processor records timings with a
// single atomic store: no lookup, no allocation, no shared-map contention.
type Item struct {
	index int
	_     [48]byte // padding: keeps a realistic payload size
}

// slot holds one item's timings. Producer and processor touch disjoint atomics,
// so recording is race-free without a lock on either hot path.
type slot struct {
	scheduledAt    atomic.Int64
	admissionStart atomic.Int64
	admissionEnd   atomic.Int64
	processorStart atomic.Int64
	processorEnd   atomic.Int64
	accepted       atomic.Bool
}

// Config describes one scenario run.
type Config struct {
	Name string

	BatchSize     int
	BatchInterval time.Duration

	Arrival   Arrival
	Processor Processor

	// Producers is how many goroutines offer the schedule concurrently. The
	// schedule is partitioned across them, so offered load is independent of
	// producer count.
	Producers int

	// Warmup discards samples offered before this point, so scheduler and
	// allocator warm-up does not pollute the distributions.
	Warmup time.Duration

	// Seed fixes all randomness (arrival jitter and processor service time).
	Seed int64

	// LatenessBudget marks a run invalid when the generator itself fell behind by
	// more than this at p99. Without this guard, an overloaded generator would be
	// silently reported as batcher latency.
	LatenessBudget time.Duration
}

func (c Config) withDefaults() Config {
	if c.Producers <= 0 {
		c.Producers = 1
	}

	if c.Seed == 0 {
		c.Seed = 1
	}

	if c.LatenessBudget <= 0 {
		c.LatenessBudget = 5 * time.Millisecond
	}

	return c
}

// Result is everything one run measured.
type Result struct {
	Config Config

	// Validity
	Lateness      Distribution
	LatenessValid bool

	// Latency
	EndToEnd          Distribution
	QueueDelay        Distribution
	AdmissionBlocking Distribution

	// Rates, in items per second over the offered window.
	OfferedRate   float64
	AcceptedRate  float64
	CompletedRate float64
	RejectedCount int

	// Batching efficiency
	Batches          int
	MeanBatchSize    float64
	PartialBatches   int
	BatchSizes       Distribution // as durations for reuse; nanoseconds == items
	DownstreamPerSec float64

	// Resources
	AllocsPerItem  float64
	HeapHighWater  uint64 // peak HeapAlloc observed while load was in flight
	PendingPeak    int64  // peak accepted-but-unfinished items (queue depth proxy)
	GCCount        uint32
	GCPauseTotal   time.Duration
	GoroutinesPeak int

	Duration time.Duration
}

// Run executes one scenario and returns its measurements.
//
// Completion is observed through the processor itself: the run finishes when the
// processor has accounted for every accepted item. Join is deliberately not used
// as the completion signal, since its 1ms polling would quantise results in the
// sub-10ms range this harness exists to measure.
func Run(cfg Config) Result {
	cfg = cfg.withDefaults()

	schedule := cfg.Arrival.Schedule(cfg.Seed)
	if len(schedule) == 0 {
		return Result{Config: cfg, LatenessValid: true}
	}

	// Partition the schedule across producers, preserving both global offer times
	// and each item's flat recording index.
	type offer struct {
		at    time.Duration
		index int
	}

	perProducer := make([][]offer, cfg.Producers)
	for i, at := range schedule {
		p := i % cfg.Producers
		perProducer[p] = append(perProducer[p], offer{at: at, index: i})
	}

	// Preallocate every recording slot before measurement begins, so neither the
	// producing nor the processing path allocates while being measured.
	slots := make([]slot, len(schedule))

	var (
		batches     []int
		batchesMu   sync.Mutex
		completed   = make(chan int, 1024)
		procRNG     = rand.New(rand.NewSource(cfg.Seed))
		start       time.Time
		goroutinePk = runtime.NumGoroutine()
	)

	b := batcher.New(
		batcher.WithBatchSize[Item](cfg.BatchSize),
		batcher.WithBatchInterval[Item](cfg.BatchInterval),
		batcher.WithProcessor(func(items []Item) error {
			procStart := int64(time.Since(start))

			if d := cfg.Processor.ServiceTime(procRNG, len(items)); d > 0 {
				time.Sleep(d)
			}

			procEnd := int64(time.Since(start))

			for _, it := range items {
				slots[it.index].processorStart.Store(procStart)
				slots[it.index].processorEnd.Store(procEnd)
			}

			batchesMu.Lock()
			batches = append(batches, len(items))
			batchesMu.Unlock()

			completed <- len(items)

			return cfg.Processor.Err
		}),
	)

	// Drain diagnostics so an erroring processor cannot grow memory unboundedly
	// during the run.
	errsDone := make(chan struct{})
	go func() {
		defer close(errsDone)
		for range b.Errors() {
		}
	}()

	var memBefore, memAfter runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&memBefore)

	// Sample heap and queue depth while load is in flight. Reading only after the
	// drain would report a recovered process and hide the peak backlog, which is
	// precisely the number overload scenarios need.
	var (
		heapPeak    atomic.Uint64
		pendingPeak atomic.Int64
		samplerDone = make(chan struct{})
		samplerStop = make(chan struct{})
	)

	go func() {
		defer close(samplerDone)

		ticker := time.NewTicker(2 * time.Millisecond)
		defer ticker.Stop()

		var ms runtime.MemStats

		for {
			select {
			case <-samplerStop:
				return
			case <-ticker.C:
				runtime.ReadMemStats(&ms)

				for {
					prev := heapPeak.Load()
					if ms.HeapAlloc <= prev || heapPeak.CompareAndSwap(prev, ms.HeapAlloc) {
						break
					}
				}

				if depth := int64(b.Len()); depth > pendingPeak.Load() {
					pendingPeak.Store(depth)
				}
			}
		}
	}()

	var (
		wg       sync.WaitGroup
		accepted = make([]int, cfg.Producers)
	)

	start = time.Now()

	for p := range cfg.Producers {
		wg.Add(1)

		go func(producer int) {
			defer wg.Done()

			for _, o := range perProducer[producer] {
				// Open loop: sleep until the item's scheduled time, regardless of
				// how the batcher is coping. Never gate on completion or capacity.
				if d := o.at - time.Since(start); d > 0 {
					time.Sleep(d)
				}

				s := &slots[o.index]
				s.scheduledAt.Store(int64(o.at))
				s.admissionStart.Store(int64(time.Since(start)))

				b.Add(Item{index: o.index})

				s.admissionEnd.Store(int64(time.Since(start)))
				s.accepted.Store(true)
				accepted[producer]++
			}
		}(p)
	}

	wg.Wait()
	offeredFor := time.Since(start)

	if n := runtime.NumGoroutine(); n > goroutinePk {
		goroutinePk = n
	}

	totalAccepted := 0
	for _, n := range accepted {
		totalAccepted += n
	}

	// Wait for the processor to account for every accepted item.
	deadline := time.NewTimer(60 * time.Second)
	defer deadline.Stop()

	for done := 0; done < totalAccepted; {
		select {
		case n := <-completed:
			done += n
		case <-deadline.C:
			// Give up rather than hang; the result will show the shortfall.
			done = totalAccepted
		}
	}

	runDuration := time.Since(start)

	close(samplerStop)
	<-samplerDone

	runtime.ReadMemStats(&memAfter)

	if err := b.Close(); err != nil && !errors.Is(err, batcher.ErrTimeout) {
		// Close errors are reported through the result's completed counts.
		_ = err
	}

	<-errsDone

	return summarise(cfg, slots, batches, offeredFor, runDuration,
		memBefore, memAfter, goroutinePk, heapPeak.Load(), pendingPeak.Load())
}
