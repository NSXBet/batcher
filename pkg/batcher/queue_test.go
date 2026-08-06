package batcher

import (
	"context"
	"testing"

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
