package batcher

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestQueueCompactsConsumedPrefix pins a memory bound that is easy to miss:
// queue backing storage must be proportional to queue depth, not the total number
// of items ever pushed.
//
// Before compaction, this test's one-item queue reached a capacity of 219,136
// after 200,000 push/pop cycles. The queue was never observed empty, so its
// full-drain reset never fired, push kept appending, and the backing array grew
// without bound even though the live depth stayed at one.
func TestQueueCompactsConsumedPrefix(t *testing.T) {
	t.Parallel()

	q := newQueue[int](0)
	sealCh := make(chan struct{})

	require.NoError(t, q.push(context.Background(), 0, sealCh))

	const cycles = 200_000

	for i := range cycles {
		require.NoError(t, q.push(context.Background(), i, sealCh))

		_, ok := q.pop()
		require.True(t, ok, "cycle %d: queue unexpectedly empty", i)
	}

	q.mu.Lock()
	defer q.mu.Unlock()

	require.Equal(t, 1, len(q.items)-q.head, "one item must remain live")
	require.LessOrEqual(t, cap(q.items), 8,
		"backing capacity must remain proportional to depth; got cap=%d after %d cycles",
		cap(q.items), cycles)
}

// TestQueueCompactionPreservesOrder verifies that moving the unconsumed suffix to
// the front does not reorder items.
func TestQueueCompactionPreservesOrder(t *testing.T) {
	t.Parallel()

	q := newQueue[int](0)
	sealCh := make(chan struct{})

	for i := range 100 {
		require.NoError(t, q.push(context.Background(), i, sealCh))
	}

	// Consume enough to trigger at least one prefix compaction while leaving a
	// substantial suffix behind.
	for i := range 75 {
		item, ok := q.pop()
		require.True(t, ok)
		require.Equal(t, i, item)
	}

	for i := 100; i < 150; i++ {
		require.NoError(t, q.push(context.Background(), i, sealCh))
	}

	for want := 75; want < 150; want++ {
		item, ok := q.pop()
		require.True(t, ok)
		require.Equal(t, want, item, "queue order must survive compaction")
	}

	_, ok := q.pop()
	require.False(t, ok)
}

// TestPopBatchPreservesFIFOAcrossCompaction is the equivalence check that matters:
// batch draining must be indistinguishable from repeated pop, including across the
// prefix compaction that moves the live region.
func TestPopBatchPreservesFIFOAcrossCompaction(t *testing.T) {
	t.Parallel()

	q := newQueue[int](0)
	sealCh := make(chan struct{})

	const total = 5_000

	var (
		got  []int
		next int
		buf  []int
	)

	// Interleave pushes and partial batch drains so head advances, the suffix is
	// compacted, and drains land mid-array rather than always at offset zero.
	for next < total {
		for range 7 {
			if next >= total {
				break
			}

			require.NoError(t, q.push(context.Background(), next, sealCh))

			next++
		}

		buf = q.popBatch(buf[:0], 3)
		got = append(got, buf...)
	}

	for {
		buf = q.popBatch(buf[:0], 64)
		if len(buf) == 0 {
			break
		}

		got = append(got, buf...)
	}

	require.Len(t, got, total)

	for i, v := range got {
		require.Equal(t, i, v, "batch draining must preserve FIFO order")
	}
}

// TestPopBatchRespectsMaxAndReportsEmpty pins the transfer cap, which is what bounds
// how long the queue lock is held. Without it a large backlog would move in one
// acquisition and starve producers for an unbounded time.
func TestPopBatchRespectsMaxAndReportsEmpty(t *testing.T) {
	t.Parallel()

	q := newQueue[int](0)
	sealCh := make(chan struct{})

	for i := range 10 {
		require.NoError(t, q.push(context.Background(), i, sealCh))
	}

	got := q.popBatch(nil, 4)
	require.Equal(t, []int{0, 1, 2, 3}, got, "at most max items may be transferred")

	got = q.popBatch(got[:0], 100)
	require.Equal(t, []int{4, 5, 6, 7, 8, 9}, got,
		"a max above the queued count must transfer only what is queued")

	require.Empty(t, q.popBatch(got[:0], 8), "an empty queue must transfer nothing")

	// A non-positive max is a caller bug; returning dst unchanged keeps it from
	// silently draining the queue or panicking.
	require.NoError(t, q.push(context.Background(), 42, sealCh))
	require.Empty(t, q.popBatch(nil, 0))
	require.Equal(t, 1, q.length(), "max<=0 must not consume items")
}

// TestPopBatchReusesCallerBuffer pins the allocation property the aggregator relies
// on: steady-state draining into a reused buffer must not allocate.
func TestPopBatchReusesCallerBuffer(t *testing.T) {
	// AllocsPerRun temporarily changes process-wide GC settings, so Go rejects it
	// from a parallel test.
	q := newQueue[int](0)
	sealCh := make(chan struct{})

	buf := make([]int, 0, 64)

	// Warm up so slice growth and the queue's own backing array settle.
	for range 64 {
		require.NoError(t, q.push(context.Background(), 1, sealCh))
	}

	buf = q.popBatch(buf[:0], 64)
	require.Len(t, buf, 64)

	allocs := testing.AllocsPerRun(200, func() {
		for range 32 {
			_ = q.push(context.Background(), 1, sealCh)
		}

		buf = q.popBatch(buf[:0], 32)
	})

	require.Zero(t, allocs,
		"draining into a reused buffer must not allocate; got %.1f allocs/op", allocs)
}

// TestPopBatchDrainedQueueReclaimsStorage pins that batch draining keeps the same
// memory bound as pop: backing storage proportional to depth, not to throughput.
func TestPopBatchDrainedQueueReclaimsStorage(t *testing.T) {
	t.Parallel()

	q := newQueue[int](0)
	sealCh := make(chan struct{})

	var buf []int

	require.NoError(t, q.push(context.Background(), 0, sealCh))

	const cycles = 50_000

	for i := range cycles {
		require.NoError(t, q.push(context.Background(), i, sealCh))

		buf = q.popBatch(buf[:0], 1)
		require.Len(t, buf, 1, "cycle %d: queue unexpectedly empty", i)
	}

	q.mu.Lock()
	defer q.mu.Unlock()

	require.Equal(t, 1, len(q.items)-q.head, "one item must remain live")
	require.LessOrEqual(t, cap(q.items), 8,
		"backing capacity must stay proportional to depth; got cap=%d after %d cycles",
		cap(q.items), cycles)
}

// TestPopBatchSignalsBoundedProducers pins the actual release mechanism: a batch
// transfer from a bounded queue arms notFull. push selects on this latch while full,
// so it is the signal that wakes parked publishers. Testing the channel directly is
// stronger than racing a goroutine against the drain: a publisher can be scheduled
// to retry after space exists even if the signal is accidentally removed.
func TestPopBatchSignalsBoundedProducers(t *testing.T) {
	t.Parallel()

	q := newQueue[int](4)
	sealCh := make(chan struct{})

	for i := range 4 {
		require.NoError(t, q.push(context.Background(), i, sealCh))
	}

	require.False(t, q.tryPush(99), "queue must be full")

	// Ensure the signal observed below comes from this popBatch, not an earlier pop.
	select {
	case <-q.notFull:
	default:
	}

	buf := q.popBatch(nil, 4)
	require.Len(t, buf, 4)

	select {
	case <-q.notFull:
		// The one-place latch is armed. A publisher parked in push can now retry.
	case <-time.After(time.Second):
		t.Fatal("popBatch did not arm notFull; bounded publishers would remain parked")
	}
}
