package batcher

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"
)

// ErrTimeout is returned when a wait for pending work to finish expires.
var ErrTimeout = errors.New("timeout waiting for batches to complete")

// ErrClosing is returned by Enqueue when admission has been sealed, and by a
// blocking enqueue that is released by shutdown. It means the item was not
// accepted and no work was queued for it.
var ErrClosing = errors.New("batcher is closing")

type Processor[T any] func([]T) error

func NoOpProcessor[T any]([]T) error {
	return nil
}

type Config[T any] struct {
	SkipAutoStart   bool
	BatchSize       int
	BatchInterval   time.Duration
	Concurrency     int
	MaxQueueSize    int
	CloseGrace      time.Duration
	ErrorBufferSize int
	ProcessorFunc   Processor[T]

	// UnorderedProcessingAcknowledged records that the caller accepted the
	// ordering trade required for Concurrency > 1. See WithoutOrderedProcessing.
	UnorderedProcessingAcknowledged bool
}

type Batcher[T any] struct {
	config *Config[T]

	// configFrozen becomes true before any processing goroutine starts. Options
	// are construction-time configuration: applying one to an existing batcher is
	// a no-op, so a caller cannot race the aggregator or processor by mutating
	// BatchSize, BatchInterval or ProcessorFunc after New returns.
	configFrozen atomic.Bool

	// runtime is the immutable snapshot the processing goroutines use. It is taken
	// in New, before any goroutine exists, so no worker ever reads the Config
	// struct a caller may still hold a reference to.
	runtime runtimeConfig[T]

	// constructed becomes true at the end of New. Start is inert before that.
	//
	// Option is an arbitrary func(*Batcher[T]), so a caller can pass one that calls
	// Start during New's option loop. Without this guard the aggregator would launch
	// while New was still assigning fields such as input, which is a data race
	// inside the library triggered by caller code.
	constructed atomic.Bool

	gate     *admissionGate
	counters counters

	// input is never closed, for the life of the batcher. A publisher can
	// therefore never send on a closed channel, which removes an entire class of
	// shutdown panic. Intake completion is signalled by noInput instead.
	input *queue[T]

	// errorsChan is a plain buffered channel closed exactly once, by the drain
	// coordinator, after every possible sender has exited.
	errorsChan chan error

	// noInput is closed by the coordinator once no publisher can ever send again.
	// It is what lets the aggregator conclude that intake is finished, since the
	// queue itself is never closed.
	noInput   chan struct{}
	stopped   chan struct{}
	startOnce sync.Once
	drainOnce sync.Once

	// shutdownDone closes only after the independent coordinator has finished
	// draining accepted work and closed diagnostics. A Close caller may time out,
	// but the coordinator never does: reporting an incomplete drain is not the
	// same as abandoning it.
	shutdownOnce sync.Once
	shutdownDone chan struct{}
}

// runtimeConfig is the frozen view of Config that processing goroutines read.
//
// Config is a plain struct and callers can hold a reference to one, so reading it
// from a worker is a data race by construction. Copying the fields the pipeline
// needs, once, before any goroutine starts, removes that whole class rather than
// relying on every read site to remember to use a local.
type runtimeConfig[T any] struct {
	batchSize     int
	batchInterval time.Duration
	workers       int
	processor     Processor[T]
}

