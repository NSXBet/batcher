package batcher_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NSXBet/batcher/pkg/batcher"
	"github.com/stretchr/testify/require"
)

// Stats snapshot contract tests.
//
// Stats is intentionally an eventually consistent diagnostic interface: each
// field is read independently, so callers cannot use one live snapshot as a
// transactional accounting assertion. These tests pin the meaningful contract:
// ownership transitions are represented, terminal accounting converges after the
// drain, and observing the snapshot is allocation-free and cannot perturb Add.

// TestStatsShowsAggregatorHeldPartialBatch distinguishes the three ownership
// states. A partial batch has left the intake queue but cannot yet be in flight,
// because no worker receives it until the timer/size/shutdown flush. That item must
// be visible as BatchHeld rather than disappearing between Queued and InFlight.
func TestStatsShowsAggregatorHeldPartialBatch(t *testing.T) {
	t.Parallel()

	b := batcher.New(
		batcher.WithBatchSize[int](100),
		batcher.WithBatchInterval[int](time.Hour), // only shutdown can flush it
		batcher.WithProcessor(batcher.NoOpProcessor[int]),
	)

	b.Add(1)

	require.Eventually(t, func() bool {
		stats := b.Stats()

		return stats.BatchHeld == 1 && stats.Queued == 0 && stats.InFlight == 0
	}, 5*time.Second, time.Millisecond,
		"an item received by the aggregator but not dispatched must be BatchHeld")

	before := b.Stats()

	require.Equal(t, int64(1), before.Pending)
	require.Equal(t, int64(1), before.BatchHeld)
	require.Equal(t, uint64(0), before.BatchesFlushed)

	require.NoError(t, b.Close())

	after := b.Stats()

	require.Zero(t, after.Pending)
	require.Zero(t, after.BatchHeld)
	require.Zero(t, after.InFlight)
	require.Equal(t, uint64(1), after.BatchesFlushed,
		"the shutdown partial batch is still a flushed batch")
	require.Equal(t, uint64(1), after.Completed)
}

// TestStatsCountsBatchesFlushedAcrossEveryFlushPath pins that BatchesFlushed is a
// batch counter, independent of whether a flush happened by size, timer, or final
// shutdown. This is the coalescing signal operators need when choosing a window.
func TestStatsCountsBatchesFlushedAcrossEveryFlushPath(t *testing.T) {
	t.Parallel()

	const (
		batchSize = 3
		interval  = 30 * time.Millisecond
	)

	b := batcher.New(
		batcher.WithBatchSize[int](batchSize),
		batcher.WithBatchInterval[int](interval),
		batcher.WithProcessor(batcher.NoOpProcessor[int]),
	)

	// Size flush.
	for i := range batchSize {
		b.Add(i)
	}

	require.Eventually(t, func() bool {
		return b.Stats().BatchesFlushed >= 1
	}, 5*time.Second, time.Millisecond)

	// Timer flush.
	b.Add(100)
	b.Add(101)

	require.Eventually(t, func() bool {
		return b.Stats().BatchesFlushed >= 2
	}, 5*time.Second, time.Millisecond)

	// Shutdown partial flush.
	b.Add(200)

	require.NoError(t, b.Close())

	stats := b.Stats()

	require.Equal(t, uint64(3), stats.BatchesFlushed)
	require.Equal(t, uint64(batchSize+2+1), stats.Completed)
	require.Zero(t, stats.Pending)
	require.Zero(t, stats.BatchHeld)
	require.Zero(t, stats.InFlight)
}

// TestStatsTerminalOutcomesAreMutuallyExclusive pins accounting at the only point
// where a multi-field equality is meaningful: after terminal drain, when no
// publisher or worker can still be moving an item between fields.
func TestStatsTerminalOutcomesAreMutuallyExclusive(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	failure := errors.New("processor failed")

	b := batcher.New(
		batcher.WithBatchSize[int](1),
		batcher.WithBatchInterval[int](time.Millisecond),
		batcher.WithProcessor(func([]int) error {
			switch calls.Add(1) {
			case 1:
				return nil
			case 2:
				return failure
			default:
				panic("poison batch")
			}
		}),
	)

	// Do not let diagnostics block the scenario: outcomes, not diagnostics,
	// are under test here.
	go func() {
		for range b.Errors() {
		}
	}()

	for i := range 3 {
		b.Add(i)
	}

	require.NoError(t, b.Shutdown(context.Background()))

	stats := b.Stats()

	require.Equal(t, uint64(3), stats.Accepted)
	require.Equal(t, uint64(1), stats.Completed)
	require.Equal(t, uint64(1), stats.Failed)
	require.Equal(t, uint64(1), stats.Panicked)
	require.Equal(t, stats.Accepted,
		stats.Completed+stats.Failed+stats.Panicked,
		"terminal categories are mutually exclusive and exhaustive after drain")
	require.Zero(t, stats.Pending)
	require.Zero(t, stats.IntakePending)
	require.Zero(t, stats.BatchHeld)
	require.Zero(t, stats.InFlight)
	require.Zero(t, stats.PublishersInGate)
}

// TestStatsIsAllocationFree pins the read-side performance contract. Queued takes
// the queue mutex for a length check, but neither that lock nor the atomic loads
// may allocate. Metrics scraping must never generate garbage.
func TestStatsIsAllocationFree(t *testing.T) {
	// Deliberately not parallel: AllocsPerRun requires exclusive GOMAXPROCS.
	b := batcher.New(
		batcher.WithBatchSize[int](100),
		batcher.WithBatchInterval[int](time.Hour),
		batcher.WithProcessor(batcher.NoOpProcessor[int]),
	)

	defer func() { require.NoError(t, b.Close()) }()

	for range 100 {
		b.Add(1)
	}

	allocs := testing.AllocsPerRun(2_000, func() {
		_ = b.Stats()
	})

	require.Zero(t, allocs, "Stats must not allocate")
}

// TestStatsIsEventuallyConsistent, rather than pretending live counters are a
// transaction, makes the boundary explicit. While the processor is blocked, the
// snapshot has a stable meaningful ownership state. Once it is released and
// terminal drain completes, the conservation equality holds. Callers must not
// infer that arbitrary intermediate snapshots satisfy the latter equality.
func TestStatsIsEventuallyConsistent(t *testing.T) {
	t.Parallel()

	entered := make(chan struct{})
	release := make(chan struct{})

	b := batcher.New(
		batcher.WithBatchSize[int](1),
		batcher.WithBatchInterval[int](time.Millisecond),
		batcher.WithProcessor(func([]int) error {
			close(entered)
			<-release

			return nil
		}),
	)

	b.Add(1)

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("processor did not start")
	}

	live := b.Stats()

	require.Equal(t, int64(1), live.InFlight,
		"a stable live snapshot exposes the item in its current ownership state")
	require.Equal(t, int64(1), live.Pending)

	close(release)
	require.NoError(t, b.Shutdown(context.Background()))

	settled := b.Stats()

	require.Zero(t, settled.Pending)
	require.Equal(t, settled.Accepted,
		settled.Completed+settled.Failed+settled.Panicked,
		"only a terminally drained snapshot is a valid accounting assertion")
}
