package batcher_test

import (
	"sync"
	"testing"
	"time"

	"github.com/NSXBet/batcher/internal/test"
	"github.com/NSXBet/batcher/pkg/batcher"
	"github.com/stretchr/testify/require"
)

// Characterization tests for the batching engine.
//
// These were written against the rill-backed implementation and must keep
// passing, unchanged, after the aggregation loop is brought in-repo. They exist
// to pin observable behaviour that no other test asserts, so that replacing the
// engine cannot quietly change semantics.
//
// Each test states the behaviour it locks down and why it matters. Where the
// behaviour is a consequence of rill's design rather than a deliberate contract,
// that is called out, because those are exactly the places a reimplementation
// could drift.

// recordingProcessor captures the batches it receives, in order.
type recordingProcessor struct {
	mu      sync.Mutex
	batches [][]test.BatchItem
	flushed chan int
}

func newRecordingProcessor(capacity int) *recordingProcessor {
	return &recordingProcessor{flushed: make(chan int, capacity)}
}

func (r *recordingProcessor) process(items []test.BatchItem) error {
	// Copy: the engine owns the slice it passes in, and a future implementation
	// may reuse its backing array.
	batch := make([]test.BatchItem, len(items))
	copy(batch, items)

	r.mu.Lock()
	r.batches = append(r.batches, batch)
	r.mu.Unlock()

	r.flushed <- len(batch)

	return nil
}

func (r *recordingProcessor) snapshot() [][]test.BatchItem {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([][]test.BatchItem, len(r.batches))
	copy(out, r.batches)

	return out
}

// waitForBatches blocks until n batches have been processed, or fails.
func (r *recordingProcessor) waitForBatches(t *testing.T, n int, within time.Duration) {
	t.Helper()

	deadline := time.After(within)

	for range n {
		select {
		case <-r.flushed:
		case <-deadline:
			t.Fatalf("timed out waiting for %d batches; got %d", n, len(r.snapshot()))
		}
	}
}

// TestCharacterizeTimerStartsOnFirstItemOfBatch pins the single most surprising
// property of the engine: the batch interval is not a periodic flush tick. The
// timer starts when the first item enters an empty batch, so a sparse producer
// waits the full interval for every item.
//
// This is what makes the configured window a per-hop latency cost, and it is the
// behaviour the whole performance plan is built around. A reimplementation that
// used a periodic ticker instead would change latency for every user.
func TestCharacterizeTimerStartsOnFirstItemOfBatch(t *testing.T) {
	t.Parallel()

	const interval = 100 * time.Millisecond

	proc := newRecordingProcessor(8)

	b := batcher.New(
		batcher.WithBatchSize[test.BatchItem](1_000), // never size-triggered
		batcher.WithBatchInterval[test.BatchItem](interval),
		batcher.WithProcessor(proc.process),
	)

	t.Cleanup(func() { _ = b.Close() })

	// Let the batcher idle well past one interval with nothing queued.
	time.Sleep(3 * interval)

	require.Empty(t, proc.snapshot(),
		"an idle batcher must not emit empty batches; the timer is not a periodic tick")

	// The interval is measured from this item, not from any earlier tick.
	start := time.Now()
	b.Add(test.BatchItem{Key: "first"})

	proc.waitForBatches(t, 1, 5*interval)
	elapsed := time.Since(start)

	require.GreaterOrEqual(t, elapsed, interval/2,
		"flush must wait for the interval that started with the first item, not fire immediately")

	batches := proc.snapshot()
	require.Len(t, batches, 1)
	require.Len(t, batches[0], 1, "a timer flush emits exactly what accumulated")
}

