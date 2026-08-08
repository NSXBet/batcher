package scenario

import (
	"math/rand"
	"runtime"
	"runtime/debug"
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

	// Producers is how many goroutines offer the schedule concurrently.
	//
	// The schedule is partitioned round-robin, so the *set* of offer times is
	// independent of producer count. Achieving them is not: each producer sleeps
	// gap x Producers between its own items, so raising Producers raises the
	// per-producer gap and makes the target rate easier to hit.
	//
	// There is still a host ceiling. Below roughly tens of microseconds per
	// producer, sleeps overshoot and the aggregate offered rate falls short of the
	// schedule no matter how the work is divided. Result.Lateness reports the
	// overshoot and LatenessValid marks such a run invalid, which is the guard to
	// rely on rather than assuming any rate is reachable.
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

	// DisableGCDuringRun turns the collector off for the measured window and
	// restores it afterwards.
	//
	// A GC cycle landing mid-run inflates processor sleeps and tail latencies, so a
	// latency comparison is cleaner without one. It is opt-in rather than default
	// because disabling GC while deliberately overloading an unbounded queue is how
	// a scenario turns into an OOM: the overload scenarios exist to accumulate
	// gigabytes, and they need the collector running.
	//
	// Result.GCDuringRun reports whether a collection happened regardless, so a run
	// can be treated as suspect without disabling anything.
	DisableGCDuringRun bool

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
	BatchSizeDist    IntDistribution
	DownstreamPerSec float64

	// Resources
	AllocsPerItem float64

	// HeapHighWater is the peak HeapAlloc sampled while load was in flight.
	// HeapSampled reports whether the sampler actually fired: for a run shorter
	// than the sample interval this falls back to the post-drain reading, which
	// describes a recovered process rather than a peak. Treat HeapHighWater as
	// meaningless when HeapSampled is false.
	HeapHighWater uint64
	HeapSampled   bool

	// PendingWorkPeak is the peak of Batcher.Len(), sampled periodically.
	//
	// It is NOT queue depth. Len() counts accepted work that has not reached a
	// terminal outcome, which includes the batch currently accumulating and the
	// batch inside the processor. At sub-saturating rates it is therefore a
	// sawtooth roughly one window deep even with no backlog at all, and periodic
	// sampling misses the peaks of that sawtooth. It is a lower bound on
	// in-system work, useful for showing that work accumulates under saturation,
	// and unsuitable for reasoning about backlog at moderate load.
	//
	// Phase 2 adds Stats().Queued, which is true queue depth; prefer it once
	// available.
	PendingWorkPeak int64

	GCCount      uint32
	GCPauseTotal time.Duration

	// GCDuringRun reports whether a collection ran inside the measured window. A
	// GC cycle inflates processor sleeps and tail latencies, so a run with this
	// set should be treated as suspect for tail-sensitive comparisons.
	GCDuringRun bool

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

		procRNG = rand.New(rand.NewSource(cfg.Seed))
		start   time.Time
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

	// Collect once before measuring so the baseline is a settled heap rather than
	// whatever the previous scenario left behind.
	runtime.GC()

	if cfg.DisableGCDuringRun {
		previous := debug.SetGCPercent(-1)
		defer debug.SetGCPercent(previous)
	}

	runtime.ReadMemStats(&memBefore)

	// Sample heap and queue depth while load is in flight. Reading only after the
	// drain would report a recovered process and hide the peak backlog, which is
	// precisely the number overload scenarios need.
	var (
		heapPeak      atomic.Uint64
		pendingPeak   atomic.Int64
		goroutinePeak atomic.Int64
		samples       atomic.Int64
		samplerDone   = make(chan struct{})
		samplerStop   = make(chan struct{})
	)

	goroutinePeak.Store(int64(runtime.NumGoroutine()))

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
				samples.Add(1)

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

				// Sample goroutines here rather than after wg.Wait: producers have
				// exited by then, so the peak could never include them and every
				// concurrent scenario under-reported by cfg.Producers.
				if n := int64(runtime.NumGoroutine()); n > goroutinePeak.Load() {
					goroutinePeak.Store(n)
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
		memBefore, memAfter, int(goroutinePeak.Load()), heapPeak.Load(), pendingPeak.Load(),
		samples.Load())

	result.OfferedFor = offeredFor
	result.TimedOut = timedOut
	result.CloseErr = closeErr

	return result
}
