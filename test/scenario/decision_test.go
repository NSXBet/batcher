package scenario_test

import (
	"os"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/NSXBet/batcher/test/scenario"
	"github.com/stretchr/testify/require"
)

// TestDefaultWindowDecisionMatrix produces the evidence for Milestone 5.3: whether
// DefaultBatchInterval should move from 1s toward the 5-10ms range.
//
// It reports the four things the decision needs, per window:
//
//   - latency the caller experiences (p50/p99/p99.9/max);
//   - coalescing actually achieved (mean batch size, downstream calls/s);
//   - allocation cost per item;
//   - whether the measurement is even valid (schedule lateness).
//
// A window is only worth recommending if it improves latency AND still coalesces at
// the arrival rates a shared service sees. A window that reduces latency by turning
// every batch into a single item has not helped: it has just removed batching.
//
// Report-only. Latency on a shared machine is not a pass/fail gate, and this exists
// to inform a decision rather than to police one.
func TestDefaultWindowDecisionMatrix(t *testing.T) {
	if os.Getenv("SCENARIO_MATRIX") == "" {
		t.Skip("set SCENARIO_MATRIX=1 to run the decision matrix")
	}

	windows := []time.Duration{
		time.Millisecond,
		2 * time.Millisecond,
		5 * time.Millisecond,
		10 * time.Millisecond,
		20 * time.Millisecond,
		50 * time.Millisecond,
		100 * time.Millisecond,
		time.Second, // the current default
	}

	// Rates chosen to bracket the interesting region: below, at, and well above the
	// point where a small window still fills a useful batch.
	rates := []int{1_000, 10_000, 50_000}

	const duration = 2 * time.Second

	t.Logf("environment: %s", scenario.CurrentEnvironment())
	t.Logf("processor: no-op (isolates batching cost from downstream cost)")
	t.Logf("")
	t.Logf("%-8s %-8s %-10s %-10s %-10s %-9s %-10s %-9s %s",
		"rate", "window", "p50", "p99", "p99.9", "batch", "calls/s", "alloc/item", "valid")

	for _, rate := range rates {
		for _, window := range windows {
			result := scenario.Run(scenario.Config{
				Name:           "decision",
				BatchSize:      1_000,
				BatchInterval:  window,
				Arrival:        scenario.Steady(rate, duration),
				Processor:      scenario.NoOpProcessor(),
				Producers:      runtime.NumCPU(),
				Warmup:         200 * time.Millisecond,
				Seed:           1,
				LatenessBudget: 5 * time.Millisecond,
			})

			valid := "yes"
			if !result.LatenessValid {
				valid = "LATE"
			}

			if result.TimedOut {
				valid = "TIMEOUT"
			}

			t.Logf("%-8d %-8s %-10s %-10s %-10s %-9.0f %-10.0f %-9.3f %s",
				rate, window,
				result.EndToEnd.P50.Round(time.Microsecond),
				result.EndToEnd.P99.Round(time.Microsecond),
				result.EndToEnd.P999.Round(time.Microsecond),
				result.MeanBatchSize,
				result.DownstreamPerSec,
				result.AllocsPerItem,
				valid)
		}

		t.Logf("")
	}
}

// TestDefaultWindowUnderSustainedOverload records what each window does when the
// downstream cannot keep up.
//
// This is the check that stops the decision being made on latency alone. A smaller
// window must not make overload worse: if it increased queue depth or downstream
// call rate under saturation, it would trade a latency win for a stability loss.
func TestDefaultWindowUnderSustainedOverload(t *testing.T) {
	if os.Getenv("SCENARIO_MATRIX") == "" {
		t.Skip("set SCENARIO_MATRIX=1 to run the decision matrix")
	}

	const (
		// A 2ms processor retires far less than this, so every window is saturated.
		rate        = 200_000
		serviceTime = 2 * time.Millisecond
		duration    = 500 * time.Millisecond
	)

	t.Logf("environment: %s", scenario.CurrentEnvironment())
	t.Logf("offered=%d/s processor=%s (deliberately saturated)", rate, serviceTime)
	t.Logf("")
	t.Logf("%-8s %-12s %-12s %-12s %-11s %-10s %s",
		"window", "accepted/s", "completed/s", "pending_peak", "heap_peak", "calls/s", "batch")

	for _, window := range []time.Duration{
		time.Millisecond,
		5 * time.Millisecond,
		10 * time.Millisecond,
		100 * time.Millisecond,
		time.Second,
	} {
		result := scenario.Run(scenario.Config{
			Name:           "overload",
			BatchSize:      1_000,
			BatchInterval:  window,
			Arrival:        scenario.Steady(rate, duration),
			Processor:      scenario.FixedProcessor(serviceTime),
			Producers:      runtime.NumCPU(),
			LatenessBudget: time.Second,
		})

		t.Logf("%-8s %-12.0f %-12.0f %-12d %-11s %-10.0f %.0f",
			window,
			result.AcceptedRate,
			result.CompletedRate,
			result.PendingPeak,
			formatMB(result.HeapHighWater),
			result.DownstreamPerSec,
			result.MeanBatchSize)
	}
}