// TestCharacterizeNoEmptyBatchesEverEmitted pins that the processor is never
// invoked with an empty slice, even across many idle intervals. User processors
// are entitled to assume len(items) > 0.
func TestCharacterizeNoEmptyBatchesEverEmitted(t *testing.T) {
	t.Parallel()

	const interval = 20 * time.Millisecond

	var invocations, emptyInvocations int

	var mu sync.Mutex

	b := batcher.New(
		batcher.WithBatchSize[test.BatchItem](10),
		batcher.WithBatchInterval[test.BatchItem](interval),
		batcher.WithProcessor(func(items []test.BatchItem) error {
			mu.Lock()
			defer mu.Unlock()

			invocations++

			if len(items) == 0 {
				emptyInvocations++
			}

			return nil
		}),
	)

	t.Cleanup(func() { _ = b.Close() })

	time.Sleep(10 * interval)

	mu.Lock()
	defer mu.Unlock()

	require.Zero(t, invocations, "an idle batcher must never invoke the processor")
	require.Zero(t, emptyInvocations, "the processor must never receive an empty batch")
}

// TestCharacterizeSizeTriggeredFlushIgnoresInterval pins that a full batch is
// emitted immediately rather than waiting for the timer, which is what makes
// batching efficient under load.
func TestCharacterizeSizeTriggeredFlushIgnoresInterval(t *testing.T) {
	t.Parallel()

	const (
		batchSize = 5
		interval  = 10 * time.Second // must not be reached
	)

	proc := newRecordingProcessor(8)

	b := batcher.New(
		batcher.WithBatchSize[test.BatchItem](batchSize),
		batcher.WithBatchInterval[test.BatchItem](interval),
		batcher.WithProcessor(proc.process),
	)

	t.Cleanup(func() { _ = b.Close() })

	for i := range batchSize {
		b.Add(test.BatchItem{Key: keyFor(i)})
	}

	proc.waitForBatches(t, 1, 2*time.Second)

	batches := proc.snapshot()
	require.Len(t, batches, 1)
	require.Len(t, batches[0], batchSize,
		"a full batch must flush on size without waiting for the interval")
}

// TestCharacterizeBatchBoundariesAndOrdering pins exact batch boundaries and
// ordering for a sequential producer: items are grouped in arrival order, in
// batches of exactly BatchSize until the remainder.
//
// Ordering here is a property of a single producer's program order. Concurrent
// producers have no defined relative order, and this test deliberately does not
// claim otherwise.
func TestCharacterizeBatchBoundariesAndOrdering(t *testing.T) {
	t.Parallel()

	const (
		batchSize = 4
		total     = 10 // two full batches plus a remainder of two
	)

	proc := newRecordingProcessor(8)

	b := batcher.New(
		batcher.WithBatchSize[test.BatchItem](batchSize),
		batcher.WithBatchInterval[test.BatchItem](25*time.Millisecond),
		batcher.WithProcessor(proc.process),
	)

	for i := range total {
		b.Add(test.BatchItem{Key: keyFor(i)})
	}

	require.NoError(t, b.Join(5*time.Second))
	require.NoError(t, b.Close())

	batches := proc.snapshot()

	var (
		sizes []int
		keys  []string
	)

	for _, batch := range batches {
		sizes = append(sizes, len(batch))

		for _, item := range batch {
			keys = append(keys, item.Key)
		}
	}

	require.Equal(t, []int{batchSize, batchSize, total - 2*batchSize}, sizes,
		"batches must be exactly BatchSize until the remainder")

	expected := make([]string, total)
	for i := range expected {
		expected[i] = keyFor(i)
	}

	require.Equal(t, expected, keys,
		"a single producer's items must be processed in arrival order")
}

// TestCharacterizePartialBatchFlushesOnClose pins that shutdown flushes an
// under-full batch rather than discarding it. This is the guarantee Phase 2.3
// later strengthens; pinning it now means the engine swap cannot regress it.
func TestCharacterizePartialBatchFlushesOnClose(t *testing.T) {
	t.Parallel()

	proc := newRecordingProcessor(4)

	b := batcher.New(
		batcher.WithBatchSize[test.BatchItem](1_000), // never size-triggered
		batcher.WithBatchInterval[test.BatchItem](25*time.Millisecond),
		batcher.WithProcessor(proc.process),
	)

	b.Add(test.BatchItem{Key: "only"})

	require.NoError(t, b.Close())

	batches := proc.snapshot()
	require.Len(t, batches, 1, "a partial batch must be flushed during shutdown")
	require.Len(t, batches[0], 1)
	require.Equal(t, "only", batches[0][0].Key)
}

