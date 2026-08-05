package scenario_test

import (
	"errors"
	"testing"
	"time"

	"github.com/NSXBet/batcher/internal/scenario"
	"github.com/stretchr/testify/require"
)

// TestHarnessIsOpenLoop asserts the generator offers its whole schedule even
// when the processor cannot keep up. A closed-loop harness would silently stop
// offering load here, which is the coordinated-omission trap this design avoids.
func TestHarnessIsOpenLoop(t *testing.T) {
	t.Parallel()

	const offered = 200

	result := scenario.Run(scenario.Config{
		Name:           "open-loop-under-slow-processor",
		BatchSize:      10,
		BatchInterval:  2 * time.Millisecond,
		Arrival:        scenario.Sparse(offered, time.Millisecond),
		Processor:      scenario.FixedProcessor(5 * time.Millisecond),
		LatenessBudget: time.Second, // this run is expected to be late; see below
	})

	require.Equal(t, offered, result.EndToEnd.Count+result.RejectedCount,
		"every scheduled item must be offered regardless of processor speed")
	require.Positive(t, result.Lateness.P99,
		"a slow processor must show up as generator lateness, not as suppressed load")
}

// TestHarnessReportsLatenessAndInvalidatesBadRuns asserts the validity guard.
// Without it, a generator that itself fell behind would be reported as batcher
// latency, which is how benchmark harnesses produce confidently wrong numbers.
func TestHarnessReportsLatenessAndInvalidatesBadRuns(t *testing.T) {
	t.Parallel()

	healthy := scenario.Run(scenario.Config{
		Name:           "healthy",
		BatchSize:      100,
		BatchInterval:  5 * time.Millisecond,
		Arrival:        scenario.Sparse(100, 2*time.Millisecond),
		Processor:      scenario.NoOpProcessor(),
		LatenessBudget: 5 * time.Millisecond,
	})

	require.True(t, healthy.LatenessValid,
		"an idle no-op run must not be marked invalid (p99 lateness %s)", healthy.Lateness.P99)

	strict := scenario.Run(scenario.Config{
		Name:           "impossible-budget",
		BatchSize:      100,
		BatchInterval:  5 * time.Millisecond,
		Arrival:        scenario.Sparse(100, 2*time.Millisecond),
		Processor:      scenario.NoOpProcessor(),
		LatenessBudget: time.Nanosecond, // no real scheduler can meet this
	})

	require.False(t, strict.LatenessValid,
		"a run whose lateness exceeds its budget must be marked invalid")
}

// TestHarnessRecorderDoesNotAllocatePerItem is the harness self-check required
// before trusting any allocation number it reports. If the recorder allocated
// per item, every allocation measurement would be measuring the harness.
func TestHarnessRecorderDoesNotAllocatePerItem(t *testing.T) {
	t.Parallel()

	small := scenario.Run(scenario.Config{
		Name:          "alloc-small",
		BatchSize:     100,
		BatchInterval: 2 * time.Millisecond,
		Arrival:       scenario.Steady(20_000, 100*time.Millisecond),
		Processor:     scenario.NoOpProcessor(),
	})

	large := scenario.Run(scenario.Config{
		Name:          "alloc-large",
		BatchSize:     100,
		BatchInterval: 2 * time.Millisecond,
		Arrival:       scenario.Steady(20_000, 400*time.Millisecond),
		Processor:     scenario.NoOpProcessor(),
	})

	// Per-item allocations must not grow with run length. Batcher itself
	// allocates per item today, so the absolute number is not zero; what matters
	// is that the harness adds no per-item growth of its own.
	require.InDelta(t, small.AllocsPerItem, large.AllocsPerItem, 1.0,
		"per-item allocations must not scale with run length (small=%.2f large=%.2f)",
		small.AllocsPerItem, large.AllocsPerItem)
}

