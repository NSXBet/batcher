package batcher_test

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NSXBet/batcher/pkg/batcher"
	"github.com/stretchr/testify/require"
)

// Worker pool behaviour at acknowledged concurrency.
//
// These tests cover the properties that make concurrency safe rather than merely
// faster: that it actually removes head-of-line blocking, that it does not void
// the queue bound, that shutdown waits for active workers, and that workers do not
// leak.

func unorderedBatcher[T any](workers int, opts ...batcher.Option[T]) []batcher.Option[T] {
	return append(opts,
		batcher.WithConcurrency[T](workers),
		batcher.WithoutOrderedProcessing[T](),
	)
}

// TestConcurrentWorkersProcessBatchesInParallel is the point of the whole phase.
//
// At n=1 a slow processor bounds the effective batch interval, because one batch
// must finish before the next can start. This asserts that n>1 actually overlaps
// processor calls rather than merely being configured to.
func TestConcurrentWorkersProcessBatchesInParallel(t *testing.T) {
	t.Parallel()

	const (
		workers   = 4
		batchSize = 1
		items     = 8
	)

	var (
		active    atomic.Int64
		maxActive atomic.Int64
		done      sync.WaitGroup
	)

	done.Add(items)

	// Hold every worker inside the processor until all of them have arrived, so
	// overlap is proven rather than inferred from timing.
	release := make(chan struct{})
	arrived := make(chan struct{}, items)

	b := batcher.New(unorderedBatcher[int](workers,
		batcher.WithBatchSize[int](batchSize),
		batcher.WithBatchInterval[int](time.Millisecond),
		batcher.WithProcessor(func([]int) error {
			defer done.Done()

			current := active.Add(1)

			for {
				observed := maxActive.Load()
				if current <= observed || maxActive.CompareAndSwap(observed, current) {
					break
				}
			}

			arrived <- struct{}{}
			<-release

			active.Add(-1)

			return nil
		}),
	)...)

	for i := range items {
		b.Add(i)
	}

	// Wait for exactly `workers` concurrent invocations. If the pool were serial,
	// only one would ever arrive and this would time out.
	deadline := time.After(10 * time.Second)

	for range workers {
		select {
		case <-arrived:
		case <-deadline:
			t.Fatalf("only %d concurrent processor invocations; expected %d",
				active.Load(), workers)
		}
	}

	require.Equal(t, int64(workers), maxActive.Load(),
		"n=%d must invoke the processor %d times concurrently", workers, workers)

	close(release)
	done.Wait()

	require.NoError(t, b.Close())

	stats := b.Stats()
	require.Equal(t, uint64(items), stats.Completed)
	require.Zero(t, stats.Pending)
	require.Zero(t, stats.InFlight, "no work may remain in flight after shutdown")
}

// TestConcurrencyPreservesOrderWithinEachBatch pins the guarantee that survives
// concurrency. Cross-batch order is given up; within a batch, items keep
// publication order, which is what lets a processor rely on its slice.
func TestConcurrencyPreservesOrderWithinEachBatch(t *testing.T) {
	t.Parallel()

	const (
		workers   = 4
		batchSize = 10
		batches   = 20
	)

	var (
		mu        sync.Mutex
		disorders int
	)

	b := batcher.New(unorderedBatcher[int](workers,
		batcher.WithBatchSize[int](batchSize),
		batcher.WithBatchInterval[int](5*time.Millisecond),
		batcher.WithProcessor(func(items []int) error {
			for i := 1; i < len(items); i++ {
				if items[i] < items[i-1] {
					mu.Lock()
					disorders++
					mu.Unlock()
				}
			}

			return nil
		}),
	)...)

	for i := range batchSize * batches {
		b.Add(i)
	}

	require.NoError(t, b.Close())

	mu.Lock()
	defer mu.Unlock()

	require.Zero(t, disorders,
		"items must retain publication order within each batch even at n>1")
}