// TestCharacterizeProcessorErrorsSurfaceOnErrorsChannel pins error propagation:
// a processor error is published without stopping the pipeline, and the item is
// still accounted for so Join can complete.
func TestCharacterizeProcessorErrorsSurfaceOnErrorsChannel(t *testing.T) {
	t.Parallel()

	failure := errCharacterization

	b := batcher.New(
		batcher.WithBatchSize[test.BatchItem](1),
		batcher.WithBatchInterval[test.BatchItem](10*time.Millisecond),
		batcher.WithProcessor(func([]test.BatchItem) error {
			return failure
		}),
	)

	t.Cleanup(func() { _ = b.Close() })

	b.Add(test.BatchItem{Key: "fails"})

	select {
	case err := <-b.Errors():
		require.ErrorIs(t, err, failure)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for a processor error")
	}

	// A failed batch still counts as completed work: Join must not hang.
	require.NoError(t, b.Join(5*time.Second),
		"a failed batch must still decrement pending work")

	// The pipeline must remain usable after an error.
	b.Add(test.BatchItem{Key: "also-fails"})

	select {
	case err := <-b.Errors():
		require.ErrorIs(t, err, failure)
	case <-time.After(5 * time.Second):
		t.Fatal("pipeline stopped processing after an error")
	}
}

// TestCharacterizeErrorsChannelClosesAfterShutdown pins that Errors() is closed
// on shutdown, so a consumer ranging over it terminates instead of blocking.
func TestCharacterizeErrorsChannelClosesAfterShutdown(t *testing.T) {
	t.Parallel()

	b := batcher.New(
		batcher.WithBatchSize[test.BatchItem](10),
		batcher.WithBatchInterval[test.BatchItem](10*time.Millisecond),
		batcher.WithProcessor(batcher.NoOpProcessor[test.BatchItem]),
	)

	drained := make(chan struct{})

	go func() {
		defer close(drained)

		for range b.Errors() {
		}
	}()

	require.NoError(t, b.Close())

	select {
	case <-drained:
	case <-time.After(5 * time.Second):
		t.Fatal("Errors() was not closed on shutdown; a consumer would block forever")
	}
}

// TestCharacterizeLenReflectsAcceptedButUnfinishedWork pins Len's meaning: it
// counts accepted work that has not finished processing, and returns to zero
// once the pipeline drains.
func TestCharacterizeLenReflectsAcceptedButUnfinishedWork(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	entered := make(chan struct{}, 1)

	b := batcher.New(
		batcher.WithBatchSize[test.BatchItem](2),
		batcher.WithBatchInterval[test.BatchItem](10*time.Millisecond),
		batcher.WithProcessor(func([]test.BatchItem) error {
			select {
			case entered <- struct{}{}:
			default:
			}

			<-release

			return nil
		}),
	)

	t.Cleanup(func() {
		close(release)
		_ = b.Close()
	})

	b.Add(test.BatchItem{Key: "a"})
	b.Add(test.BatchItem{Key: "b"})

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("processor was never invoked")
	}

	require.Positive(t, b.Len(),
		"work handed to a blocked processor must still count as pending")
}

func keyFor(i int) string {
	return "item-" + itoa(i)
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}

	var digits []byte

	for i > 0 {
		digits = append([]byte{byte('0' + i%10)}, digits...)
		i /= 10
	}

	return string(digits)
}

var errCharacterization = errCharacterizationType("characterization failure")

type errCharacterizationType string

func (e errCharacterizationType) Error() string { return string(e) }
