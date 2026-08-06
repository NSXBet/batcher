package scenario_test

import (
	"os"
	"testing"
	"time"

	"github.com/NSXBet/batcher/pkg/batcher"
	"github.com/NSXBet/batcher/test/scenario"
	"github.com/stretchr/testify/require"
)

// concurrencyOptions builds the acknowledged worker-pool configuration. n=1 needs
// no options, which keeps the comparison honest: the serial case is the library's
// default rather than a specially configured mode.
func concurrencyOptions(workers int) []batcher.Option[scenario.Item] {
	if workers <= 1 {
		return nil
	}

	return []batcher.Option[scenario.Item]{
		batcher.WithConcurrency[scenario.Item](workers),
		batcher.WithoutOrderedProcessing[scenario.Item](),
	}
}

// TestConcurrencyRemovesSlowProcessorCoupling is the measurement the whole plan
// exists to justify.
//
// At n=1 a batch must finish before the next can start, so the effective flush
// interval is max(BatchInterval, processor duration) and a small window buys
// nothing. This asserts that acknowledged concurrency actually breaks that
// coupling under an open-loop load, rather than merely being configurable.
//
// The assertion is relative — n>1 must beat n=1 on the same scenario — because
// absolute latency depends on the host, and a fixed millisecond threshold would
// encode the machine it was written on.
func TestConcurrencyRemovesSlowProcessorCoupling(t *testing.T) {
	t.Parallel()

	if testing.Short() {
		t.Skip("timing-sensitive; runs in the full suite only")
	}

	const (
		window      = 5 * time.Millisecond
		serviceTime = 50 * time.Millisecond
		rate        = 10_000
		duration    = 1500 * time.Millisecond
	)

	run := func(workers int) scenario.Result {
		return scenario.Run(scenario.Config{
			Name:           "coupling",
			BatchSize:      100_000, // never size-triggered: isolate the timer
			BatchInterval:  window,
			Arrival:        scenario.Steady(rate, duration),
			Processor:      scenario.FixedProcessor(serviceTime),
			BatcherOptions: concurrencyOptions(workers),
			LatenessBudget: time.Second,
		})
	}

	serial := run(1)
	concurrent := run(8)

	t.Logf("n=1  p50=%-10s p99=%-10s mean_batch=%-7.0f downstream/s=%.0f",
		serial.EndToEnd.P50, serial.EndToEnd.P99,
		serial.MeanBatchSize, serial.DownstreamPerSec)
	t.Logf("n=8  p50=%-10s p99=%-10s mean_batch=%-7.0f downstream/s=%.0f",
		concurrent.EndToEnd.P50, concurrent.EndToEnd.P99,
		concurrent.MeanBatchSize, concurrent.DownstreamPerSec)

	require.False(t, serial.TimedOut, "serial run must complete")
	require.False(t, concurrent.TimedOut, "concurrent run must complete")

	require.Less(t, concurrent.EndToEnd.P50, serial.EndToEnd.P50,
		"with a %s processor, concurrency must reduce p50 latency: "+
			"at n=1 the effective interval is bounded by the processor, not the window",
		serviceTime)

	// Concurrency adds capacity rather than changing the batching rule, so more
	// batches leave per second and each is correspondingly smaller.
	require.Greater(t, concurrent.DownstreamPerSec, serial.DownstreamPerSec,
		"concurrency must raise the flush rate")
	require.Less(t, concurrent.MeanBatchSize, serial.MeanBatchSize,
		"a higher flush rate means less pooling per batch")
}

