package batcher_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NSXBet/batcher/pkg/batcher"
	"github.com/stretchr/testify/require"
)

// Shutdown semantics tests.
//
// The contract these pin is "drain or report, never discard". Before this
// milestone, a shutdown whose timeout expired closed the pipeline and threw away
// accepted work, which is the worst possible outcome: the caller was told
// shutdown finished and the items were gone.

// TestShutdownIsResumable is the core of the contract. A caller that gives up
// early must not affect the drain, and a later caller must be able to keep waiting
// on the same drain rather than starting a new one.
func TestShutdownIsResumable(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})

	var processed atomic.Int64

	b := batcher.New(
		batcher.WithBatchSize[int](1),
		batcher.WithBatchInterval[int](time.Millisecond),
		batcher.WithProcessor(func(items []int) error {
			<-release
			processed.Add(int64(len(items)))

			return nil
		}),
	)

	b.Add(1)

	require.Eventually(t, func() bool {
		return b.Stats().Pending > 0
	}, 5*time.Second, time.Millisecond)

	// First attempt: give up almost immediately.
	shortCtx, cancelShort := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancelShort()

	err := b.Shutdown(shortCtx)

	var incomplete *batcher.ShutdownIncompleteError

	require.ErrorAs(t, err, &incomplete,
		"an expired wait must report an incomplete drain")
	require.Positive(t, incomplete.Pending,
		"the report must say how much work remained")
	require.ErrorIs(t, err, context.DeadlineExceeded,
		"the cause must be visible to errors.Is")
	require.ErrorIs(t, err, batcher.ErrTimeout,
		"callers written against Close's original error must keep working")

	// The drain was not abandoned: the batcher is sealed but not closed.
	require.True(t, b.IsClosing(), "admission must be sealed after the first attempt")
	require.False(t, b.IsClosed(), "the drain must still be in progress")

	// Let the processor finish, then resume waiting with a fresh context.
	close(release)

	require.NoError(t, b.Shutdown(context.Background()),
		"a later Shutdown must wait on the same drain and observe completion")

	require.True(t, b.IsClosed(), "the batcher must be terminal once the drain completes")
	require.Equal(t, int64(1), processed.Load(),
		"the accepted item must be processed, not discarded by the first timeout")
}

// TestShutdownDeadlineDoesNotPoisonLaterCallers pins that a per-caller timeout is
// per-caller. A short deadline must not be stored as the batcher's terminal
// result, or one impatient caller would corrupt every other caller's answer.
func TestShutdownDeadlineDoesNotPoisonLaterCallers(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})

	b := batcher.New(
		batcher.WithBatchSize[int](1),
		batcher.WithBatchInterval[int](time.Millisecond),
		batcher.WithProcessor(func([]int) error {
			<-release

			return nil
		}),
	)

	b.Add(1)

	require.Eventually(t, func() bool {
		return b.Stats().Pending > 0
	}, 5*time.Second, time.Millisecond)

	shortCtx, cancelShort := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancelShort()

	longDone := make(chan error, 1)

	go func() {
		longDone <- b.Shutdown(context.Background())
	}()

	shortErr := b.Shutdown(shortCtx)
	require.Error(t, shortErr, "the impatient caller must get its own timeout")

	close(release)

	select {
	case err := <-longDone:
		require.NoError(t, err,
			"the patient caller must observe a completed drain, not the other caller's timeout")
	case <-time.After(10 * time.Second):
		t.Fatal("the patient caller never observed drain completion")
	}

	// A shutdown after terminal completion returns the stored terminal result.
	require.NoError(t, b.Shutdown(context.Background()),
		"shutdown after completion must return the terminal result")
}

