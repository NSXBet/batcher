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

// Admission and drain protocol tests.
//
// These cover the interleavings that make shutdown safe. Each one exists because
// a specific plausible implementation is wrong in a way that ordinary tests do not
// catch: a lost wakeup hangs forever, a missed reservation drops accepted work, and
// a closed input channel panics in the caller's goroutine.

// TestSealWhileProducersRaceNeverPanics is the blunt instrument: hammer admission
// while sealing, many times, and require that nothing panics and nothing is lost.
//
// The input queue is never closed precisely so this cannot panic. A design that
// closed it would fail here with "send on closed channel" in a caller's goroutine,
// which is the worst place for a library to fail.
func TestSealWhileProducersRaceNeverPanics(t *testing.T) {
	t.Parallel()

	const (
		trials    = 400
		producers = 8
		perActor  = 25
	)

	for trial := range trials {
		var processed atomic.Int64

		b := batcher.New(
			batcher.WithBatchSize[int](16),
			batcher.WithBatchInterval[int](time.Millisecond),
			batcher.WithProcessor(func(items []int) error {
				processed.Add(int64(len(items)))

				return nil
			}),
		)

		var (
			wg    sync.WaitGroup
			start = make(chan struct{})
		)

		for range producers {
			wg.Add(1)

			go func() {
				defer wg.Done()

				<-start

				for range perActor {
					b.Add(1)
				}
			}()
		}

		closed := make(chan error, 1)

		go func() {
			<-start
			closed <- b.Close()
		}()

		close(start)
		wg.Wait()

		require.NoError(t, <-closed, "trial %d: shutdown must complete", trial)

		stats := b.Stats()

		require.GreaterOrEqual(t, stats.Pending, int64(0),
			"trial %d: pending must never go negative", trial)
		require.Zero(t, stats.Pending,
			"trial %d: pending must converge to zero after drain", trial)
		require.Zero(t, stats.IntakePending,
			"trial %d: intake must be exhausted after drain", trial)
		require.Zero(t, stats.PublishersInGate,
			"trial %d: no publisher may remain in the gate", trial)

		// Every accepted item reached a terminal outcome, and nothing was invented.
		require.Equal(t, stats.Accepted, stats.Completed+stats.Failed+stats.Panicked,
			"trial %d: accepted must equal terminal outcomes at quiescence", trial)
		require.Equal(t, int64(stats.Accepted), processed.Load(),
			"trial %d: every accepted item must be processed", trial)
	}
}

// TestLastPublisherLeavesBeforeSealCompletesShutdown covers the lost-wakeup
// interleaving where the final publisher exits the gate *before* sealing begins.
//
// leave() only signals quiescence when it observes the seal, so if nothing else
// checked, no signal would ever arrive and Close would hang forever. The
// coordinator's own post-seal gate check is what prevents that, and this test is
// what proves the check is load-bearing: remove it and this hangs.
func TestLastPublisherLeavesBeforeSealCompletesShutdown(t *testing.T) {
	t.Parallel()

	const trials = 300

	for trial := range trials {
		b := batcher.New(
			batcher.WithBatchSize[int](4),
			batcher.WithBatchInterval[int](time.Millisecond),
			batcher.WithProcessor(batcher.NoOpProcessor[int]),
		)

		// Publish and return, so the gate is empty well before sealing starts.
		b.Add(1)

		done := make(chan error, 1)

		go func() { done <- b.Close() }()

		select {
		case err := <-done:
			require.NoError(t, err, "trial %d", trial)
		case <-time.After(10 * time.Second):
			t.Fatalf("trial %d: Close hung; the coordinator's post-seal gate check is missing", trial)
		}
	}
}

// TestShutdownWithQueuedWorkProcessesEverything is the final-intake completeness
// case: seal immediately after publishing, repeatedly, and require the item is
// always processed.
//
// An implementation that decided "intake is finished" by observing an empty queue
// would fail here, because an item can be published between that observation and
// the decision. Draining by intake accounting is what makes it correct.
func TestShutdownWithQueuedWorkProcessesEverything(t *testing.T) {
	t.Parallel()

	const trials = 300

	for trial := range trials {
		var processed atomic.Int64

		b := batcher.New(
			// Large batch and long interval: only shutdown can flush this.
			batcher.WithBatchSize[int](10_000),
			batcher.WithBatchInterval[int](time.Hour),
			batcher.WithProcessor(func(items []int) error {
				processed.Add(int64(len(items)))

				return nil
			}),
		)

		b.Add(1)

		require.NoError(t, b.Close(), "trial %d", trial)
		require.Equal(t, int64(1), processed.Load(),
			"trial %d: the queued item must be processed, not discarded", trial)
	}
}

