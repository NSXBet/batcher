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

// Stability tests: lifecycle interleavings that must not hang, panic, or lose work.
//
// Each of these covers behaviour that is currently correct but pinned by nothing, so
// a future refactor could break it silently. They are written as repeated trials
// rather than single runs because every one of them is an ordering race, and a single
// pass proves very little about a race.

// TestBlockingAddIsReleasedByShutdown covers the one admission path with no error
// return.
//
// Enqueue blocked on a full bounded queue is already tested, but Add is the
// compatibility fast path: it returns nothing, so if shutdown failed to release it
// the caller would hang with no way to observe why. That makes it the worst place to
// leave untested, and it is only reachable in bounded mode.
func TestBlockingAddIsReleasedByShutdown(t *testing.T) {
	t.Parallel()

	const trials = 100

	for trial := range trials {
		release := make(chan struct{})

		var releaseOnce sync.Once

		releaseAll := func() { releaseOnce.Do(func() { close(release) }) }

		b := batcher.New(
			batcher.WithBatchSize[int](1),
			batcher.WithBatchInterval[int](time.Hour),
			batcher.WithMaxQueueSize[int](2),
			// Unstarted, so nothing drains and the queue stays full.
			batcher.WithSkipAutoStart[int](),
			batcher.WithProcessor(func([]int) error {
				<-release

				return nil
			}),
		)

		func() {
			defer releaseAll()

			for i := range 2 {
				b.Add(i)
			}

			parked := make(chan struct{})

			go func() {
				defer close(parked)

				b.Add(99) // must block: bounded queue full, nothing consuming
			}()

			shutdownDone := make(chan error, 1)

			go func() { shutdownDone <- b.Close() }()

			select {
			case <-parked:
			case <-time.After(15 * time.Second):
				t.Fatalf("trial %d: a blocking Add was never released by shutdown; "+
					"Add has no error return, so the caller would hang indefinitely", trial)
			}

			releaseAll()

			select {
			case <-shutdownDone:
			case <-time.After(15 * time.Second):
				t.Fatalf("trial %d: shutdown never completed with a parked Add", trial)
			}
		}()
	}
}

// TestAddRacingCloseKeepsAccountingExact covers the interleaving a caller is most
// likely to produce accidentally: a request handler still enqueuing while the
// process shuts down.
//
// Every accepted item must reach exactly one terminal outcome. A lost item, a
// double-count, or a send on a closed channel all show up here.
func TestAddRacingCloseKeepsAccountingExact(t *testing.T) {
	t.Parallel()

	const (
		trials      = 200
		perProducer = 50
	)

	for trial := range trials {
		b := batcher.New(
			batcher.WithBatchSize[int](8),
			batcher.WithBatchInterval[int](time.Millisecond),
			batcher.WithProcessor(batcher.NoOpProcessor[int]),
		)

		produced := make(chan struct{})

		go func() {
			defer close(produced)

			for i := range perProducer {
				b.Add(i)
			}
		}()

		require.NoError(t, b.Close(), "trial %d", trial)
		<-produced

		stats := b.Stats()

		require.Equal(t, stats.Accepted, stats.Completed+stats.Failed+stats.Panicked,
			"trial %d: every accepted item must reach exactly one terminal outcome "+
				"(accepted=%d completed=%d failed=%d panicked=%d)",
			trial, stats.Accepted, stats.Completed, stats.Failed, stats.Panicked)
		require.Zero(t, stats.Pending, "trial %d", trial)
		require.Zero(t, stats.IntakePending, "trial %d", trial)
		require.LessOrEqual(t, stats.Accepted, uint64(perProducer),
			"trial %d: cannot accept more than was offered", trial)
	}
}