// TestDefaultWindowUnderBursts records burst absorption and recovery, which a steady
// arrival rate cannot show.
//
// A service that is idle between spikes is exactly where a small window looks worst
// on paper: each burst has to be coalesced from a standing start.
func TestDefaultWindowUnderBursts(t *testing.T) {
	if os.Getenv("SCENARIO_MATRIX") == "" {
		t.Skip("set SCENARIO_MATRIX=1 to run the decision matrix")
	}

	t.Logf("environment: %s", scenario.CurrentEnvironment())
	t.Logf("pattern: 50k/s for 100ms, idle 100ms, x5")
	t.Logf("")
	t.Logf("%-8s %-10s %-10s %-9s %-10s %s",
		"window", "p50", "p99", "batch", "calls/s", "valid")

	for _, window := range []time.Duration{
		time.Millisecond,
		5 * time.Millisecond,
		10 * time.Millisecond,
		100 * time.Millisecond,
		time.Second,
	} {
		result := scenario.Run(scenario.Config{
			Name:           "burst",
			BatchSize:      1_000,
			BatchInterval:  window,
			Arrival:        scenario.Burst(50_000, 100*time.Millisecond, 100*time.Millisecond, 5),
			Processor:      scenario.NoOpProcessor(),
			Producers:      runtime.NumCPU(),
			LatenessBudget: 5 * time.Millisecond,
		})

		valid := "yes"
		if !result.LatenessValid {
			valid = "LATE"
		}

		t.Logf("%-8s %-10s %-10s %-9.0f %-10.0f %s",
			window,
			result.EndToEnd.P50.Round(time.Microsecond),
			result.EndToEnd.P99.Round(time.Microsecond),
			result.MeanBatchSize,
			result.DownstreamPerSec,
			valid)
	}
}

// TestDefaultWindowCoalescingHoldsAtRate is the one assertion in this file.
//
// The plan's sizing rule is that expected batch size is approximately
// arrival rate x window. If that relationship does not hold, the recommendation in
// the tuning guide is wrong and the decision record should not be trusted.
//
// Asserted loosely: the point is that the rule predicts the right order of
// magnitude, not that scheduling is exact on a shared machine.
func TestDefaultWindowCoalescingHoldsAtRate(t *testing.T) {
	t.Parallel()

	if testing.Short() {
		t.Skip("timing-sensitive; runs in the full suite only")
	}

	const (
		rate     = 10_000
		window   = 10 * time.Millisecond
		duration = time.Second
	)

	result := scenario.Run(scenario.Config{
		Name:           "coalescing-rule",
		BatchSize:      10_000, // never size-triggered: isolate the timer
		BatchInterval:  window,
		Arrival:        scenario.Steady(rate, duration),
		Processor:      scenario.NoOpProcessor(),
		Producers:      runtime.NumCPU(),
		LatenessBudget: 5 * time.Millisecond,
	})

	// rate x window = 10_000/s x 10ms = 100 items.
	predicted := float64(rate) * window.Seconds()

	t.Logf("predicted mean batch %.0f, measured %.0f (batches=%d, calls/s=%.0f)",
		predicted, result.MeanBatchSize, result.Batches, result.DownstreamPerSec)

	require.False(t, result.TimedOut, "the run must complete")

	// Half to double the prediction. Scheduling jitter on a shared machine moves
	// this around; an order-of-magnitude miss would mean the rule is wrong.
	require.Greater(t, result.MeanBatchSize, predicted*0.5,
		"measured batch size far below rate x window: the sizing rule in the tuning "+
			"guide would be misleading")
	require.Less(t, result.MeanBatchSize, predicted*2.0,
		"measured batch size far above rate x window: the sizing rule in the tuning "+
			"guide would be misleading")
}

func formatMB(bytes uint64) string {
	const mb = 1 << 20

	return strconv.FormatUint(bytes/mb, 10) + "MB"
}