// New creates a new Batcher with the given options.
func New[T any](options ...Option[T]) *Batcher[T] {
	b := &Batcher[T]{
		config: &Config[T]{
			BatchSize:       DefaultBatchSize,
			BatchInterval:   DefaultBatchInterval,
			Concurrency:     DefaultConcurrency,
			CloseGrace:      DefaultCloseGrace,
			ErrorBufferSize: DefaultErrorBufferSize,
			ProcessorFunc:   NoOpProcessor[T],
		},

		gate:         newAdmissionGate(),
		noInput:      make(chan struct{}),
		stopped:      make(chan struct{}),
		shutdownDone: make(chan struct{}),
	}

	for _, option := range options {
		option(b)
	}

	b.validateConfig()

	// No option may change configuration after this point, including a batcher
	// constructed with WithSkipAutoStart. SkipAutoStart controls lifecycle, not
	// whether the configuration is still mutable.
	b.configFrozen.Store(true)

	b.runtime = runtimeConfig[T]{
		batchSize:     b.config.BatchSize,
		batchInterval: b.config.BatchInterval,
		workers:       b.config.Concurrency,
		processor:     b.config.ProcessorFunc,
	}

	if b.runtime.batchSize < 1 {
		b.runtime.batchSize = DefaultBatchSize
	}

	if b.runtime.workers < 1 {
		b.runtime.workers = 1
	}

	if b.runtime.processor == nil {
		b.runtime.processor = NoOpProcessor[T]
	}

	b.input = newQueue[T](b.config.MaxQueueSize)
	b.errorsChan = make(chan error, b.config.ErrorBufferSize)

	// Everything the pipeline touches is now assigned, so Start may launch it. A
	// Start attempted earlier -- from a hostile or buggy Option -- was a no-op, and
	// this is where that stops being true.
	b.constructed.Store(true)

	if !b.config.SkipAutoStart {
		b.Start()
	}

	return b
}

// validateConfig rejects configurations whose guarantees contradict each other.
//
// This panics rather than returning an error because New has never been able to
// fail, and adding an error return would break every existing call site for a
// mistake that is always a programming error: the combination is fixed at
// construction, so it is caught by the first test run rather than in production.
func (b *Batcher[T]) validateConfig() {
	if b.config.Concurrency > 1 && !b.config.UnorderedProcessingAcknowledged {
		panic(fmt.Sprintf(
			"batcher: WithConcurrency(%d) requires WithoutOrderedProcessing(). "+
				"Concurrent processing gives up cross-batch ordering and lets the "+
				"processor be invoked concurrently, so the processor must be "+
				"goroutine-safe. Add WithoutOrderedProcessing() to acknowledge this, "+
				"or keep WithConcurrency(1).",
			b.config.Concurrency,
		))
	}
}

// Start begins processing. It is idempotent: repeated or concurrent calls start
// exactly one processing lifecycle.
//
// Start always launches the consumer, even if shutdown has already sealed
// admission. That is deliberate: a sealed batcher may still hold queued work, and
// the drain needs a consumer to make progress. Declining to start here would leave
// the drain waiting on a consumer that never exists, which deadlocks shutdown when
// Start and Shutdown race.
func (b *Batcher[T]) Start() {
	// Inert until New has finished wiring the batcher. Option is an arbitrary
	// closure, so a caller can invoke Start from inside the option loop; launching
	// the aggregator then would read fields New has not assigned yet. Ignoring the
	// call is safe: New starts the batcher itself unless WithSkipAutoStart was
	// given, and a caller that meant to start it can call Start after New returns.
	if !b.constructed.Load() {
		return
	}

	b.startOnce.Do(func() {
		b.gate.state.CompareAndSwap(stateNew, stateRunning)

		go b.run()
	})
}

// Config returns a copy of the batcher's configuration.
//
// It is a snapshot, not a handle. Returning the live pointer let a caller mutate a
// running batcher's batch size, interval or processor from another goroutine, which
// is a data race against the aggregation loop and could change batching semantics
// mid-batch. Options are meant to be applied at construction; mutating them
// afterwards was never coherent, so this closes the hole rather than documenting it.
//
// Callers that need to change configuration should construct a new Batcher.
func (b *Batcher[T]) Config() Config[T] {
	return *b.config
}

// Add enqueues an item. It is the compatibility fast path and returns no error.
//
// In unbounded mode it never blocks. In bounded mode it blocks while the queue is
// full, exactly as it did before bounding existed, and shutdown releases it. After
// admission is sealed it is a no-op that counts a rejection: it neither panics nor
// silently pretends to have accepted the item.
//
// Callers that need to observe rejection or bound their wait should use Enqueue.
func (b *Batcher[T]) Add(item T) {
	_ = b.publish(context.Background(), item, true)
}

