package batcher

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestProcessingReadsOnlyTheRuntimeSnapshot pins that processing goroutines never
// read the mutable Config struct.
//
// Config is a plain struct and a caller can hold a reference to one, so any read of
// it from a worker is a data race by construction. run() previously snapshotted
// batch size, interval and worker count but still dereferenced
// b.config.ProcessorFunc on every batch, which made the rest of that snapshot
// pointless: one live field is enough to race.
//
// This test writes the Config field directly, bypassing the frozen-option guard
// entirely, because the guard is a separate defence. Under -race it fails if any
// processing path reads Config again.
func TestProcessingReadsOnlyTheRuntimeSnapshot(t *testing.T) {
	t.Parallel()

	var processed int

	b := New(
		WithBatchSize[int](1),
		WithBatchInterval[int](time.Millisecond),
		WithProcessor(func(items []int) error {
			processed += len(items)

			return nil
		}),
	)

	t.Cleanup(func() { _ = b.Close() })

	done := make(chan struct{})

	go func() {
		defer close(done)

		for range 300 {
			b.Add(1)
		}
	}()

	// A hostile or stale caller mutating configuration while work is in flight.
	for range 300 {
		b.config.ProcessorFunc = func([]int) error { return nil }
		b.config.BatchSize = 999
		b.config.BatchInterval = time.Hour
	}

	<-done

	require.NoError(t, b.Join(10*time.Second))

	// The pipeline must still be running the configuration it started with.
	require.Equal(t, 1, b.runtime.batchSize)
	require.Equal(t, time.Millisecond, b.runtime.batchInterval)
	require.Equal(t, 1, b.runtime.workers)
	require.NotNil(t, b.runtime.processor)
	require.Positive(t, processed,
		"the original processor must have run; a swapped-in one would not count here")
}

// TestRuntimeSnapshotNormalisesInvalidConfig pins the defaults applied when the
// snapshot is taken, so a zero or negative value cannot produce a pipeline with no
// capacity or no processor.
func TestRuntimeSnapshotNormalisesInvalidConfig(t *testing.T) {
	t.Parallel()

	b := New[int](WithSkipAutoStart[int]())

	t.Cleanup(func() { _ = b.Close() })

	// Defaults, not zero values.
	require.Positive(t, b.runtime.batchSize)
	require.GreaterOrEqual(t, b.runtime.workers, 1)
	require.NotNil(t, b.runtime.processor,
		"a nil processor must normalise to the no-op rather than panicking a worker")
}
