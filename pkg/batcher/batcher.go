package batcher

import (
	"context"
	"errors"
	"runtime/debug"
	"sync"
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
}

type Batcher[T any] struct {
	config *Config[T]

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

	b.input = newQueue[T](b.config.MaxQueueSize)
	b.errorsChan = make(chan error, b.config.ErrorBufferSize)

	if !b.config.SkipAutoStart {
		b.Start()
	}

	return b
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
	b.startOnce.Do(func() {
		b.gate.state.CompareAndSwap(stateNew, stateRunning)

		go b.run()
	})
}

// Config returns the batcher's configuration.
func (b *Batcher[T]) Config() *Config[T] {
	return b.config
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
		Accepted:         b.counters.accepted.Load(),
		Completed:        b.counters.completed.Load(),
		Failed:           b.counters.failed.Load(),
		Panicked:         b.counters.panicked.Load(),
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
// Aggregation runs here and processing runs in its own goroutine, connected by an
// unbuffered channel. That separation is load-bearing: it lets the next batch
// accumulate while the current one is being processed. Merging the two measurably
// changes latency — with a 50ms processor at 10k items/s a merged loop made a 5ms
// window faster than a 100ms one, inverting the documented baseline — and that is
// a Phase 3 decision, not this milestone's.
func (b *Batcher[T]) run() {
	defer close(b.stopped)

	batches := make(chan []T)

	processing := make(chan struct{})

	go func() {
		defer close(processing)

		for items := range batches {
			b.process(items)
		}
	}()

	defer func() {
		close(batches)
		<-processing
	}()

	var (
		batch  []T
		timer  *time.Timer
		timerC <-chan time.Time
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

		batches <- items
	}

	take := func(item T) {
		if len(batch) == 0 {
			batch = make([]T, 0, b.config.BatchSize)

			if timer == nil {
				timer = time.NewTimer(b.config.BatchInterval)
			} else {
				timer.Reset(b.config.BatchInterval)
			}

			timerC = timer.C
		}

		batch = append(batch, item)

		if len(batch) >= b.config.BatchSize {
			flush()
		}
	}

	// drainReady empties whatever is currently queued. The aggregator drains
	// greedily rather than one item per wakeup, so a burst costs one signal.
	drainReady := func() {
		for {
			item, ok := b.input.pop()
			if !ok {
				return
			}

			b.counters.received(1)
			take(item)
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
func (b *Batcher[T]) process(items []T) {
	outcome := outcomeCompleted

	defer func() {
		b.counters.terminal(outcome, len(items))
	}()

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

	if err := b.config.ProcessorFunc(items); err != nil {
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