// TestAddAfterSealIsRejectedNotPanicking pins that a late Add is a counted
// rejection.
//
// Previously this panicked with "send on closed channel", turning a graceful
// shutdown into a crash in the caller's goroutine. Silently succeeding would be
// just as bad, because the caller would believe the item was queued.
func TestAddAfterSealIsRejectedNotPanicking(t *testing.T) {
	t.Parallel()

	b := batcher.New(
		batcher.WithBatchSize[int](4),
		batcher.WithBatchInterval[int](time.Millisecond),
		batcher.WithProcessor(batcher.NoOpProcessor[int]),
	)

	require.NoError(t, b.Close())

	before := b.Stats()

	require.NotPanics(t, func() { b.Add(1) },
		"Add after shutdown must not panic")

	after := b.Stats()

	require.Equal(t, before.Accepted, after.Accepted,
		"a rejected Add must not count as accepted")
	require.Equal(t, before.Rejected+1, after.Rejected,
		"a rejected Add must be counted")
	require.Zero(t, after.Pending,
		"a rejected Add must leave no pending obligation")
}

// TestEnqueueAfterSealReportsClosing pins that Enqueue tells the caller why the
// item was refused, rather than failing silently the way Add must.
func TestEnqueueAfterSealReportsClosing(t *testing.T) {
	t.Parallel()

	b := batcher.New(
		batcher.WithBatchSize[int](4),
		batcher.WithBatchInterval[int](time.Millisecond),
		batcher.WithProcessor(batcher.NoOpProcessor[int]),
	)

	require.NoError(t, b.Close())

	err := b.Enqueue(context.Background(), 1)

	require.ErrorIs(t, err, batcher.ErrClosing)
	require.Zero(t, b.Stats().Pending, "a refused Enqueue must leave no obligation")
}

// TestEnqueueRespectsContextOnFullQueue pins back-pressure with a deadline: a
// bounded queue that is full must let the caller give up, and giving up must not
// leave a phantom obligation that would stall shutdown forever.
func TestEnqueueRespectsContextOnFullQueue(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	defer close(release)

	b := batcher.New(
		batcher.WithBatchSize[int](1),
		batcher.WithBatchInterval[int](time.Hour),
		batcher.WithMaxQueueSize[int](2),
		batcher.WithProcessor(func([]int) error {
			<-release

			return nil
		}),
	)

	saturate(t, b)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	before := b.Stats()
	err := b.Enqueue(ctx, 99)

	require.ErrorIs(t, err, context.DeadlineExceeded,
		"a full bounded queue must honour the caller's deadline")

	after := b.Stats()

	require.Equal(t, before.Accepted, after.Accepted,
		"a cancelled Enqueue must not count as accepted")
	require.Equal(t, before.Pending, after.Pending,
		"a cancelled Enqueue must roll its reservation back")
	require.Equal(t, before.Rejected+1, after.Rejected,
		"a cancelled Enqueue must be counted as rejected")
}

// TestBlockedEnqueueIsReleasedByShutdown pins that sealing releases a publisher
// parked on a full bounded queue.
//
// This is not a nicety. A parked publisher cannot leave the admission gate, and
// shutdown waits for the gate to empty, so without this release the two would
// deadlock permanently.
func TestBlockedEnqueueIsReleasedByShutdown(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	defer close(release)

	b := batcher.New(
		batcher.WithBatchSize[int](1),
		batcher.WithBatchInterval[int](time.Hour),
		batcher.WithMaxQueueSize[int](2),
		batcher.WithProcessor(func([]int) error {
			<-release

			return nil
		}),
	)

	saturate(t, b)

	parked := make(chan error, 1)

	go func() {
		parked <- b.Enqueue(context.Background(), 99)
	}()

	// Give the publisher time to actually park inside the queue.
	require.Eventually(t, func() bool {
		return b.Stats().PublishersInGate > 0
	}, 5*time.Second, time.Millisecond)

	go func() {
		_ = b.Close()
	}()

	select {
	case err := <-parked:
		require.ErrorIs(t, err, batcher.ErrClosing,
			"a parked publisher must be released with ErrClosing when sealing begins")
	case <-time.After(10 * time.Second):
		t.Fatal("a publisher parked on a full queue was never released by shutdown")
	}
}