// TestShutdownBeforeStartDrainsQueuedWork pins that work accepted by a batcher
// that was never started is still drained.
//
// Without this, a caller using WithSkipAutoStart who enqueued and then shut down
// would silently lose everything, because nothing was ever consuming.
func TestShutdownBeforeStartDrainsQueuedWork(t *testing.T) {
	t.Parallel()

	var processed atomic.Int64

	b := batcher.New(
		batcher.WithBatchSize[int](10_000), // only shutdown can flush this
		batcher.WithBatchInterval[int](time.Hour),
		batcher.WithSkipAutoStart[int](),
		batcher.WithProcessor(func(items []int) error {
			processed.Add(int64(len(items)))

			return nil
		}),
	)

	for i := range 5 {
		b.Add(i)
	}

	require.NoError(t, b.Shutdown(context.Background()))
	require.Equal(t, int64(5), processed.Load(),
		"work queued before Start must still be drained")
}

// TestShutdownOnEmptyUnstartedBatcherCompletes pins the trivial case: an unused
// batcher must shut down promptly rather than waiting for a consumer that never
// had work.
func TestShutdownOnEmptyUnstartedBatcherCompletes(t *testing.T) {
	t.Parallel()

	b := batcher.New(
		batcher.WithSkipAutoStart[int](),
		batcher.WithProcessor(batcher.NoOpProcessor[int]),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	require.NoError(t, b.Shutdown(ctx))
	require.True(t, b.IsClosed())
}

// TestShutdownWithParkedPublisherOnFullBoundedQueue pins the specific ordering
// hazard the protocol exists to prevent.
//
// A publisher parked on a full bounded queue cannot leave the admission gate, and
// shutdown waits for the gate to empty. If shutdown waited before ensuring a
// consumer was draining, or failed to release parked publishers, the two would
// deadlock permanently. This is run repeatedly because it is an ordering race.
func TestShutdownWithParkedPublisherOnFullBoundedQueue(t *testing.T) {
	t.Parallel()

	const trials = 300

	for trial := range trials {
		release := make(chan struct{})

		b := batcher.New(
			batcher.WithBatchSize[int](1),
			batcher.WithBatchInterval[int](time.Hour),
			batcher.WithMaxQueueSize[int](2),
			batcher.WithSkipAutoStart[int](),
			batcher.WithProcessor(func([]int) error {
				<-release

				return nil
			}),
		)

		// Fill the bounded queue while nothing is consuming.
		for range 2 {
			require.NoError(t, b.Enqueue(context.Background(), 1))
		}

		// This publisher must park: the queue is full and unstarted.
		parked := make(chan error, 1)

		go func() { parked <- b.Enqueue(context.Background(), 99) }()

		shutdownDone := make(chan error, 1)

		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			shutdownDone <- b.Shutdown(ctx)
		}()

		// The publisher must be released rather than held forever. It may return
		// ErrClosing, or it may win the final race with the newly available slot:
		// it entered the admission gate before sealing, and once the shutdown
		// consumer drains a slot, both sealCh and notFull are ready. The protocol
		// permits that pre-seal publisher to succeed, provided the drain accounts
		// for it. Enqueue calls that START after sealing are covered separately by
		// TestEnqueueAfterSealReportsClosing and must return ErrClosing.
		select {
		case err := <-parked:
			require.True(t, err == nil || errors.Is(err, batcher.ErrClosing),
				"trial %d: a pre-seal parked publisher may succeed or be released "+
					"with ErrClosing, got %v", trial, err)
		case <-time.After(10 * time.Second):
			t.Fatalf("trial %d: parked publisher was never released", trial)
		}

		close(release)

		select {
		case err := <-shutdownDone:
			require.NoError(t, err,
				"trial %d: the drain must complete, not merely return", trial)
		case <-time.After(10 * time.Second):
			t.Fatalf("trial %d: shutdown deadlocked with a parked publisher", trial)
		}
	}
}

// TestPartialBatchWithLongIntervalIsNotDiscarded pins the data-loss bug directly.
//
// With an interval far longer than the shutdown grace, the old implementation
// reported a timeout and threw the batch away: 50 items accepted, 0 processed, and
// Len still claiming 50. Now the batch must be processed, or the shortfall must be
// reported; never silently dropped.
func TestPartialBatchWithLongIntervalIsNotDiscarded(t *testing.T) {
	t.Parallel()

	var processed atomic.Int64

	b := batcher.New(
		batcher.WithBatchSize[int](100),
		batcher.WithBatchInterval[int](30*time.Second), // far beyond the grace period
		batcher.WithProcessor(func(items []int) error {
			processed.Add(int64(len(items)))

			return nil
		}),
	)

	const accepted = 50

	for i := range accepted {
		b.Add(i)
	}

	require.NoError(t, b.Close(),
		"shutdown must flush the partial batch rather than waiting for the interval")
	require.Equal(t, int64(accepted), processed.Load(),
		"every accepted item must be processed")
	require.Zero(t, b.Len(), "no phantom pending count may remain")
}