// TestPanicInFinalBatchStillClosesDiagnostics covers the narrowest window in the
// shutdown protocol: a diagnostic published from the last batch, while the
// coordinator is closing the diagnostics channel.
//
// Get the single-closer rule wrong and this panics with "send on closed channel"
// inside the library's own goroutine, where the caller cannot recover it.
func TestPanicInFinalBatchStillClosesDiagnostics(t *testing.T) {
	t.Parallel()

	const trials = 150

	for trial := range trials {
		b := batcher.New(
			// Large batch, long interval: only shutdown flushes this.
			batcher.WithBatchSize[int](100),
			batcher.WithBatchInterval[int](time.Hour),
			batcher.WithProcessor(func([]int) error {
				panic("poison final batch")
			}),
		)

		drained := make(chan int, 1)

		go func() {
			count := 0

			for range b.Errors() {
				count++
			}

			drained <- count
		}()

		b.Add(1)

		require.NoError(t, b.Shutdown(context.Background()), "trial %d", trial)

		select {
		case count := <-drained:
			require.Positive(t, count,
				"trial %d: the recovered panic must be observable", trial)
		case <-time.After(15 * time.Second):
			t.Fatalf("trial %d: Errors() never closed, so a ranging consumer "+
				"would block forever", trial)
		}

		stats := b.Stats()

		require.Equal(t, uint64(1), stats.Panicked, "trial %d", trial)
		require.Zero(t, stats.Pending,
			"trial %d: a panic must not strand a drain obligation", trial)
	}
}

// TestConcurrentShutdownCallersAllTerminate covers many goroutines calling Shutdown
// at once, which is what happens when several subsystems react to the same signal.
//
// Exactly one drain must run, none may deadlock, and no caller may receive a false
// error for work that completed.
func TestConcurrentShutdownCallersAllTerminate(t *testing.T) {
	t.Parallel()

	const (
		trials  = 100
		callers = 8
	)

	for trial := range trials {
		var processed atomic.Int64

		b := batcher.New(
			batcher.WithBatchSize[int](4),
			batcher.WithBatchInterval[int](time.Millisecond),
			batcher.WithProcessor(func(items []int) error {
				processed.Add(int64(len(items)))

				return nil
			}),
		)

		for i := range 12 {
			b.Add(i)
		}

		var (
			wg   sync.WaitGroup
			errs = make([]error, callers)
		)

		for i := range callers {
			wg.Add(1)

			go func(idx int) {
				defer wg.Done()

				errs[idx] = b.Shutdown(context.Background())
			}(i)
		}

		wg.Wait()

		for i, err := range errs {
			require.NoError(t, err,
				"trial %d caller %d: an unbounded wait must observe the completed drain",
				trial, i)
		}

		require.True(t, b.IsClosed(), "trial %d", trial)
		require.Equal(t, int64(12), processed.Load(),
			"trial %d: every item processed exactly once regardless of caller count", trial)
	}
}

// TestErroringProcessorDoesNotStallShutdown covers the failure mode that hangs a
// process rather than reporting: a processor that always errors.
//
// A failed batch must still release its drain obligation. If it did not, Pending
// would never reach zero and shutdown would wait forever on work that already ran.
func TestErroringProcessorDoesNotStallShutdown(t *testing.T) {
	t.Parallel()

	failure := errors.New("downstream unavailable")

	const items = 200

	b := batcher.New(
		batcher.WithBatchSize[int](1),
		batcher.WithBatchInterval[int](time.Millisecond),
		// Deliberately tiny: most diagnostics are dropped, which must not block.
		batcher.WithErrorBufferSize[int](2),
		batcher.WithProcessor(func([]int) error {
			return failure
		}),
	)

	for i := range items {
		b.Add(i)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	require.NoError(t, b.Shutdown(ctx),
		"a processor that always fails must not prevent the drain from completing")

	stats := b.Stats()

	require.Equal(t, uint64(items), stats.Failed)
	require.Zero(t, stats.Completed)
	require.Zero(t, stats.Pending)
	require.Positive(t, stats.DroppedErrors,
		"with a 2-entry buffer and %d failures, diagnostics must be dropped and counted",
		items)
}

// TestRepeatedStartAndShutdownIsStable covers Start and Shutdown racing from several
// goroutines, which is reachable through Fx lifecycle hooks and supervisors.
//
// Exactly one processing lifecycle must exist, and the batcher must always reach a
// terminal state.
func TestRepeatedStartAndShutdownIsStable(t *testing.T) {
	t.Parallel()

	const trials = 150

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

		for range 4 {
			wg.Add(1)

			go func() {
				defer wg.Done()

				b.Start()
			}()
		}

		wg.Add(1)

		go func() {
			defer wg.Done()

			_ = b.Shutdown(context.Background())
		}()

		wg.Wait()

		require.NoError(t, b.Shutdown(context.Background()), "trial %d", trial)
		require.True(t, b.IsClosed(), "trial %d", trial)
		require.Equal(t, int64(1), processed.Load(),
			"trial %d: the item must be processed exactly once by one lifecycle", trial)
	}
}
