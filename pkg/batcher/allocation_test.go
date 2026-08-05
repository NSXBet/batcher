package batcher_test

import (
	"testing"
	"time"

	"github.com/NSXBet/batcher/internal/test"
	"github.com/NSXBet/batcher/pkg/batcher"
	"github.com/stretchr/testify/require"
)

// TestAddAllocatesNothingPerCall is a blocking allocation gate.
//
// Allocation counts are the signal the PR lane enforces, because they are stable
// even on noisy shared runners, whereas latency percentiles there are not.
//
// Scope: this measures the caller-visible cost of Add only. Batcher still
// allocates per batch on the aggregation path (the batch slice) and inside the
// unbounded queue's internal buffer; those are amortised across a batch and are
// measured by the scenario harness, not here.
//
// Threshold recorded in docs/improvements/thresholds.md.
func TestAddAllocatesNothingPerCall(t *testing.T) {
	// Deliberately not parallel: testing.AllocsPerRun requires exclusive control
	// of GOMAXPROCS and panics when called from a parallel test.
	batch := batcher.New(
		batcher.WithProcessor(func(_ []test.BatchItem) error {
			return nil
		}),
		batcher.WithBatchSize[test.BatchItem](1_000),
		batcher.WithBatchInterval[test.BatchItem](5*time.Millisecond),
	)

	t.Cleanup(func() {
		require.NoError(t, batch.Close())
	})

	item := test.BatchItem{Key: "allocation-gate"}

	// Warm up so queue buffers reach a steady capacity. Without this, the growth
	// of internal buffers would be attributed to Add.
	for range 10_000 {
		batch.Add(item)
	}

	require.NoError(t, batch.Join(30*time.Second))

	allocs := testing.AllocsPerRun(2_000, func() {
		batch.Add(item)
	})

	require.Zero(t, allocs,
		"Add must not allocate per call in steady state; got %v allocations", allocs)

	require.NoError(t, batch.Join(30*time.Second))
}
