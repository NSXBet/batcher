package batcher

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// Queue edge cases.
//
// These are package-internal because they exercise the queue directly. Driving them
// through the public API would require winning a race to observe the state, so a
// direct test is both deterministic and honest about what it covers.

// TestQueueRejectsWhenBoundedAndFull covers tryPush's rejection path, which is how a
// non-blocking Add refuses work in bounded mode.
//
// Reached only when the queue is exactly at capacity, so ordinary tests miss it.
func TestQueueRejectsWhenBoundedAndFull(t *testing.T) {
	t.Parallel()

	q := newQueue[int](2)

	require.True(t, q.tryPush(1))
	require.True(t, q.tryPush(2))
	require.False(t, q.tryPush(3),
		"tryPush must refuse rather than grow past the configured bound")
	require.Equal(t, 2, q.length())

	// Draining one slot must make room again, so a rejection is transient rather
	// than permanent.
	item, ok := q.pop()
	require.True(t, ok)
	require.Equal(t, 1, item)
	require.True(t, q.tryPush(3), "space must be reusable after a pop")
	require.Equal(t, 2, q.length())
}

// TestQueueUnboundedNeverRejects pins that the default mode has no capacity limit.
//
// The library's documented default is unbounded, so a spurious rejection here would
// silently drop accepted work in the configuration most callers use.
func TestQueueUnboundedNeverRejects(t *testing.T) {
	t.Parallel()

	q := newQueue[int](0)

	for i := range 10_000 {
		require.True(t, q.tryPush(i), "unbounded queue rejected at %d", i)
	}

	require.Equal(t, 10_000, q.length())
}

// TestNewQueueNormalisesNegativeCapacity pins that a negative bound means unbounded
// rather than a queue that rejects everything.
//
// WithMaxQueueSize clamps at the option layer, but the queue is constructed
// elsewhere too, so the invariant belongs here as well.
func TestNewQueueNormalisesNegativeCapacity(t *testing.T) {
	t.Parallel()

	q := newQueue[int](-5)

	require.Equal(t, 0, q.capacity, "a negative bound must normalise to unbounded")
	require.True(t, q.tryPush(1))
}

// TestQueuePopOnEmptyResetsCursor pins the full-drain reset.
//
// Without it the read cursor would keep advancing past a reused backing array. This
// is distinct from the compaction test: that one covers a partially drained queue,
// this one the fully drained case.
func TestQueuePopOnEmptyResetsCursor(t *testing.T) {
	t.Parallel()

	q := newQueue[int](0)
	sealCh := make(chan struct{})

	for i := range 4 {
		require.NoError(t, q.push(context.Background(), i, sealCh))
	}

	for range 4 {
		_, ok := q.pop()
		require.True(t, ok)
	}

	// The queue is now empty but head is still advanced; popping resets it.
	_, ok := q.pop()
	require.False(t, ok)

	q.mu.Lock()
	head, length := q.head, len(q.items)
	q.mu.Unlock()

	require.Zero(t, head, "a fully drained queue must reset its read cursor")
	require.Zero(t, length, "a fully drained queue must reset its length")

	// And it must still be usable afterwards.
	require.True(t, q.tryPush(99))

	item, ok := q.pop()
	require.True(t, ok)
	require.Equal(t, 99, item)
}

// TestQueuePushRespectsContextWhenFull pins that a blocking push honours the
// caller's deadline instead of parking indefinitely.
func TestQueuePushRespectsContextWhenFull(t *testing.T) {
	t.Parallel()

	q := newQueue[int](1)
	sealCh := make(chan struct{})

	require.NoError(t, q.push(context.Background(), 1, sealCh))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := q.push(ctx, 2, sealCh)

	require.ErrorIs(t, err, context.Canceled,
		"a full queue must release a blocked publisher when its context ends")
	require.Equal(t, 1, q.length(), "the refused item must not be queued")
}

// TestQueuePushReleasedBySeal pins the shutdown release path.
//
// This is what stops a publisher parked on a full queue from deadlocking shutdown:
// it can never leave the admission gate until this returns.
func TestQueuePushReleasedBySeal(t *testing.T) {
	t.Parallel()

	q := newQueue[int](1)
	sealCh := make(chan struct{})

	require.NoError(t, q.push(context.Background(), 1, sealCh))

	result := make(chan error, 1)

	go func() { result <- q.push(context.Background(), 2, sealCh) }()

	close(sealCh)

	require.ErrorIs(t, <-result, ErrClosing,
		"sealing must release a publisher parked on a full queue")
}

// TestCapacityEstimatorClampsBelowFloor pins behaviour for a BatchSize smaller than
// the capacity floor.
//
// A caller can legitimately set WithBatchSize(1), and capacity must never exceed
// BatchSize, so the floor cannot win that comparison.
func TestCapacityEstimatorClampsBelowFloor(t *testing.T) {
	t.Parallel()

	for _, batchSize := range []int{1, 2, 8} {
		est := newCapacityEstimator(batchSize)

		require.LessOrEqual(t, est.capacity(), batchSize,
			"batchSize=%d: capacity must never exceed the configured ceiling", batchSize)

		est.observe(batchSize)

		require.LessOrEqual(t, est.capacity(), batchSize,
			"batchSize=%d: capacity must stay within the ceiling after observation",
			batchSize)
	}
}

// TestCapacityEstimatorRejectsNonPositiveBatchSize pins that a nonsensical ceiling
// degrades to something usable rather than producing a zero-capacity slice forever.
func TestCapacityEstimatorRejectsNonPositiveBatchSize(t *testing.T) {
	t.Parallel()

	for _, batchSize := range []int{0, -1, -100} {
		est := newCapacityEstimator(batchSize)

		require.Positive(t, est.capacity(),
			"batchSize=%d must not yield a zero-capacity estimator", batchSize)
	}
}

// TestClampCapacityDirectly covers every branch of the helper. The estimator tests
// exercise it indirectly, but an indirect test can miss a branch when the estimator
// changes shape; this helper is the retention bound itself and deserves a direct
// table.
func TestClampCapacityDirectly(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		candidate int
		batchSize int
		want      int
	}{
		{"above ceiling", 2_000, 1_000, 1_000},
		{"below floor with small ceiling", 1, 8, 8},
		{"below floor with normal ceiling", 1, 1_000, capacityFloor},
		{"inside bounds", 128, 1_000, 128},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, clampCapacity(tc.candidate, tc.batchSize))
		})
	}
}
