package scenario_test

import (
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/NSXBet/batcher/pkg/batcher"
	"github.com/NSXBet/batcher/test/scenario"
)

// TestScenarioMatrix runs the sweep that Phase 5 uses to choose a default batch
// window, and prints a report. It is opt-in because it deliberately takes
// minutes and produces measurements, not pass/fail assertions.
//
// Latency percentiles are reported, never gated: shared CI runners are too noisy
// for a p99 threshold to mean anything. Correctness and allocation counts are
// what the blocking lane checks.
//
// Run with:
//
//	SCENARIO_MATRIX=1 go test -run TestScenarioMatrix -timeout 30m ./test/scenario/
func TestScenarioMatrix(t *testing.T) {
	if os.Getenv("SCENARIO_MATRIX") == "" {
		t.Skip("set SCENARIO_MATRIX=1 to run the reporting matrix")
	}

	windows := []time.Duration{
		500 * time.Microsecond,
		time.Millisecond,
		2 * time.Millisecond,
		5 * time.Millisecond,
		10 * time.Millisecond,
		20 * time.Millisecond,
		50 * time.Millisecond,
		100 * time.Millisecond,
	}

	const (
		duration  = 2 * time.Second
		warmup    = 200 * time.Millisecond
		batchSize = 1_000
	)

	processors := []scenario.Processor{
		scenario.NoOpProcessor(),
		scenario.FixedProcessor(2 * time.Millisecond),
		scenario.JitteredProcessor(2*time.Millisecond, time.Millisecond),
		scenario.SlowOutlierProcessor(2*time.Millisecond, 50*time.Millisecond, 50),
	}

	var results []scenario.Result

	for _, proc := range processors {
		for _, window := range windows {
			for _, rate := range []int{1_000, 10_000, 50_000} {
				results = append(results, scenario.Run(scenario.Config{
					Name:           proc.Name + "/rate=" + itoa(rate),
					BatchSize:      batchSize,
					BatchInterval:  window,
					Arrival:        scenario.Steady(rate, duration),
					Processor:      proc,
					Warmup:         warmup,
					Seed:           1,
					LatenessBudget: 2 * time.Millisecond,
				}))
			}

			// Sparse arrivals isolate timer-driven flushing, which is where
			// per-hop latency comes from.
			results = append(results, scenario.Run(scenario.Config{
				Name:           proc.Name + "/sparse",
				BatchSize:      batchSize,
				BatchInterval:  window,
				Arrival:        scenario.Sparse(300, window*2),
				Processor:      proc,
				Seed:           1,
				LatenessBudget: 2 * time.Millisecond,
			}))
		}
	}

	// Phase 3 comparison: same slow downstream, serial versus acknowledged worker
	// pool. This is the evidence used to decide whether a 5-10ms window is worth
	// recommending for a service with processor-bound batches.
	for _, workers := range []int{1, 2, 4, 8} {
		options := []batcher.Option[scenario.Item](nil)
		if workers > 1 {
			options = []batcher.Option[scenario.Item]{
				batcher.WithConcurrency[scenario.Item](workers),
				batcher.WithoutOrderedProcessing[scenario.Item](),
			}
		}

		for _, window := range []time.Duration{time.Millisecond, 5 * time.Millisecond, 10 * time.Millisecond, 100 * time.Millisecond} {
			result := scenario.Run(scenario.Config{
				Name:           "concurrency/n=" + itoa(workers),
				BatchSize:      1_000,
				BatchInterval:  window,
				Arrival:        scenario.Steady(10_000, 2*time.Second),
				Processor:      scenario.FixedProcessor(20 * time.Millisecond),
				BatcherOptions: options,
				Producers:      runtime.NumCPU(),
				Warmup:         200 * time.Millisecond,
				Seed:           1,
				LatenessBudget: 2 * time.Millisecond,
			})

			if result.TimedOut {
				t.Errorf("concurrency/n=%d window=%s timed out; excluded from the report",
					workers, window)

				continue
			}

			results = append(results, result)
		}
	}

	if err := scenario.WriteReport(os.Stdout, results); err != nil {
		t.Fatalf("writing report: %v", err)
	}
}

func itoa(v int) string {
	if v >= 1_000 {
		return itoa(v/1_000) + "k"
	}

	digits := ""
	for v > 0 {
		digits = string(rune('0'+v%10)) + digits
		v /= 10
	}

	if digits == "" {
		return "0"
	}

	return digits
}
