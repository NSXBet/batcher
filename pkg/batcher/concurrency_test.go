package batcher_test

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NSXBet/batcher/pkg/batcher"
	"github.com/stretchr/testify/require"
)

// Concurrency configuration and the n=1 ordering contract.
//
// These tests pin what a caller is entitled to assume at the default
// concurrency, and pin that raising concurrency cannot happen by accident. The
// guarantees are only worth documenting if something enforces them, because they
// currently hold by construction and a later refactor could remove them silently.

// TestDefaultConcurrencyIsOne pins the default.
//
// It was 3 before this milestone, but read nowhere, so observed parallelism was
// always 1. Changing the constant to 1 keeps existing callers on the behaviour
// they already had; shipping 3 as a live value would have parallelised every
// current user's processor without asking.
func TestDefaultConcurrencyIsOne(t *testing.T) {
	t.Parallel()

	require.Equal(t, 1, batcher.DefaultConcurrency)

	b := batcher.New(batcher.WithProcessor(batcher.NoOpProcessor[int]))

	t.Cleanup(func() { _ = b.Close() })

	require.Equal(t, 1, b.Config().Concurrency,
		"a batcher built with no options must be serial")
}

// TestConcurrencyAboveOneRequiresAcknowledgement pins the gate.
//
// The acknowledgement carries no behaviour of its own; it exists so that giving
// up ordering and processor mutual exclusion is stated in the code that does it.
// Failing at construction means the mistake surfaces on the first test run rather
// than as a rare production data race.
func TestConcurrencyAboveOneRequiresAcknowledgement(t *testing.T) {
	t.Parallel()

	require.PanicsWithValue(t,
		"batcher: WithConcurrency(3) requires WithoutOrderedProcessing(). "+
			"Concurrent processing gives up cross-batch ordering and lets the "+
			"processor be invoked concurrently, so the processor must be "+
			"goroutine-safe. Add WithoutOrderedProcessing() to acknowledge this, "+
			"or keep WithConcurrency(1).",
		func() {
			batcher.New(
				batcher.WithProcessor(batcher.NoOpProcessor[int]),
				batcher.WithConcurrency[int](3),
			)
		},
		"WithConcurrency(n>1) without the acknowledgement must panic at construction")
}

// TestConcurrencyOneNeedsNoAcknowledgement pins that the gate does not get in the
// way of the safe configuration.
func TestConcurrencyOneNeedsNoAcknowledgement(t *testing.T) {
	t.Parallel()

	require.NotPanics(t, func() {
		b := batcher.New(
			batcher.WithProcessor(batcher.NoOpProcessor[int]),
			batcher.WithConcurrency[int](1),
		)

		_ = b.Close()
	}, "WithConcurrency(1) surrenders nothing, so it must not require the gate")
}

// TestAcknowledgedConcurrencyIsAccepted pins that the acknowledged combination
// constructs. Milestone 3.2 makes n>1 actually parallel; here it only has to be
// a legal configuration.
func TestAcknowledgedConcurrencyIsAccepted(t *testing.T) {
	t.Parallel()

	require.NotPanics(t, func() {
		b := batcher.New(
			batcher.WithProcessor(batcher.NoOpProcessor[int]),
			batcher.WithConcurrency[int](4),
			batcher.WithoutOrderedProcessing[int](),
		)

		t.Cleanup(func() { _ = b.Close() })

		require.Equal(t, 4, b.Config().Concurrency)
	})
}

// TestConcurrencyIsClampedToAtLeastOne pins that a nonsensical value degrades to
// the safe default rather than producing a batcher with no processing capacity.
func TestConcurrencyIsClampedToAtLeastOne(t *testing.T) {
	t.Parallel()

	for _, n := range []int{0, -1, -100} {
		b := batcher.New(
			batcher.WithProcessor(batcher.NoOpProcessor[int]),
			batcher.WithConcurrency[int](n),
		)

		require.Equal(t, 1, b.Config().Concurrency,
			"WithConcurrency(%d) must clamp to 1, not disable processing", n)

		require.NoError(t, b.Close())
	}
}

