package scenario_test

import (
	"testing"
	"time"
	"unsafe"

	"github.com/NSXBet/batcher/test/scenario"
	"github.com/stretchr/testify/require"
)

// TestSparseWindowAllocationEvidence measures allocated bytes per flush for the
// workload Milestone 4.2 targets: a small window with a large BatchSize, where
// every batch closes on the timer holding far fewer items than it reserved
// capacity for.
//
// This exists to decide whether 4.2 should be implemented at all. The plan gates it
// on sparse-window allocation pressure above 2 KB/flush.
//
// The "bytes/flush (est)" column is the PRE-ADAPTIVE BASELINE, deliberately kept
// after 4.2 landed: it is (BatchSize - actual) * sizeof(T), the waste a fixed
// make([]T, 0, BatchSize) reservation would incur. The aggregator now reserves
// capacities.capacity() instead, so this column reports the waste adaptive capacity
// removed rather than waste that remains. It is the figure the milestone is measured
// against, which is why it is not recomputed from the current reservation.
//
// The measurements are report-only: no allocation threshold is asserted, because the
// numbers are an input to a decision rather than a gate. The scenario's own validity
// is still enforced -- a run that timed out is not evidence of anything, so
// result.TimedOut fails the test.
func TestSparseWindowAllocationEvidence(t *testing.T) {
	t.Parallel()

	if testing.Short() {
		t.Skip("measurement only; runs in the full suite")
	}

	cases := []struct {
		name      string
		batchSize int
		window    time.Duration
		rate      int
	}{
		// The worst realistic case: 1ms window, 1000-item BatchSize, sparse arrivals.
		{"sparse/1ms/batch=1000", 1_000, time.Millisecond, 200},
		{"sparse/5ms/batch=1000", 1_000, 5 * time.Millisecond, 200},
		{"sparse/1ms/batch=10000", 10_000, time.Millisecond, 200},
		// Moderate rate: batches still close on the timer well below BatchSize.
		{"steady/5ms/batch=1000", 1_000, 5 * time.Millisecond, 10_000},
		// Control: batches fill, so reserved capacity is actually used.
		{"full/100ms/batch=1000", 1_000, 100 * time.Millisecond, 50_000},
	}

	t.Logf("%-26s %-11s %-9s %-12s %s",
		"scenario", "mean_batch", "batches", "allocs/item", "bytes/flush (pre-4.2 est)")

	for _, c := range cases {
		result := scenario.Run(scenario.Config{
			Name:           c.name,
			BatchSize:      c.batchSize,
			BatchInterval:  c.window,
			Arrival:        scenario.Steady(c.rate, time.Second),
			Processor:      scenario.NoOpProcessor(),
			LatenessBudget: time.Second,
		})

		require.False(t, result.TimedOut,
			"%s: scenario timed out; allocation evidence is invalid", c.name)

		// scenario.Item is the harness payload. BatchSize, not the adaptive estimate,
		// is deliberately used here: this reproduces what a fixed reservation would
		// have cost, which is the baseline 4.2 is compared against.
		itemSize := float64(unsafe.Sizeof(scenario.Item{}))

		wastePerFlush := float64(c.batchSize-int(result.MeanBatchSize)) * itemSize
		if wastePerFlush < 0 {
			wastePerFlush = 0
		}

		t.Logf("%-26s %-11.0f %-9d %-12.3f %.0f B",
			c.name, result.MeanBatchSize, result.Batches,
			result.AllocsPerItem, wastePerFlush)
	}
}

// TestSparseWindowAllocationEvidenceRejectsTimedOutRun pins that evidence is not
// logged when the processor did not finish. A timed-out run has incomplete batches
// and allocation counts, so treating it as capacity evidence would be misleading.
func TestSparseWindowAllocationEvidenceRejectsTimedOutRun(t *testing.T) {
	t.Parallel()

	result := scenario.Run(scenario.Config{
		Name:               "timed-out-evidence",
		BatchSize:          1,
		BatchInterval:      time.Millisecond,
		Arrival:            scenario.Steady(1_000, 100*time.Millisecond),
		Processor:          scenario.FixedProcessor(100 * time.Millisecond),
		CompletionDeadline: time.Millisecond,
		LatenessBudget:     time.Second,
	})

	require.True(t, result.TimedOut, "the deliberately blocked run must time out")
}
