package batcher_test

import (
	"testing"
	"time"

	"github.com/NSXBet/batcher/internal/test"
	"github.com/NSXBet/batcher/pkg/batcher"
	"github.com/stretchr/testify/require"
)

func TestWithProcessor(t *testing.T) {
	called := false

	processor := batcher.Processor[test.BatchItem](func(_ []test.BatchItem) error {
		called = true

		return nil
	})

	// Options are construction-time configuration. Applying one after New starts
	// the batcher races its aggregation goroutine and was never a coherent runtime
	// reconfiguration API.
	b := batcher.New(batcher.WithProcessor(processor))
	defer func() { _ = b.Close() }()

	// Invoke the configured processor rather than checking its type. Every
	// Processor[T] has the same type, including the default no-op, so a type
	// assertion would pass even if WithProcessor had done nothing.
	require.NoError(t, b.Config().ProcessorFunc(nil))
	require.True(t, called, "the configured processor must replace the default")
}

func TestWithBatchSize(t *testing.T) {
	b := batcher.New(batcher.WithBatchSize[test.BatchItem](1000))
	defer func() { _ = b.Close() }()

	require.Equal(t, 1000, b.Config().BatchSize)

	t.Run("WithBatchSize - zero size", func(t *testing.T) {
		b := batcher.New(batcher.WithBatchSize[test.BatchItem](0))
		defer func() { _ = b.Close() }()

		require.Equal(t, batcher.DefaultBatchSize, b.Config().BatchSize)
	})
}

func TestWithBatchInterval(t *testing.T) {
	duration := time.Second
	b := batcher.New(batcher.WithBatchInterval[test.BatchItem](duration))
	defer func() { _ = b.Close() }()

	require.Equal(t, duration, b.Config().BatchInterval)

	t.Run("WithBatchInterval - zero duration", func(t *testing.T) {
		b := batcher.New(batcher.WithBatchInterval[test.BatchItem](0))
		defer func() { _ = b.Close() }()

		require.Equal(t, batcher.DefaultBatchInterval, b.Config().BatchInterval)
	})
}
