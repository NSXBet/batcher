package batcher_test

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NSXBet/batcher/internal/test"
	"github.com/NSXBet/batcher/pkg/batcher"
	"github.com/stretchr/testify/require"
)

// TestOptionsAreFrozenAfterNew pins the construction-time option contract.
//
// Option[T] is a callable function, so callers can retain one and invoke it after
// New returns. Before this test, that was a data race: WithProcessor wrote the live
// config while process() read ProcessorFunc, and the other options could similarly
// change batching semantics mid-run. Configuration was never coherent at runtime,
// so every post-New option application is deliberately a no-op.
//
// Run under -race: this fails if a future option bypasses configFrozen and writes a
// live field again.
func TestOptionsAreFrozenAfterNew(t *testing.T) {
	t.Parallel()

	var (
		originalCalls atomic.Int64
		mutatedCalls  atomic.Int64
	)

	original := batcher.Processor[test.BatchItem](func([]test.BatchItem) error {
		originalCalls.Add(1)

		return nil
	})

	mutated := batcher.Processor[test.BatchItem](func([]test.BatchItem) error {
		mutatedCalls.Add(1)

		return nil
	})

	b := batcher.New(
		batcher.WithBatchSize[test.BatchItem](1),
		batcher.WithBatchInterval[test.BatchItem](time.Millisecond),
		batcher.WithProcessor(original),
	)

	defer func() { require.NoError(t, b.Close()) }()

	// Apply every mutable option concurrently with active processing. They must all
	// become no-ops after New; the original config remains the one run() snapshots.
	var wg sync.WaitGroup

	for range 4 {
		wg.Add(1)

		go func() {
			defer wg.Done()

			for range 200 {
				batcher.WithProcessor(mutated)(b)
				batcher.WithBatchSize[test.BatchItem](99)(b)
				batcher.WithBatchInterval[test.BatchItem](time.Hour)(b)
				batcher.WithConcurrency[test.BatchItem](8)(b)
				batcher.WithMaxQueueSize[test.BatchItem](1)(b)
			}
		}()
	}

	for i := range 400 {
		b.Add(test.BatchItem{Key: string(rune('a' + i%26))})
	}

	wg.Wait()

	require.NoError(t, b.Join(10*time.Second))

	config := b.Config()

	require.Equal(t, 1, config.BatchSize)
	require.Equal(t, time.Millisecond, config.BatchInterval)
	require.Equal(t, 1, config.Concurrency)
	require.Equal(t, 0, config.MaxQueueSize)
	require.Equal(t, int64(400), originalCalls.Load(),
		"processing must keep using the construction-time processor")
	require.Zero(t, mutatedCalls.Load(),
		"a post-start WithProcessor must not replace the original")
}

// TestEveryOptionRespectsTheFreeze covers the frozen-config early return in each
// option individually.
//
// TestOptionsAreFrozenAfterNew proves the guard works under concurrent load, but it
// exercises only the options it applies. This table walks every option, so adding a
// new one without the guard shows up as a coverage and assertion gap rather than as
// a data race discovered later.
func TestEveryOptionRespectsTheFreeze(t *testing.T) {
	t.Parallel()

	b := batcher.New(
		batcher.WithBatchSize[test.BatchItem](64),
		batcher.WithBatchInterval[test.BatchItem](7*time.Millisecond),
		batcher.WithMaxQueueSize[test.BatchItem](128),
		batcher.WithCloseGrace[test.BatchItem](11*time.Second),
		batcher.WithErrorBufferSize[test.BatchItem](32),
		batcher.WithProcessor(batcher.NoOpProcessor[test.BatchItem]),
	)

	defer func() { require.NoError(t, b.Close()) }()

	before := b.Config()

	// Every option, applied post-construction, must be inert.
	for _, option := range []batcher.Option[test.BatchItem]{
		batcher.WithBatchSize[test.BatchItem](1),
		batcher.WithBatchInterval[test.BatchItem](time.Hour),
		batcher.WithMaxQueueSize[test.BatchItem](1),
		batcher.WithCloseGrace[test.BatchItem](time.Nanosecond),
		batcher.WithErrorBufferSize[test.BatchItem](1),
		batcher.WithConcurrency[test.BatchItem](16),
		batcher.WithoutOrderedProcessing[test.BatchItem](),
		batcher.WithSkipAutoStart[test.BatchItem](),
		batcher.WithProcessor(batcher.NoOpProcessor[test.BatchItem]),
	} {
		option(b)
	}

	after := b.Config()

	// Config contains a function field, which Go cannot compare for equality even
	// when it is the same function. Assert every scalar explicitly and prove the
	// processor remains callable below.
	require.Equal(t, before.SkipAutoStart, after.SkipAutoStart)
	require.Equal(t, before.BatchSize, after.BatchSize)
	require.Equal(t, before.BatchInterval, after.BatchInterval)
	require.Equal(t, before.Concurrency, after.Concurrency)
	require.Equal(t, before.MaxQueueSize, after.MaxQueueSize)
	require.Equal(t, before.CloseGrace, after.CloseGrace)
	require.Equal(t, before.ErrorBufferSize, after.ErrorBufferSize)
	require.Equal(t, before.UnorderedProcessingAcknowledged, after.UnorderedProcessingAcknowledged)
	require.NotNil(t, after.ProcessorFunc,
		"post-construction WithProcessor must not replace the original processor")
	require.NoError(t, after.ProcessorFunc(nil))
}