// TestStartIsIdempotent pins that repeated and concurrent Start calls produce
// exactly one processing lifecycle.
//
// Previously each call spawned another loop whose deferred cleanup closed the same
// channels, so a second Start turned into a double-close panic.
func TestStartIsIdempotent(t *testing.T) {
	t.Parallel()

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

	var wg sync.WaitGroup

	for range 8 {
		wg.Add(1)

		go func() {
			defer wg.Done()

			b.Start()
		}()
	}

	wg.Wait()

	b.Add(1)

	require.NoError(t, b.Close())
	require.Equal(t, int64(1), processed.Load(),
		"exactly one lifecycle must process the item exactly once")
}

// TestShutdownIsIdempotent pins that concurrent and repeated shutdowns agree.
func TestShutdownIsIdempotent(t *testing.T) {
	t.Parallel()

	b := batcher.New(
		batcher.WithBatchSize[int](4),
		batcher.WithBatchInterval[int](time.Millisecond),
		batcher.WithProcessor(batcher.NoOpProcessor[int]),
	)

	b.Add(1)

	var (
		wg   sync.WaitGroup
		errs = make([]error, 4)
	)

	for i := range errs {
		wg.Add(1)

		go func(idx int) {
			defer wg.Done()

			errs[idx] = b.Close()
		}(i)
	}

	wg.Wait()

	for i, err := range errs {
		require.NoError(t, err, "caller %d must observe a completed drain", i)
	}

	require.True(t, b.IsClosed(), "the batcher must be terminal after shutdown")
	require.True(t, b.IsClosing(), "IsClosing must remain true once closed")
}

// TestErrorsChannelClosesExactlyOnceAfterShutdown pins that diagnostics terminate
// cleanly, so a consumer ranging over Errors() does not block forever and the
// channel is never closed twice.
func TestErrorsChannelClosesExactlyOnceAfterShutdown(t *testing.T) {
	t.Parallel()

	failure := errors.New("processor failed")

	b := batcher.New(
		batcher.WithBatchSize[int](1),
		batcher.WithBatchInterval[int](time.Millisecond),
		batcher.WithProcessor(func([]int) error {
			return failure
		}),
	)

	drained := make(chan int)

	go func() {
		count := 0

		for range b.Errors() {
			count++
		}

		drained <- count
	}()

	b.Add(1)

	require.NoError(t, b.Close())

	select {
	case count := <-drained:
		require.Positive(t, count, "the processor error must be observable")
	case <-time.After(10 * time.Second):
		t.Fatal("Errors() was not closed after shutdown; a consumer would block forever")
	}

	stats := b.Stats()

	require.Equal(t, uint64(1), stats.Failed, "a processor error counts as Failed")
	require.Zero(t, stats.Completed, "a failed batch must not also count as Completed")
	require.Zero(t, stats.Pending, "a failed batch must still release its obligation")
}

// TestIdleBatcherDoesNotSpin pins that steady-state idling costs nothing.
//
// The aggregator waits in select. If it polled instead, an idle batcher would burn
// CPU continuously, which matters because callers create many of them.
func TestIdleBatcherDoesNotSpin(t *testing.T) {
	t.Parallel()

	b := batcher.New(
		batcher.WithBatchSize[int](1_000),
		batcher.WithBatchInterval[int](10*time.Millisecond),
		batcher.WithProcessor(batcher.NoOpProcessor[int]),
	)

	t.Cleanup(func() { _ = b.Close() })

	// An idle batcher must not invoke the processor at all, no matter how many
	// intervals elapse.
	time.Sleep(200 * time.Millisecond)

	stats := b.Stats()

	require.Zero(t, stats.Accepted)
	require.Zero(t, stats.Completed, "an idle batcher must never flush an empty batch")
	require.Zero(t, stats.Queued)
}

// saturate fills a bounded batcher whose processor is blocked, so that a further
// enqueue is guaranteed to park.
//
// It enqueues with a short deadline until admission refuses, which is the only
// reliable signal that the queue is genuinely full: the aggregator drains greedily,
// so simply watching Queued can race with it.
func saturate(t *testing.T, b *batcher.Batcher[int]) {
	t.Helper()

	for attempt := range 1_000 {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		err := b.Enqueue(ctx, attempt)

		cancel()

		if err != nil {
			require.ErrorIs(t, err, context.DeadlineExceeded,
				"saturation should end with a deadline, not %v", err)

			return
		}
	}

	t.Fatal("bounded queue never filled; the capacity bound is not being enforced")
}
