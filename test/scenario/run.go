package scenario

import (
	"math/rand"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/NSXBet/batcher/pkg/batcher"
)

// completionDeadline bounds how long a run waits for the processor to account for
// every accepted item. Exceeding it marks the run as timed out rather than hanging
// the whole sweep.
const completionDeadline = 60 * time.Second

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

	// CompletionDeadline bounds how long the harness waits for the processor to
	// account for every accepted item. Zero selects the package default. A timeout
	// is reported in Result rather than leaking the processor on a full completion
	// channel.
	CompletionDeadline time.Duration

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

	if c.CompletionDeadline <= 0 {
		c.CompletionDeadline = completionDeadline
	}

	return c
}

// Result is everything one run measured.
type Result struct {
	Config Config

	// OfferedFor is the wall time spent offering the schedule. Offered and accepted
	// rates are computed over this window, so any derived figure must use it rather
	// than the configured duration: an open-loop generator on a slow host takes
	// longer than its schedule implies.
	OfferedFor time.Duration

	// TimedOut reports that the run gave up waiting for completion. Latency and
	// batching figures from such a run describe an incomplete run and must not be
	// compared with a complete one.
	TimedOut bool

	// CloseErr is whatever shutting the batcher down reported. A non-nil value with
	// TimedOut set usually means the processor was still blocked.
	CloseErr error

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
		batches   []int
		batchesMu sync.Mutex

		// completedItems and completedSignal replace a buffered channel on purpose.
		// A bounded channel blocks the processor forever once the waiter stops
		// reading it, which happens whenever the completion deadline expires: the
		// batcher's processing goroutine parks on the send, Close cannot drain, and
		// each timed-out run leaks a goroutine and a blocked batch. The matrix sweep
		// performs over a hundred runs in one process, so that compounds.
		//
		// An atomic counter with a non-blocking latch cannot block the processor at
		// all, at the cost of the waiter re-reading the counter after each wakeup.
		completedItems  atomic.Int64
		completedSignal = make(chan struct{}, 1)

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

			completedItems.Add(int64(len(items)))

			// Non-blocking: a latch that is already armed needs no second signal,
			// because the waiter re-reads the counter after waking.
			select {
			case completedSignal <- struct{}{}:
			default:
			}

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
	deadline := time.NewTimer(cfg.CompletionDeadline)
	defer deadline.Stop()

	timedOut := false

	for completedItems.Load() < int64(totalAccepted) {
		select {
		case <-completedSignal:
			// Re-check the counter on the next loop iteration.
		case <-deadline.C:
			// Give up rather than hang. The shortfall is reported rather than
			// silently folded into the completion counts.
			timedOut = true
		}

		if timedOut {
			break
		}
	}

	runDuration := time.Since(start)

	close(samplerStop)
	<-samplerDone

	runtime.ReadMemStats(&memAfter)

	closeErr := b.Close()

	<-errsDone

	result := summarise(cfg, slots, batches, offeredFor, runDuration,
		memBefore, memAfter, goroutinePk, heapPeak.Load(), pendingPeak.Load())

	result.OfferedFor = offeredFor
	result.TimedOut = timedOut
	result.CloseErr = closeErr

	return result
}