// TestUnbufferedDispatchKeepsAcceptedWorkBounded pins the capacity contract.
//
// A buffered dispatch channel would hold batches nobody accounted for, silently
// voiding MaxQueueSize. The bound is
// MaxQueueSize + BatchSize + (n × BatchSize) + PublishersInGate, and the test
// records PublishersInGate separately so the terms stay distinguishable.
func TestUnbufferedDispatchKeepsAcceptedWorkBounded(t *testing.T) {
	t.Parallel()

	const (
		workers      = 3
		batchSize    = 4
		maxQueueSize = 8
		parked       = 5
	)

	// Closed once, at the end, to release both the workers and the parked
	// publishers.
	release := make(chan struct{})

	b := batcher.New(unorderedBatcher[int](workers,
		batcher.WithBatchSize[int](batchSize),
		batcher.WithBatchInterval[int](time.Millisecond),
		batcher.WithMaxQueueSize[int](maxQueueSize),
		batcher.WithProcessor(func([]int) error {
			<-release

			return nil
		}),
	)...)

	// Park publishers deliberately: each one blocks on a full bounded queue and is
	// therefore a caller goroutine Batcher cannot bound, only report.
	var wg sync.WaitGroup

	for i := range parked {
		wg.Add(1)

		go func(v int) {
			defer wg.Done()

			_ = b.Enqueue(context.Background(), v)
		}(i)
	}

	// Saturate until admission starts refusing, so the queue is genuinely full.
	for attempt := range 1_000 {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		err := b.Enqueue(ctx, attempt)

		cancel()

		if err != nil {
			break
		}
	}

	limit := int64(maxQueueSize + batchSize + workers*batchSize + parked)

	maxObservedGate := int64(0)

	for range 50 {
		stats := b.Stats()

		if stats.PublishersInGate > maxObservedGate {
			maxObservedGate = stats.PublishersInGate
		}

		require.LessOrEqual(t, stats.Pending, limit,
			"accepted-but-unfinished work must stay within "+
				"MaxQueueSize + BatchSize + n*BatchSize + PublishersInGate = %d "+
				"(pending=%d queued=%d inflight=%d gate=%d)",
			limit, stats.Pending, stats.Queued, stats.InFlight, stats.PublishersInGate)

		time.Sleep(2 * time.Millisecond)
	}

	require.LessOrEqual(t, maxObservedGate, int64(parked),
		"publishers in the gate must not exceed the goroutines intentionally parked")

	close(release)
	wg.Wait()
}

// TestShutdownWaitsForActiveWorkers pins that a batch inside a worker is drained
// rather than abandoned, which is the worker-mode version of the Phase 2 guarantee.
//
// This is also the case that distinguishes IntakePending from Pending: with a batch
// dispatched to a busy worker and nothing left in the queue, an aggregator that
// waited on Pending would block on a receive that can never arrive.
func TestShutdownWaitsForActiveWorkers(t *testing.T) {
	t.Parallel()

	const (
		workers   = 4
		batchSize = 2
		items     = 8
	)

	var processed atomic.Int64

	started := make(chan struct{}, items)
	release := make(chan struct{})

	b := batcher.New(unorderedBatcher[int](workers,
		batcher.WithBatchSize[int](batchSize),
		batcher.WithBatchInterval[int](time.Millisecond),
		batcher.WithProcessor(func(batch []int) error {
			started <- struct{}{}
			<-release
			processed.Add(int64(len(batch)))

			return nil
		}),
	)...)

	for i := range items {
		b.Add(i)
	}

	// Wait until work is genuinely in flight before shutting down.
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("the processor was never invoked")
	}

	require.Eventually(t, func() bool {
		return b.Stats().InFlight >= batchSize
	}, 5*time.Second, time.Millisecond,
		"Stats().InFlight must expose work currently inside a worker")

	shutdownDone := make(chan error, 1)

	go func() { shutdownDone <- b.Shutdown(context.Background()) }()

	// Shutdown must not complete while workers are held.
	select {
	case err := <-shutdownDone:
		t.Fatalf("shutdown completed while workers were still active: %v", err)
	case <-time.After(200 * time.Millisecond):
	}

	close(release)

	select {
	case err := <-shutdownDone:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("shutdown never completed after workers were released")
	}

	stats := b.Stats()

	require.Equal(t, int64(items), processed.Load(),
		"every accepted item must be processed, not discarded")
	require.Equal(t, uint64(items), stats.Accepted)
	require.Equal(t, stats.Accepted, stats.Completed+stats.Failed+stats.Panicked,
		"accounting invariant must hold at quiescence")
	require.Zero(t, stats.Pending)
	require.Zero(t, stats.IntakePending)
	require.Zero(t, stats.InFlight)
	require.True(t, b.IsClosed())
}

