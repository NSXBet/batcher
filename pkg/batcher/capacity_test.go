package batcher

import "testing"

// Capacity estimator unit tests.
//
// These pin the properties the aggregator relies on, independently of timing: the
// bound that protects a retained slice, adaptation in both directions, and the
// behaviour on the two workloads that made the rejected EWMA estimator worse than
// doing nothing.

// TestCapacityNeverExceedsBatchSize pins the retention bound. A processor may keep
// its batch slice, so capacity must never exceed the configured ceiling; otherwise
// a retained slice could hold more than today's worst case.
func TestCapacityNeverExceedsBatchSize(t *testing.T) {
	t.Parallel()

	for _, batchSize := range []int{1, 7, 16, 100, 1_000} {
		est := newCapacityEstimator(batchSize)

		if got := est.capacity(); got > batchSize {
			t.Fatalf("batchSize=%d: initial capacity %d exceeds ceiling", batchSize, got)
		}

		// Feed sizes at and beyond the ceiling, including absurd ones.
		for _, size := range []int{0, 1, batchSize, batchSize * 4, batchSize * 1000} {
			est.observe(size)

			if got := est.capacity(); got > batchSize {
				t.Fatalf("batchSize=%d: capacity %d exceeds ceiling after observing %d",
					batchSize, got, size)
			}
		}
	}
}

// TestCapacityStartsAtBatchSize pins the pessimistic start.
//
// Starting small measured as a 2.2% allocation regression on size-triggered
// workloads, because the first full batch grew repeatedly before any evidence
// existed. The first batch therefore reserves the ceiling.
func TestCapacityStartsAtBatchSize(t *testing.T) {
	t.Parallel()

	est := newCapacityEstimator(1_000)

	if got := est.capacity(); got != 1_000 {
		t.Fatalf("initial capacity = %d, want 1000 (no evidence yet)", got)
	}
}

// TestCapacityAdaptsDownAfterFirstSparseBatch pins that a sparse workload stops
// reserving the ceiling immediately, rather than waiting a full evaluation window.
func TestCapacityAdaptsDownAfterFirstSparseBatch(t *testing.T) {
	t.Parallel()

	est := newCapacityEstimator(1_000)

	est.observe(1)

	got := est.capacity()

	if got >= 1_000 {
		t.Fatalf("capacity = %d after a 1-item batch, want well below 1000", got)
	}

	if got < capacityFloor {
		t.Fatalf("capacity = %d, want at least the floor %d", got, capacityFloor)
	}
}

// TestCapacityRisesImmediatelyOnLargerBatch pins that growth is not deferred to a
// window boundary. Under-allocating a large batch costs a reallocation right away,
// so the ceiling must react on the spot.
func TestCapacityRisesImmediatelyOnLargerBatch(t *testing.T) {
	t.Parallel()

	est := newCapacityEstimator(1_000)

	est.observe(1)

	small := est.capacity()

	est.observe(500)

	grown := est.capacity()

	if grown <= small {
		t.Fatalf("capacity did not rise: %d -> %d after observing 500", small, grown)
	}

	if grown < 500 {
		t.Fatalf("capacity = %d, want at least the observed 500", grown)
	}
}

// TestCapacityFullWorkloadStaysAtBatchSize pins the no-regression control.
// Repeated full batches keep the recent maximum at the configured ceiling, so the
// estimator must continue allocating BatchSize and never pay append growth.
func TestCapacityFullWorkloadStaysAtBatchSize(t *testing.T) {
	t.Parallel()

	est := newCapacityEstimator(1_000)

	for range capacityWindow * 2 {
		est.observe(1_000)

		if got := est.capacity(); got != 1_000 {
			t.Fatalf("capacity = %d after a full batch, want 1000", got)
		}
	}
}

// TestCapacityDecaysAfterWindow pins that a batcher which quiets down releases its
// oversized reservation, so a formerly busy batcher does not hold capacity forever.
func TestCapacityDecaysAfterWindow(t *testing.T) {
	t.Parallel()

	est := newCapacityEstimator(1_000)

	// Reach the ceiling under a busy workload.
	est.observe(1_000)

	busy := est.capacity()

	if busy != 1_000 {
		t.Fatalf("capacity = %d, want 1000 after a full batch", busy)
	}

	// Two full windows of sparse traffic.
	for range capacityWindow * 2 {
		est.observe(1)
	}

	quiet := est.capacity()

	if quiet >= busy {
		t.Fatalf("capacity did not decay: %d -> %d after sustained sparse traffic",
			busy, quiet)
	}
}

// TestCapacityHandlesAlternatingTraffic is the regression guard for the estimator
// that was rejected during planning.
//
// An EWMA of the mean allocated MORE than doing nothing on this pattern, because the
// mean sits between the two modes and every large batch grew from a too-small start.
// Tracking the maximum must keep capacity sized for the large mode.
func TestCapacityHandlesAlternatingTraffic(t *testing.T) {
	t.Parallel()

	est := newCapacityEstimator(1_000)

	for range 50 {
		est.observe(1)
		est.observe(1_000)
	}

	if got := est.capacity(); got != 1_000 {
		t.Fatalf("capacity = %d on alternating traffic, want 1000: a mean-based "+
			"estimator would sit between the modes and reallocate every large batch",
			got)
	}
}

// TestRoundUpPow2 pins the rounding helper, including its floor and idempotence.
func TestRoundUpPow2(t *testing.T) {
	t.Parallel()

	cases := map[int]int{
		-5: capacityFloor,
		0:  capacityFloor,
		1:  capacityFloor,
		16: capacityFloor,
		17: 32,
		32: 32,
		33: 64,
		63: 64,
		65: 128,
	}

	for input, want := range cases {
		if got := roundUpPow2(input); got != want {
			t.Errorf("roundUpPow2(%d) = %d, want %d", input, got, want)
		}
	}
}
