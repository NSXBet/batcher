package batcher_test

import (
	"testing"
	"time"

	"github.com/NSXBet/batcher/internal/test"
	"github.com/NSXBet/batcher/pkg/batcher"
	"github.com/stretchr/testify/require"
)

func TestWithProcessor(t *testing.T) {
	// ARRANGE
	b := batcher.New[test.BatchItem]()

	var (
		pr        batcher.Processor[test.BatchItem]
		processor batcher.Processor[test.BatchItem]
	)

	processor = func(items []test.BatchItem) error {
		return nil
	}

	// ACT
	batcher.WithProcessor(processor)(b)

	// ASSERT
	pr = b.Config().ProcessorFunc
	require.IsType(t, processor, pr)
}

func TestWithBatchSize(t *testing.T) {
	// ARRANGE
	b := batcher.New[test.BatchItem]()

	// ACT
	batcher.WithBatchSize[test.BatchItem](1000)(b)

	// ASSERT
	require.Equal(t, 1000, b.Config().BatchSize)

	t.Run("WithBatchSize - zero size", func(t *testing.T) {
		// ARRANGE
		b := batcher.New[test.BatchItem]()

		// ACT
		batcher.WithBatchSize[test.BatchItem](0)(b)

		// ASSERT
		require.Equal(t, batcher.DefaultBatchSize, b.Config().BatchSize)
	})
}

func TestWithBatchInterval(t *testing.T) {
	// ARRANGE
	b := batcher.New[test.BatchItem]()
	duration := 1 * time.Second

	// ACT
	batcher.WithBatchInterval[test.BatchItem](duration)(b)

	// ASSERT
	require.Equal(t, duration, b.Config().BatchInterval)

	t.Run("WithBatchInterval - zero duration", func(t *testing.T) {
		// ARRANGE
		b := batcher.New[test.BatchItem]()

		// ACT
		batcher.WithBatchInterval[test.BatchItem](0)(b)

		// ASSERT
		require.Equal(t, batcher.DefaultBatchInterval, b.Config().BatchInterval)
	})
}
