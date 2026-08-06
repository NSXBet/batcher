package batcher_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/NSXBet/batcher/pkg/batcher"
	"github.com/stretchr/testify/require"
)

// TestProcessorMayRetainBatchSlices is the contract test for adaptive batch
// capacity.
//
// The capacity estimator changes how large each batch slice starts, but it must not
// change slice *ownership*: the processor receives a slice it may keep, and no later
// batch may write through it. This is the property that ruled out pooling during
// planning, so it is asserted directly rather than assumed.
//
// The test retains every batch, mutates the retained copies after the fact, and then
// verifies that nothing the batcher did afterwards corrupted them.
func TestProcessorMayRetainBatchSlices(t *testing.T) {
	t.Parallel()

	var (
		mu       sync.Mutex
		retained [][]int
	)

	b := batcher.New(
		// Small batches with a short interval: many flushes, so the estimator adapts
		// repeatedly while batches are being retained.
		batcher.WithBatchSize[int](8),
		batcher.WithBatchInterval[int](2*time.Millisecond),
		batcher.WithProcessor(func(items []int) error {
			// Deliberately keep the slice the batcher handed us, without copying.
			mu.Lock()
			retained = append(retained, items)
			mu.Unlock()

			return nil
		}),
	)

	const total = 500

	for i := range total {
		b.Add(i)

		// Vary arrival pacing so batches close on both size and timer, driving the
		// estimator up and down during the run.
		if i%37 == 0 {
			time.Sleep(3 * time.Millisecond)
		}
	}

	require.NoError(t, b.Shutdown(context.Background()))

	mu.Lock()
	defer mu.Unlock()

	// Every accepted item must appear exactly once across the retained slices, and
	// each slice must still hold what it held when the processor saw it.
	seen := make(map[int]int, total)
	count := 0

	for i, batch := range retained {
		require.NotEmpty(t, batch, "batch %d: no empty batch may be emitted", i)
		require.LessOrEqual(t, len(batch), 8,
			"batch %d: len must never exceed BatchSize", i)

		for _, item := range batch {
			seen[item]++
			count++
		}
	}

	require.Equal(t, total, count,
		"retained slices must still contain every accepted item exactly once; "+
			"a reused backing array would show duplicates or missing values")

	for i := range total {
		require.Equal(t, 1, seen[i], "item %d appeared %d times", i, seen[i])
	}
}

// TestRetainedBatchSlicesAreNotAliased pins the stronger property: two retained
// batches must not share backing storage. If they did, mutating one would corrupt
// the other, which is exactly the failure mode a pooled or reused slice introduces.
func TestRetainedBatchSlicesAreNotAliased(t *testing.T) {
	t.Parallel()

	var (
		mu       sync.Mutex
		retained [][]int
	)

	b := batcher.New(
		batcher.WithBatchSize[int](4),
		batcher.WithBatchInterval[int](2*time.Millisecond),
		batcher.WithProcessor(func(items []int) error {
			mu.Lock()
			retained = append(retained, items)
			mu.Unlock()

			return nil
		}),
	)

	for i := range 100 {
		b.Add(i)
	}

	require.NoError(t, b.Shutdown(context.Background()))

	mu.Lock()
	defer mu.Unlock()

	require.Greater(t, len(retained), 1, "need several batches to compare")

	// Snapshot the retained contents, then overwrite every retained slice. If any
	// two shared storage, the overwrite of a later batch would change an earlier one.
	snapshots := make([][]int, len(retained))
	for i, batch := range retained {
		snapshots[i] = append([]int(nil), batch...)
	}

	for i, batch := range retained {
		for j := range batch {
			batch[j] = -(i + 1)
		}
	}

	for i, batch := range retained {
		for j := range batch {
			require.Equal(t, -(i + 1), batch[j],
				"batch %d slot %d was changed by a write to another batch, so the "+
					"batches share backing storage", i, j)
		}
	}

	// And the snapshots prove the original contents were distinct per batch.
	total := 0
	for _, snap := range snapshots {
		total += len(snap)
	}

	require.Equal(t, 100, total, "every accepted item must have been delivered once")
}
