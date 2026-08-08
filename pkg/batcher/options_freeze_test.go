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