// TestReproducesInlineSlowProcessorInversion pins the central finding that
// motivates the whole plan: because the processor runs inline in the aggregation
// loop, the effective window becomes max(window, processor_duration). A smaller
// configured window therefore makes latency WORSE, not better.
//
// If a future change decouples intake from processing, this test should start
// failing, and that failure is the signal that the fix worked.
func TestReproducesInlineSlowProcessorInversion(t *testing.T) {
	t.Parallel()

	if testing.Short() {
		t.Skip("timing-sensitive; runs in the full suite only")
	}

	const (
		rate        = 10_000
		duration    = 1500 * time.Millisecond
		serviceTime = 50 * time.Millisecond
	)

	small := scenario.Run(scenario.Config{
		Name:           "window=5ms",
		BatchSize:      100_000, // never size-triggered: isolate the timer
		BatchInterval:  5 * time.Millisecond,
		Arrival:        scenario.Steady(rate, duration),
		Processor:      scenario.FixedProcessor(serviceTime),
		LatenessBudget: time.Second,
	})

	large := scenario.Run(scenario.Config{
		Name:           "window=100ms",
		BatchSize:      100_000,
		BatchInterval:  100 * time.Millisecond,
		Arrival:        scenario.Steady(rate, duration),
		Processor:      scenario.FixedProcessor(serviceTime),
		LatenessBudget: time.Second,
	})

	t.Logf("5ms window:   p50=%s p99=%s mean_batch=%.0f downstream/s=%.0f",
		small.EndToEnd.P50, small.EndToEnd.P99, small.MeanBatchSize, small.DownstreamPerSec)
	t.Logf("100ms window: p50=%s p99=%s mean_batch=%.0f downstream/s=%.0f",
		large.EndToEnd.P50, large.EndToEnd.P99, large.MeanBatchSize, large.DownstreamPerSec)

	require.Greater(t, small.EndToEnd.P50, large.EndToEnd.P50,
		"with a %s processor the 5ms window should be SLOWER than 100ms, "+
			"because the inline processor bounds the effective window", serviceTime)
}

// TestSmallWindowGivesNoOverloadProtection pins the second baseline finding:
// shrinking the batch window does not bound queued work. Overload protection
// requires bounded admission, which the library does not yet offer.
func TestSmallWindowGivesNoOverloadProtection(t *testing.T) {
	t.Parallel()

	if testing.Short() {
		t.Skip("allocates aggressively; runs in the full suite only")
	}

	const (
		rate     = 500_000
		duration = 300 * time.Millisecond
	)

	for _, window := range []time.Duration{100 * time.Millisecond, 1 * time.Millisecond} {
		result := scenario.Run(scenario.Config{
			Name:           "overload",
			BatchSize:      500,
			BatchInterval:  window,
			Arrival:        scenario.Steady(rate, duration),
			Processor:      scenario.FixedProcessor(2 * time.Millisecond),
			LatenessBudget: time.Second,
		})

		t.Logf("window=%-6s offered/s=%.0f accepted/s=%.0f completed/s=%.0f "+
			"pending_peak=%d heap_peak=%dMB",
			window, result.OfferedRate, result.AcceptedRate, result.CompletedRate,
			result.PendingPeak, result.HeapHighWater/(1<<20))

		require.Zero(t, result.RejectedCount,
			"window=%s: an unbounded queue must accept everything, which is exactly "+
				"why a smaller window cannot protect the process", window)

		// The load is deliberately far above what a 2ms processor can retire, so a
		// large backlog must accumulate no matter how small the window is. This is
		// the evidence that overload protection needs bounded admission, not a
		// shorter timer.
		require.Greater(t, result.PendingPeak, int64(rate/10),
			"window=%s: expected a large backlog to accumulate under overload", window)
	}
}

// TestErroringProcessorDoesNotStallTheHarness keeps the diagnostics path covered,
// so scenarios can model failing downstreams without hanging the run.
func TestErroringProcessorDoesNotStallTheHarness(t *testing.T) {
	t.Parallel()

	result := scenario.Run(scenario.Config{
		Name:          "erroring",
		BatchSize:     10,
		BatchInterval: 2 * time.Millisecond,
		Arrival:       scenario.Sparse(50, time.Millisecond),
		Processor:     scenario.ErroringProcessor(errors.New("downstream unavailable")),
	})

	require.Positive(t, result.Batches, "batches must still be processed when the processor errors")
	require.Positive(t, result.EndToEnd.Count, "latency must still be recorded for failed batches")
}

// TestDeterministicSchedules asserts fixed seeds produce identical schedules, so
// a reported result can be reproduced exactly.
func TestDeterministicSchedules(t *testing.T) {
	t.Parallel()

	arrival := scenario.Poisson(5_000, 200*time.Millisecond)

	first := arrival.Schedule(42)
	second := arrival.Schedule(42)
	different := arrival.Schedule(43)

	require.Equal(t, first, second, "same seed must produce the same schedule")
	require.NotEqual(t, first, different, "different seeds must produce different schedules")
}