// Enqueue enqueues an item, reporting why it could not be accepted.
//
// It returns ErrClosing when admission is sealed, ctx.Err() when the caller's
// context expires while waiting for space in a bounded queue, or nil on success.
// A non-nil error means nothing was queued.
func (b *Batcher[T]) Enqueue(ctx context.Context, item T) error {
	return b.publish(ctx, item, false)
}

// publish is the single admission path shared by Add and Enqueue.
//
// The ordering here is the protocol, and each step exists for a reason:
//
//  1. enter the gate, so shutdown cannot conclude that intake is finished while
//     this publisher is still deciding;
//  2. reserve the drain obligation BEFORE publishing, so the drain cannot
//     terminate in the window between reserving and publishing;
//  3. publish;
//  4. on failure, roll the reservation back BEFORE leaving the gate, so no
//     phantom obligation can outlive the gate;
//  5. leave the gate.
func (b *Batcher[T]) publish(ctx context.Context, item T, nonBlockingWhenFull bool) error {
	if !b.gate.enter() {
		b.counters.rejected.Add(1)

		return ErrClosing
	}

	defer b.gate.leave()

	b.counters.reserve()

	var err error

	if nonBlockingWhenFull && b.config.MaxQueueSize <= 0 {
		// Unbounded fast path: publication cannot fail.
		_ = b.input.tryPush(item)
	} else {
		err = b.input.push(ctx, item, b.gate.sealCh)
	}

	if err != nil {
		b.counters.rollback()

		return err
	}

	b.counters.accept()

	return nil
}

// Len reports accepted-or-reserved work that has not reached a terminal outcome.
//
// Note this includes work currently inside a processor call, so it can be non-zero
// while the queue is empty. Use Stats().Queued for queue depth.
func (b *Batcher[T]) Len() int {
	return int(b.counters.pending.Load())
}

// Stats returns an O(1), allocation-free snapshot of the batcher's counters. See
// Stats for its consistency guarantees.
func (b *Batcher[T]) Stats() Stats {
	return Stats{
		Pending:          b.counters.pending.Load(),
		IntakePending:    b.counters.intakePending.Load(),
		PublishersInGate: b.gate.inGate(),
		Queued:           int64(b.input.length()),
		BatchHeld:        b.counters.batchHeld.Load(),
		InFlight:         b.counters.inFlight.Load(),
		Accepted:         b.counters.accepted.Load(),
		Completed:        b.counters.completed.Load(),
		Failed:           b.counters.failed.Load(),
		Panicked:         b.counters.panicked.Load(),
		BatchesFlushed:   b.counters.batchesFlushed.Load(),
		Rejected:         b.counters.rejected.Load(),
		DroppedErrors:    b.counters.droppedErrors.Load(),
	}
}

// Join waits until no work is pending, or the timeout expires.
//
// This is a quiescence snapshot, not a barrier: a concurrent Add can make work
// pending again the instant it returns. It is only authoritative after admission
// has been sealed.
func (b *Batcher[T]) Join(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)

	for {
		if b.Len() == 0 {
			return nil
		}

		if time.Now().After(deadline) {
			return ErrTimeout
		}

		time.Sleep(1 * time.Millisecond)
	}
}

// Errors returns the diagnostics channel. It is closed once shutdown completes,
// so a consumer ranging over it terminates.
func (b *Batcher[T]) Errors() <-chan error {
	return b.errorsChan
}