// TestPanicInWorkerDoesNotReduceConcurrency pins that recovery is scoped to the
// batch, not the worker. A panic that killed its worker would silently degrade
// throughput for the life of the process.
func TestPanicInWorkerDoesNotReduceConcurrency(t *testing.T) {
	t.Parallel()

	const workers = 3

	var calls atomic.Int64

	b := batcher.New(unorderedBatcher[int](workers,
		batcher.WithBatchSize[int](1),
		batcher.WithBatchInterval[int](time.Millisecond),
		batcher.WithProcessor(func([]int) error {
			// Panic on the first several batches: enough to kill every worker if
			// recovery were scoped to the loop instead of the batch.
			if calls.Add(1) <= int64(workers) {
				panic("poison batch")
			}

			return nil
		}),
	)...)

	go func() {
		for range b.Errors() {
		}
	}()

	const items = 40

	for i := range items {
		b.Add(i)
	}

	require.NoError(t, b.Close())

	stats := b.Stats()

	require.Equal(t, uint64(workers), stats.Panicked)
	require.Equal(t, uint64(items)-uint64(workers), stats.Completed,
		"batches after the panics must still be processed by surviving workers")
	require.Zero(t, stats.Pending, "a panic must not strand a drain obligation")
}

// TestWorkerGoroutineBudget pins the per-batcher goroutine cost across states.
//
// Callers create one batcher per tenant or key, so this is a real cost, and a leak
// would accumulate one set per batcher for the life of the process.
func TestWorkerGoroutineBudget(t *testing.T) {
	// Not parallel: goroutine counting requires no other test starting batchers.
	for _, workers := range []int{1, 2, 4} {
		baseline := settledGoroutines()

		b := batcher.New(unorderedBatcher[int](workers,
			batcher.WithBatchSize[int](100),
			batcher.WithBatchInterval[int](time.Hour), // stay idle
			batcher.WithProcessor(batcher.NoOpProcessor[int]),
		)...)

		// aggregator + one goroutine per worker.
		want := 1 + workers

		running := 0

		for range 100 {
			running = runtime.NumGoroutine()

			if running >= baseline+want {
				break
			}

			time.Sleep(20 * time.Millisecond)
		}

		require.InDelta(t, float64(want), float64(running-baseline), 0.5,
			"n=%d must own exactly %d goroutines (aggregator + %d workers); "+
				"measured %d over baseline %d",
			workers, want, workers, running-baseline, baseline)

		require.NoError(t, b.Close())

		settled := settledGoroutines()

		require.LessOrEqual(t, settled, baseline,
			"n=%d must return to the pre-construction goroutine count after Close "+
				"(baseline %d, settled %d)", workers, baseline, settled)
	}
}

// TestConcurrentWorkersUnderRace exercises worker completion against close, error
// publication, and concurrent enqueue simultaneously. It exists to be run under
// -race, where an unsynchronised worker interaction shows up as a reported race
// rather than as a rare wrong answer.
func TestConcurrentWorkersUnderRace(t *testing.T) {
	t.Parallel()

	const (
		trials    = 40
		workers   = 4
		producers = 4
		perActor  = 25
	)

	failure := errors.New("processor failed")

	for trial := range trials {
		b := batcher.New(unorderedBatcher[int](workers,
			batcher.WithBatchSize[int](8),
			batcher.WithBatchInterval[int](time.Millisecond),
			batcher.WithProcessor(func(items []int) error {
				// Alternate success and failure so error publication races worker
				// completion and channel close.
				if len(items)%2 == 0 {
					return failure
				}

				return nil
			}),
		)...)

		drained := make(chan struct{})

		go func() {
			defer close(drained)

			for range b.Errors() {
			}
		}()

		var wg sync.WaitGroup

		for range producers {
			wg.Add(1)

			go func() {
				defer wg.Done()

				for i := range perActor {
					b.Add(i)
				}
			}()
		}

		closed := make(chan error, 1)

		go func() { closed <- b.Close() }()

		wg.Wait()

		require.NoError(t, <-closed, "trial %d", trial)
		<-drained

		stats := b.Stats()

		require.Zero(t, stats.Pending, "trial %d", trial)
		require.Zero(t, stats.InFlight, "trial %d", trial)
		require.Equal(t, stats.Accepted, stats.Completed+stats.Failed+stats.Panicked,
			"trial %d: accounting invariant must hold", trial)
	}
}

// settledGoroutines waits for the goroutine count to stop changing, so a scheduler
// that has not yet reaped finished goroutines is not mistaken for a leak.
func settledGoroutines() int {
	previous := -1

	for range 100 {
		runtime.GC()
		time.Sleep(20 * time.Millisecond)

		current := runtime.NumGoroutine()
		if current == previous {
			return current
		}

		previous = current
	}

	return previous
}