// TestSemanticDifferenceMatrix records how n=1 and n>1 differ under an identical
// slow processor, which Milestone 3.2 requires as documentation rather than as a
// pass/fail gate.
//
// Only the load-bearing contract is asserted: latency must improve monotonically
// enough that n=8 beats n=1. The rest is reported, because batch-size distribution
// and admission blocking depend on host scheduling.
func TestSemanticDifferenceMatrix(t *testing.T) {
	t.Parallel()

	if testing.Short() {
		t.Skip("timing-sensitive; runs in the full suite only")
	}

	const (
		window      = 5 * time.Millisecond
		serviceTime = 20 * time.Millisecond
		rate        = 10_000
		duration    = time.Second
	)

	type row struct {
		workers int
		result  scenario.Result
	}

	rows := make([]row, 0, 4)

	for _, workers := range []int{1, 2, 4, 8} {
		rows = append(rows, row{
			workers: workers,
			result: scenario.Run(scenario.Config{
				Name:           "matrix",
				BatchSize:      100_000,
				BatchInterval:  window,
				Arrival:        scenario.Steady(rate, duration),
				Processor:      scenario.FixedProcessor(serviceTime),
				BatcherOptions: concurrencyOptions(workers),
				LatenessBudget: time.Second,
			}),
		})
	}

	t.Logf("window=%s processor=%s arrival=%d/s", window, serviceTime, rate)
	t.Logf("%-4s %-11s %-11s %-11s %-12s %-12s %s",
		"n", "p50", "p99", "max", "mean_batch", "calls/s", "admission_p99")

	for _, r := range rows {
		t.Logf("%-4d %-11s %-11s %-11s %-12.0f %-12.0f %s",
			r.workers,
			r.result.EndToEnd.P50.Round(time.Microsecond),
			r.result.EndToEnd.P99.Round(time.Microsecond),
			r.result.EndToEnd.Max.Round(time.Microsecond),
			r.result.MeanBatchSize,
			r.result.DownstreamPerSec,
			r.result.AdmissionBlocking.P99.Round(time.Microsecond),
		)

		require.False(t, r.result.TimedOut, "n=%d run must complete", r.workers)
		require.Zero(t, r.result.RejectedCount,
			"n=%d: unbounded admission must accept everything", r.workers)
	}

	serial, highest := rows[0].result, rows[len(rows)-1].result

	require.Less(t, highest.EndToEnd.P50, serial.EndToEnd.P50,
		"n=%d must reduce p50 versus serial processing", rows[len(rows)-1].workers)
}

// TestConcurrencyDoesNotHelpWhenNotProcessorBound is the control for the claim
// above.
//
// If concurrency appeared to improve a window that was never processor-bound, the
// measurement would be suspect: the improvement must come from removing the
// processor bottleneck, not from perturbing the timer. With a 100ms window and a
// 20ms processor there is no bottleneck to remove, so latency should be
// essentially unchanged.
func TestConcurrencyDoesNotHelpWhenNotProcessorBound(t *testing.T) {
	t.Parallel()

	if testing.Short() {
		t.Skip("timing-sensitive; runs in the full suite only")
	}

	const (
		window      = 100 * time.Millisecond
		serviceTime = 20 * time.Millisecond
		rate        = 10_000
		duration    = time.Second
	)

	run := func(workers int) scenario.Result {
		return scenario.Run(scenario.Config{
			Name:           "not-bound",
			BatchSize:      100_000,
			BatchInterval:  window,
			Arrival:        scenario.Steady(rate, duration),
			Processor:      scenario.FixedProcessor(serviceTime),
			BatcherOptions: concurrencyOptions(workers),
			LatenessBudget: time.Second,
		})
	}

	serial := run(1)
	concurrent := run(4)

	t.Logf("n=1  p50=%s mean_batch=%.0f", serial.EndToEnd.P50, serial.MeanBatchSize)
	t.Logf("n=4  p50=%s mean_batch=%.0f", concurrent.EndToEnd.P50, concurrent.MeanBatchSize)

	// The window dominates, so both should sit near it. Allow a generous margin:
	// the point is that concurrency does not change the regime, not that timing is
	// exact on a shared runner.
	require.InDelta(t, serial.EndToEnd.P50.Seconds(), concurrent.EndToEnd.P50.Seconds(),
		(window / 2).Seconds(),
		"when the window already exceeds the processor, concurrency must not change "+
			"the latency regime; an improvement here would mean the batching rule "+
			"changed rather than the bottleneck being removed")
}

// TestConcurrencyMatrixReport prints the full concurrency sweep as an artifact for
// the Phase 5 default-window decision. Opt-in, because it is measurement rather
// than verification.
func TestConcurrencyMatrixReport(t *testing.T) {
	if os.Getenv("SCENARIO_MATRIX") == "" {
		t.Skip("set SCENARIO_MATRIX=1 to run the concurrency report")
	}

	var results []scenario.Result

	for _, workers := range []int{1, 2, 4, 8} {
		for _, window := range []time.Duration{
			time.Millisecond,
			5 * time.Millisecond,
			10 * time.Millisecond,
			100 * time.Millisecond,
		} {
			results = append(results, scenario.Run(scenario.Config{
				Name:           "n=" + itoa(workers),
				BatchSize:      1_000,
				BatchInterval:  window,
				Arrival:        scenario.Steady(10_000, 2*time.Second),
				Processor:      scenario.FixedProcessor(20 * time.Millisecond),
				BatcherOptions: concurrencyOptions(workers),
				Warmup:         200 * time.Millisecond,
				Seed:           1,
				LatenessBudget: 2 * time.Millisecond,
			}))
		}
	}

	require.NoError(t, scenario.WriteReport(os.Stdout, results))
}