// run aggregates items into batches and hands each one to the processing loop.
//
// Batching rules, unchanged from the previous implementation and pinned by the
// characterization tests:
//
//   - The interval timer is armed when the first item enters an empty batch, not
//     on a periodic tick. A sparse producer waits one interval per item, and an
//     idle batcher does no work at all.
//   - A batch reaching BatchSize flushes immediately.
//   - Empty batches are never emitted.
//   - Shutdown flushes the pending partial batch rather than discarding it.
//
// Aggregation runs here and processing runs in Concurrency separate goroutines,
// connected by an UNBUFFERED channel. Two properties follow, and both are
// deliberate:
//
//   - The separation lets the next batch accumulate while the current one is being
//     processed. Merging them measurably changes latency: with a 50ms processor at
//     10k items/s a merged loop made a 5ms window faster than a 100ms one,
//     inverting the documented baseline.
//   - The channel is unbuffered, so back-pressure stays at admission. A buffered
//     dispatch queue would silently void the MaxQueueSize contract by holding
//     batches nobody counted, so accepted-but-unfinished work stays bounded by
//     MaxQueueSize + (1 + Concurrency) × BatchSize + publishers in the gate. The
//     leading batch is the one the aggregator holds after it leaves the queue; the
//     Concurrency term is the batches inside processors.
//
// At Concurrency 1 there is exactly one worker, so the processor is never invoked
// concurrently and batches are processed in publication order. Above 1 the caller
// has acknowledged, via WithoutOrderedProcessing, that cross-batch ordering and
// processor mutual exclusion are given up.
func (b *Batcher[T]) run() {
	defer close(b.stopped)

	// Use the immutable runtime snapshot taken by New before any goroutine starts.
	// In particular, ProcessorFunc is passed to workers rather than dereferenced
	// from Config per batch. This keeps every processing read off the mutable Config
	// struct, so a caller with a stale reference cannot race the pipeline.
	var (
		batchSize     = b.runtime.batchSize
		batchInterval = b.runtime.batchInterval
		workers       = b.runtime.workers
		processor     = b.runtime.processor
	)

	batches := make(chan []T)

	var processing sync.WaitGroup

	processing.Add(workers)

	for range workers {
		go func() {
			defer processing.Done()

			for items := range batches {
				b.process(processor, items)
			}
		}()
	}

	// Closing the dispatch channel stops the workers, and waiting for them is what
	// makes the drain complete: shutdown must not report success while a worker is
	// still inside the processor.
	defer func() {
		close(batches)
		processing.Wait()
	}()

	var (
		batch      []T
		timer      *time.Timer
		timerC     <-chan time.Time
		capacities = newCapacityEstimator(batchSize)
	)

	stopTimer := func() {
		timerC = nil

		if timer == nil {
			return
		}

		if !timer.Stop() {
			// Drain a tick that fired while we were flushing, so the next batch
			// cannot inherit a stale expiry and flush early.
			select {
			case <-timer.C:
			default:
			}
		}
	}

	flush := func() {
		if len(batch) == 0 {
			return
		}

		items := batch
		batch = nil

		stopTimer()

		// The send blocks until a worker is free. Until it returns, the batch is
		// still aggregator-held, which is the state BatchHeld exists to expose:
		// releasing the counter before the send would hide a batch waiting on
		// saturated workers.
		batches <- items

		b.counters.dispatched(len(items))
		capacities.observe(len(items))
	}

	take := func(item T) {
		if len(batch) == 0 {
			// The capacity estimator is owned by this aggregation goroutine. It
			// reduces timer-flush allocation pressure without pooling or reusing a
			// slice after the processor receives it, so callers may still retain the
			// batch slice exactly as before.
			batch = make([]T, 0, capacities.capacity())

			if timer == nil {
				timer = time.NewTimer(batchInterval)
			} else {
				timer.Reset(batchInterval)
			}

			timerC = timer.C
		}

		batch = append(batch, item)

		if len(batch) >= batchSize {
			flush()
		}
	}

	// drainReady empties whatever is currently queued. The aggregator drains
	// greedily rather than one item per wakeup, so a burst costs one signal.
	//
	// It transfers a run of items under a single queue lock rather than locking per
	// item. The aggregator is the only consumer and keeps no invariant between items,
	// so a block transfer is indistinguishable from N pops to it, while per-item
	// locking was what serialised producers: profiling attributed 86% of mutex delay
	// and 61% of CPU to the push path contending with this loop.
	//
	// Each transfer takes only what the batch being built can still accept. That
	// bounds how long the queue lock is held, and it keeps the capacity contract
	// exact: transferred items move straight into batch, so no additional
	// batch-sized buffer of accepted work exists outside the queue. Draining a full
	// batchSize regardless of the batch's remaining space added exactly that third
	// buffer and pushed accepted work past N + 2*BatchSize + gate.
	//
	// drained is reused across wakeups to keep steady-state draining allocation-free.
	// It is deliberately separate from batch: batch is handed to a worker and may be
	// retained by the processor, whereas drained never leaves this goroutine.
	drained := make([]T, 0, batchSize)

	drainReady := func() {
		for {
			room := batchSize - len(batch)
			if room <= 0 {
				// take flushes at batchSize, so this only happens if a flush could not
				// complete. Fall back to one item and let take drive the flush.
				room = 1
			}

			drained = b.input.popBatch(drained[:0], room)
			if len(drained) == 0 {
				return
			}

			for _, item := range drained {
				b.counters.received(1)
				take(item)
			}
		}
	}

	for {
		select {
		case <-b.input.ready():
			drainReady()
		case <-timerC:
			flush()
		case <-b.noInput:
			// Intake is finished: no publisher can ever send again. Drain by
			// accounting rather than by observing an empty queue, so an item that was
			// reserved-then-published during shutdown cannot be missed.
			for b.counters.intakePending.Load() > 0 {
				drainReady()
			}

			flush()

			return
		}
	}
}

