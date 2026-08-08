package batcher

import (
	"context"
	"sync"
)

// queue is Batcher's owned intake queue.
//
// It replaces golang.design/x/chann for two reasons, one correctness and one
// cost:
//
//   - A chann unbounded queue owns a relay goroutine whose only exit path closes
//     its own ingress channel — the very channel publishers send to. That makes
//     "never close the input" and "leak no goroutines" mutually exclusive.
//   - Owning the queue removes a channel hop and a goroutine handoff per item.
//
// One implementation serves both modes so there is a single code path to reason
// about: capacity 0 means unbounded, capacity N bounds queued items at N.
//
// The queue is NEVER closed. Shutdown is signalled out of band, by sealing
// admission and by intake accounting, so a publisher can never send on a closed
// channel. Readiness is published on capacity-1 signal channels rather than by
// handing out the storage itself, which is what lets the same type back both a
// bounded and an unbounded mode without a relay.
type queue[T any] struct {
	mu    sync.Mutex
	items []T
	// head is the read cursor. Popping advances head instead of reslicing from
	// the front, so the backing array is reused and steady-state enqueue does not
	// allocate. The array is only reset once fully drained.
	head     int
	capacity int

	// notEmpty and notFull are capacity-1 latches, not queues: a pending signal
	// means "there may be work" or "there may be space". Consumers must re-check
	// the real state after waking, which they do by calling pop or retrying push.
	notEmpty chan struct{}
	notFull  chan struct{}
}

// newQueue creates a queue. capacity <= 0 means unbounded.
func newQueue[T any](capacity int) *queue[T] {
	if capacity < 0 {
		capacity = 0
	}

	return &queue[T]{
		capacity: capacity,
		notEmpty: make(chan struct{}, 1),
		notFull:  make(chan struct{}, 1),
	}
}

// ready returns the readiness latch the aggregator selects on.
func (q *queue[T]) ready() <-chan struct{} {
	return q.notEmpty
}

// push appends an item, blocking only when a bounded queue is full.
//
// While blocked it selects on ctx and sealCh, so a parked publisher is always
// releasable: ctx for callers that supplied a deadline, sealCh for shutdown.
// Without the sealCh arm, a publisher parked on a full queue could never leave
// the admission gate and shutdown would deadlock.
//
// An unbounded queue never blocks, so ctx and sealCh are not consulted.
func (q *queue[T]) push(ctx context.Context, item T, sealCh <-chan struct{}) error {
	for {
		q.mu.Lock()

		if q.capacity == 0 || len(q.items)-q.head < q.capacity {
			q.items = append(q.items, item)
			q.mu.Unlock()

			signal(q.notEmpty)

			return nil
		}

		q.mu.Unlock()

		select {
		case <-q.notFull:
			// Space may be available; retry.
		case <-ctx.Done():
			return ctx.Err()
		case <-sealCh:
			return ErrClosing
		}
	}
}

// tryPush appends without ever blocking, reporting whether it fit.
func (q *queue[T]) tryPush(item T) bool {
	q.mu.Lock()

	if q.capacity != 0 && len(q.items)-q.head >= q.capacity {
		q.mu.Unlock()

		return false
	}

	q.items = append(q.items, item)
	q.mu.Unlock()

	signal(q.notEmpty)

	return true
}

// pop removes the oldest item, reporting false when the queue is empty.
func (q *queue[T]) pop() (T, bool) {
	var zero T

	q.mu.Lock()

	if q.head >= len(q.items) {
		// Fully drained: reset the cursor and keep the backing array, so the next
		// batch of pushes reuses capacity instead of allocating.
		if q.head > 0 {
			q.items = q.items[:0]
			q.head = 0
		}

		q.mu.Unlock()

		return zero, false
	}

	item := q.items[q.head]
	// Clear the slot so a popped item is not kept alive by the backing array.
	q.items[q.head] = zero
	q.head++

	remaining := len(q.items) - q.head

	// Reclaim the consumed prefix once it dominates the slice.
	//
	// Without this, the backing array grows with total throughput rather than with
	// queue depth: push always appends, and the full-drain reset above only fires
	// when the queue is observed completely empty. A steady producer that keeps even
	// one item resident never triggers it, so the array grows without bound.
	// Measured before this compaction: a queue holding a single item reached 219,136
	// slots after 200,000 pushes.
	//
	// Compacting when head >= remaining keeps the copy cost amortised O(1) per item,
	// because each compaction halves the live region's offset and moves at most as
	// many items as have been consumed since the last one.
	if q.head >= remaining {
		copy(q.items, q.items[q.head:])
		clear(q.items[remaining:])

		q.items = q.items[:remaining]
		q.head = 0
	}

	q.mu.Unlock()

	if remaining > 0 {
		// Keep the latch armed: the aggregator drains greedily, but re-arming means
		// a missed wakeup cannot strand items.
		signal(q.notEmpty)
	}

	signal(q.notFull)

	return item, true
}

// length reports queued items.
func (q *queue[T]) length() int {
	q.mu.Lock()
	defer q.mu.Unlock()

	return len(q.items) - q.head
}

// signal arms a capacity-1 latch without blocking. A latch that is already armed
// needs no second signal, because a waiter re-checks real state after waking.
func signal(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}