// TestStartRacingShutdownYieldsOneLifecycle pins that the two cannot combine into
// two consumers or a double close.
func TestStartRacingShutdownYieldsOneLifecycle(t *testing.T) {
	t.Parallel()

	const trials = 200

	for trial := range trials {
		var processed atomic.Int64

		b := batcher.New(
			batcher.WithBatchSize[int](1),
			batcher.WithBatchInterval[int](time.Millisecond),
			batcher.WithSkipAutoStart[int](),
			batcher.WithProcessor(func(items []int) error {
				processed.Add(int64(len(items)))

				return nil
			}),
		)

		b.Add(1)

		var wg sync.WaitGroup

		wg.Add(2)

		go func() {
			defer wg.Done()

			b.Start()
		}()

		go func() {
			defer wg.Done()

			_ = b.Shutdown(context.Background())
		}()

		wg.Wait()

		require.NoError(t, b.Shutdown(context.Background()), "trial %d", trial)
		require.Equal(t, int64(1), processed.Load(),
			"trial %d: the item must be processed exactly once", trial)
	}
}

// TestShutdownResultIgnoresProcessorErrors pins the separation between "the drain
// finished" and "the work succeeded". Conflating them would make a failing
// downstream look like a broken shutdown.
func TestShutdownResultIgnoresProcessorErrors(t *testing.T) {
	t.Parallel()

	failure := errors.New("downstream unavailable")

	b := batcher.New(
		batcher.WithBatchSize[int](1),
		batcher.WithBatchInterval[int](time.Millisecond),
		batcher.WithProcessor(func([]int) error {
			return failure
		}),
	)

	// Drain diagnostics so the buffer cannot fill.
	go func() {
		for range b.Errors() {
		}
	}()

	b.Add(1)

	require.NoError(t, b.Shutdown(context.Background()),
		"a processor error must not be reported as a shutdown failure")

	stats := b.Stats()

	require.Equal(t, uint64(1), stats.Failed)
	require.Zero(t, stats.Pending)
}

// TestNonReturningProcessorKeepsBatcherDraining documents the one case the library
// cannot resolve: user code that never returns. The honest behaviour is to report
// it, not to pretend the drain finished.
func TestNonReturningProcessorKeepsBatcherDraining(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	defer close(release)

	b := batcher.New(
		batcher.WithBatchSize[int](1),
		batcher.WithBatchInterval[int](time.Millisecond),
		batcher.WithProcessor(func([]int) error {
			<-release

			return nil
		}),
	)

	b.Add(1)

	require.Eventually(t, func() bool {
		return b.Stats().Pending > 0
	}, 5*time.Second, time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	err := b.Shutdown(ctx)

	require.Error(t, err, "a blocked processor must be reported, not waited on forever")
	require.True(t, b.IsClosing(), "admission must be sealed")
	require.False(t, b.IsClosed(),
		"a batcher whose processor never returns is draining, not closed")
}

// TestShutdownIncompleteErrorFormatting pins the public error contract: callers
// inspect it with errors.Is/As, but logs and lifecycle tools also surface Error().
func TestShutdownIncompleteErrorFormatting(t *testing.T) {
	t.Parallel()

	err := &batcher.ShutdownIncompleteError{
		Pending:          7,
		PublishersInGate: 2,
		Cause:            context.DeadlineExceeded,
	}

	require.Equal(t,
		"batcher shutdown incomplete: 7 pending, 2 publishers still admitting: context deadline exceeded",
		err.Error())
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.ErrorIs(t, err, batcher.ErrTimeout)
}