// process invokes the processor for one batch and records exactly one terminal
// outcome for it.
//
// A panic in the processor is recovered and scoped to the batch. Batcher owns
// goroutines the caller cannot see, so it must not let a caller's bug become a
// process-wide crash that also destroys every other queued item: the caller has no
// way to wrap our goroutine in its own recover. The panic value and stack are
// reported as a diagnostic so the bug stays debuggable.
//
// Recovery is scoped to the batch rather than the loop, so one poison batch cannot
// silently take the consumer down and stall every later batch.
//
// The ordering is fixed and load-bearing: whatever happens, exactly one terminal
// category is counted and the drain obligation is released exactly once. Leaving it
// unreleased would inflate Pending permanently and hang shutdown forever.
func (b *Batcher[T]) process(processor Processor[T], items []T) {
	b.counters.inFlight.Add(int64(len(items)))

	outcome := outcomeCompleted

	defer func() {
		b.counters.terminal(outcome, len(items))
	}()

	defer b.counters.inFlight.Add(int64(-len(items)))

	defer func() {
		recovered := recover()
		if recovered == nil {
			return
		}

		outcome = outcomePanicked

		b.publishError(&ProcessorPanicError{
			Value: recovered,
			Stack: debug.Stack(),
		})
	}()

	if err := processor(items); err != nil {
		outcome = outcomeFailed

		b.publishError(err)
	}
}

// publishError reports a diagnostic without ever blocking the pipeline.
//
// Dropping is deliberate, and the alternative is worse. A bounded channel with a
// blocking send would deadlock the pipeline whenever a processor fails and nobody
// is reading Errors() — a self-inflicted outage caused by a diagnostic. An
// unbounded channel would instead grow without limit during an outage, which is
// exactly when memory is scarcest.
//
// Dropping the newest error rather than the oldest keeps the earliest errors of a
// storm, which usually carry the root cause, and avoids racing the consumer.
// DroppedErrors makes the loss visible: a non-zero value means "you are not
// draining Errors() fast enough", which is itself worth alerting on.
func (b *Batcher[T]) publishError(err error) {
	select {
	case b.errorsChan <- err:
	default:
		b.counters.droppedErrors.Add(1)
	}
}

// Close seals admission and drains accepted work, waiting up to the configured
// grace period (WithCloseGrace, default 30s).
//
// It reports an incomplete drain if the grace expires, but it does NOT abandon the
// drain: accepted work keeps being processed in the background. Reporting an
// incomplete drain and causing one are very different things, and the previous
// implementation silently discarded accepted items.
//
// Callers who want to control the wait, or to keep waiting after a timeout, should
// use Shutdown.
func (b *Batcher[T]) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), b.config.CloseGrace)
	defer cancel()

	return b.Shutdown(ctx)
}

