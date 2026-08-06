package batcher

import "testing"

// Capacity strategy comparison for Milestone 4.2.
//
// The aggregator reserves make([]T, 0, BatchSize) for every batch. When a batch
// closes on the timer holding far fewer items, the difference is pure waste. These
// benchmarks compare the current strategy against the recent-max estimator across
// the workloads the plan requires, including the ones that must NOT regress.
//
// EWMA-of-mean was rejected during planning because it regressed alternating
// traffic; recentMaxCapacity is benchmarked here against that same pattern to
// confirm the replacement does not repeat the mistake.

type benchItem struct {
	_ [64]byte
}

const benchBatchSize = 1_000

var benchSink []benchItem

// fullCapacity is today's strategy: always reserve BatchSize.
func fullCapacity(sizes []int) {
	for _, n := range sizes {
		batch := make([]benchItem, 0, benchBatchSize)

		for range n {
			batch = append(batch, benchItem{})
		}

		benchSink = batch
	}
}

// adaptive uses the recent-max estimator under test.
func adaptive(sizes []int) {
	est := newCapacityEstimator(benchBatchSize)

	for _, n := range sizes {
		batch := make([]benchItem, 0, est.capacity())

		for range n {
			batch = append(batch, benchItem{})
		}

		est.observe(len(batch))

		benchSink = batch
	}
}

func repeat(pattern []int, times int) []int {
	out := make([]int, 0, len(pattern)*times)
	for range times {
		out = append(out, pattern...)
	}

	return out
}

func benchBoth(b *testing.B, sizes []int) {
	b.Run("full", func(b *testing.B) {
		b.ReportAllocs()

		for range b.N {
			fullCapacity(sizes)
		}
	})

	b.Run("adaptive", func(b *testing.B) {
		b.ReportAllocs()

		for range b.N {
			adaptive(sizes)
		}
	})
}

// BenchmarkCapacitySteadySparse is the target workload: every batch holds one item.
func BenchmarkCapacitySteadySparse(b *testing.B) {
	benchBoth(b, repeat([]int{1}, 200))
}

// BenchmarkCapacitySmallBatches models a moderate rate closing on the timer.
func BenchmarkCapacitySmallBatches(b *testing.B) {
	benchBoth(b, repeat([]int{50}, 200))
}

// BenchmarkCapacityFullBatches is the control: reserved capacity is fully used, so
// adaptive must not do worse.
func BenchmarkCapacityFullBatches(b *testing.B) {
	benchBoth(b, repeat([]int{benchBatchSize}, 100))
}

// BenchmarkCapacityAlternating is the pattern that killed EWMA-of-mean.
func BenchmarkCapacityAlternating(b *testing.B) {
	benchBoth(b, repeat([]int{1, benchBatchSize}, 100))
}

// BenchmarkCapacityBurstAfterIdle models a long sparse period then a burst.
func BenchmarkCapacityBurstAfterIdle(b *testing.B) {
	sizes := append(repeat([]int{1}, 150), repeat([]int{benchBatchSize}, 50)...)
	benchBoth(b, sizes)
}

// BenchmarkCapacityBimodal models mostly small batches with regular large ones.
func BenchmarkCapacityBimodal(b *testing.B) {
	pattern := []int{20, 20, 20, 20, 20, 20, 20, 20, 20, 800}
	benchBoth(b, repeat(pattern, 20))
}