// TestSerialProcessingPreservesPublicationOrder pins FIFO at n=1 across all three
// flush triggers in one run: size-triggered batches, a timer-triggered batch, and
// the final batch flushed by shutdown.
//
// Ordering is defined by publication order from a single producer. That is the
// authoritative point because concurrent producers can reserve before they
// publish, so reservation order and queue order are not the same thing.
func TestSerialProcessingPreservesPublicationOrder(t *testing.T) {
	t.Parallel()

	const (
		batchSize = 4
		// Two full batches, then a gap for a timer flush, then a partial batch that
		// only shutdown can flush.
		firstBurst  = batchSize * 2
		timerBatch  = 3
		finalBatch  = 2
		totalItems  = firstBurst + timerBatch + finalBatch
		interval    = 40 * time.Millisecond
		flushMargin = 4 * interval
	)

	var (
		mu       sync.Mutex
		observed []string
	)

	b := batcher.New(
		batcher.WithBatchSize[string](batchSize),
		batcher.WithBatchInterval[string](interval),
		batcher.WithProcessor(func(items []string) error {
			mu.Lock()
			observed = append(observed, items...)
			mu.Unlock()

			return nil
		}),
	)

	expected := make([]string, 0, totalItems)

	publish := func(count int) {
		for range count {
			key := fmt.Sprintf("item-%03d", len(expected))
			expected = append(expected, key)
			b.Add(key)
		}
	}

	publish(firstBurst)

	// Let the size-triggered batches drain, then publish a batch that can only
	// leave on the timer.
	require.NoError(t, b.Join(5*time.Second))
	publish(timerBatch)

	time.Sleep(flushMargin)
	require.NoError(t, b.Join(5*time.Second))

	// The remainder is flushed by shutdown.
	publish(finalBatch)
	require.NoError(t, b.Close())

	mu.Lock()
	defer mu.Unlock()

	require.Equal(t, expected, observed,
		"at n=1 a single producer's items must be processed in publication order "+
			"across size, timer, and shutdown flushes")
}

// TestSerialProcessingNeverInvokesProcessorConcurrently pins processor mutual
// exclusion at n=1, using a detector inside the processor rather than inferring
// it from timing.
//
// This is the guarantee that lets a processor keep unsynchronised state, so it
// must be enforced rather than assumed.
func TestSerialProcessingNeverInvokesProcessorConcurrently(t *testing.T) {
	t.Parallel()

	var (
		active     atomic.Int64
		maxActive  atomic.Int64
		invocation atomic.Int64
	)

	b := batcher.New(
		batcher.WithBatchSize[int](1), // one batch per item: maximum opportunity to overlap
		batcher.WithBatchInterval[int](time.Millisecond),
		batcher.WithProcessor(func([]int) error {
			current := active.Add(1)

			for {
				observed := maxActive.Load()
				if current <= observed || maxActive.CompareAndSwap(observed, current) {
					break
				}
			}

			// Hold the processor open so any concurrent invocation would overlap.
			time.Sleep(time.Millisecond)

			invocation.Add(1)
			active.Add(-1)

			return nil
		}),
	)

	const items = 60

	for i := range items {
		b.Add(i)
	}

	require.NoError(t, b.Close())

	require.Equal(t, int64(items), invocation.Load(),
		"every item must be processed")
	require.Equal(t, int64(1), maxActive.Load(),
		"at n=1 the processor must never be invoked concurrently")
}

// TestConcurrentProducersPreservePerProducerOrder pins the ordering guarantee that
// actually holds with several producers, and deliberately does not claim more.
//
// Go does not order concurrent channel sends, so there is no total order across
// producers to preserve. Asserting one would encode a guarantee the library cannot
// make and would fail intermittently. What must hold is that each producer's own
// subsequence stays in order.
func TestConcurrentProducersPreservePerProducerOrder(t *testing.T) {
	t.Parallel()

	const (
		producers   = 4
		perProducer = 50
	)

	var (
		mu       sync.Mutex
		observed []string
	)

	b := batcher.New(
		batcher.WithBatchSize[string](8),
		batcher.WithBatchInterval[string](2*time.Millisecond),
		batcher.WithProcessor(func(items []string) error {
			mu.Lock()
			observed = append(observed, items...)
			mu.Unlock()

			return nil
		}),
	)

	var wg sync.WaitGroup

	for p := range producers {
		wg.Add(1)

		go func(producer int) {
			defer wg.Done()

			for i := range perProducer {
				b.Add(fmt.Sprintf("p%d-%03d", producer, i))
			}
		}(p)
	}

	wg.Wait()

	require.NoError(t, b.Close())

	mu.Lock()
	defer mu.Unlock()

	require.Len(t, observed, producers*perProducer, "no item may be lost")

	// Extract each producer's subsequence in observed order and require it to be
	// ascending. Cross-producer interleaving is unconstrained by design.
	perProducerSeen := make(map[string][]string, producers)

	for _, key := range observed {
		producer := key[:2]
		perProducerSeen[producer] = append(perProducerSeen[producer], key)
	}

	require.Len(t, perProducerSeen, producers)

	for producer, seen := range perProducerSeen {
		expected := make([]string, perProducer)
		for i := range expected {
			expected[i] = fmt.Sprintf("%s-%03d", producer, i)
		}

		require.Equal(t, expected, seen,
			"producer %s: its own items must stay in publication order", producer)
	}
}