// Shutdown seals admission and waits for accepted work to drain.
//
// It is resumable. If ctx expires first it returns *ShutdownIncompleteError and the
// drain continues; calling Shutdown again with a fresh context waits on that same
// drain rather than starting a new one. That is why there is no continuation
// token: the Batcher itself is the continuation, and it is sealed against new work
// from the first call onward.
//
// Concurrent callers are independent. A caller with a short deadline receives its
// own timeout error; it is never stored as the batcher's terminal result, so it
// cannot poison a later caller who waits longer.
//
// Processor errors and recovered panics are NOT returned here. They remain on
// Errors(), because a shutdown result describes the drain, not the work.
//
// A processor that never returns holds the batcher in draining indefinitely. This
// library cannot cancel user code, so Shutdown reports the condition rather than
// pretending to resolve it.
func (b *Batcher[T]) Shutdown(ctx context.Context) error {
	b.beginSealing()

	// An unstarted batcher may still hold queued work. Start the consumer now, or
	// the drain could never make progress. This is the one case where shutdown
	// starts a lifecycle, and it must happen before any wait: for a bounded queue
	// with a parked publisher, nothing else can unblock the gate.
	b.startDrainConsumer()

	// The coordinator runs independently of any caller's patience.
	b.shutdownOnce.Do(func() {
		go b.coordinateDrain()
	})

	// A completed drain wins over an expired context. A select chooses randomly
	// when both cases are ready; reporting ShutdownIncompleteError with Pending=0
	// for a batcher that is already closed would be false.
	select {
	case <-b.shutdownDone:
		return nil
	default:
	}

	select {
	case <-b.shutdownDone:
		return nil
	case <-ctx.Done():
		return &ShutdownIncompleteError{
			Pending:          b.Len(),
			PublishersInGate: int(b.gate.inGate()),
			Cause:            ctx.Err(),
		}
	}
}

// IsClosing reports whether admission has been sealed. The drain may still be in
// progress.
func (b *Batcher[T]) IsClosing() bool {
	return b.gate.state.Load() >= stateSealing
}

// IsClosed reports whether shutdown has fully completed and every owned goroutine
// has exited.
func (b *Batcher[T]) IsClosed() bool {
	return b.gate.state.Load() == stateClosed
}

// coordinateDrain performs the drain exactly once, however many callers are
// waiting and however soon they give up.
func (b *Batcher[T]) coordinateDrain() {
	defer close(b.shutdownDone)

	// Wait for publishers to leave the publication window. The aggregator keeps
	// draining throughout, which is what lets a publisher parked on a full bounded
	// queue make progress and exit the gate.
	<-b.gate.empty()

	b.gate.state.CompareAndSwap(stateSealing, stateDraining)

	// Only now can the aggregator conclude that intake is finished: no publisher
	// can ever send again.
	b.drainOnce.Do(func() {
		close(b.noInput)
	})

	<-b.stopped

	b.finish()
}

// beginSealing closes admission and performs the coordinator's own quiescence
// check.
func (b *Batcher[T]) beginSealing() {
	for {
		current := b.gate.state.Load()
		if current >= stateSealing {
			break
		}

		if b.gate.state.CompareAndSwap(current, stateSealing) {
			break
		}
	}

	b.gate.seal()
	b.gate.checkEmpty()
}

// startDrainConsumer ensures a consumer exists to drain queued work during
// shutdown, even if the batcher was never started.
//
// It shares startOnce with Start, so exactly one consumer ever runs regardless of
// which path gets there first.
func (b *Batcher[T]) startDrainConsumer() {
	b.Start()
}

// finish performs terminal cleanup exactly once: close diagnostics after every
// sender has exited, then mark the batcher closed.
func (b *Batcher[T]) finish() {
	if b.gate.state.CompareAndSwap(stateDraining, stateClosed) {
		close(b.errorsChan)

		return
	}

	// Another caller may already be finishing; make sure the terminal state is
	// visible before returning.
	b.gate.state.CompareAndSwap(stateSealing, stateClosed)
}
