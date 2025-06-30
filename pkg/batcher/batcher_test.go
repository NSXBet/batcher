package batcher_test

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/NSXBet/batcher/internal/test"
	"github.com/NSXBet/batcher/pkg/batcher"
	"github.com/stretchr/testify/require"
	"go.uber.org/atomic"
)

func TestCanCreateBatcherWithDefaultConfig(t *testing.T) {
	// ACT
	b := batcher.New[test.BatchItem]()

	var (
		expected batcher.Processor[test.BatchItem]
		actual   batcher.Processor[test.BatchItem]
	)

	// ASSERT
	require.NotNil(t, b)
	require.Equal(t, batcher.DefaultBatchSize, b.Config().BatchSize)
	require.Equal(t, batcher.DefaultBatchInterval, b.Config().BatchInterval)

	expected = batcher.NoOpProcessor[test.BatchItem]
	actual = b.Config().ProcessorFunc
	require.IsType(t, expected, actual)
}

func TestCanCreateBatcher(t *testing.T) {
	// ARRANGE
	noop := func(_ []test.BatchItem) error {
		return nil
	}

	// ACT
	b := batcher.New(
		batcher.WithProcessor(noop),
		batcher.WithBatchSize[test.BatchItem](1000),
		batcher.WithBatchInterval[test.BatchItem](1*time.Second),
	)

	// ASSERT
	require.NotNil(t, b)
	require.Equal(t, 1000, b.Config().BatchSize)
	require.Equal(t, 1*time.Second, b.Config().BatchInterval)
}

func TestCanAddItemsToBatch(t *testing.T) {
	// ARRANGE
	b := batcher.New[test.BatchItem]()

	// ACT
	b.Add(test.BatchItem{})

	// ASSERT
	require.Equal(t, 1, b.Len())
}

func TestCanProcessItemsWithNoOp(t *testing.T) {
	// ARRANGE
	b := batcher.New[test.BatchItem]()

	// ACT
	for i := 0; i < 1000; i++ {
		b.Add(test.BatchItem{Key: fmt.Sprintf("key_%d", i)})
	}

	// ASSERT
	require.NoError(t, b.Join(100*time.Millisecond))
	require.Equal(t, 0, b.Len())
}

func TestCanProcessItemsWithCustomProcessor(t *testing.T) {
	// ARRANGE
	foundItems := sync.Map{}
	b := batcher.New(
		batcher.WithProcessor(func(items []test.BatchItem) error {
			for _, item := range items {
				foundItems.Store(item.Key, item)
			}

			return nil
		}),
		batcher.WithBatchInterval[test.BatchItem](1*time.Millisecond),
	)

	// ACT
	for i := 0; i < 1000; i++ {
		b.Add(test.BatchItem{Key: fmt.Sprintf("key_%d", i)})
	}

	// ASSERT
	require.NoError(t, b.Join(100*time.Millisecond))
	require.Equal(t, 0, b.Len())

	for i := 0; i < 1000; i++ {
		_, ok := foundItems.Load(fmt.Sprintf("key_%d", i))
		require.True(t, ok)
	}
}

func TestCanProcessItemsWithStructProcessor(t *testing.T) {
	// ARRANGE
	processor := test.NewProcessor(t)

	b := batcher.New(
		batcher.WithProcessor(processor.Process),
		batcher.WithBatchInterval[test.BatchItem](1*time.Millisecond),
	)

	// ACT
	for i := 0; i < 1000; i++ {
		b.Add(test.BatchItem{Key: fmt.Sprintf("key_%d", i)})
	}

	// ASSERT
	require.NoError(t, b.Join(100*time.Millisecond))
	require.Equal(t, 0, b.Len())
}

func TestCanAddManyMoreItemsThanBatchSize(t *testing.T) {
	// ARRANGE
	foundItems := sync.Map{}
	b := batcher.New(
		batcher.WithBatchSize[test.BatchItem](100),
		batcher.WithProcessor(func(items []test.BatchItem) error {
			for _, item := range items {
				foundItems.Store(item.Key, item)
			}

			return nil
		}),
		batcher.WithBatchInterval[test.BatchItem](1*time.Millisecond),
	)

	// ACT
	for i := 0; i < 100000; i++ {
		b.Add(test.BatchItem{Key: fmt.Sprintf("key_%d", i)})
	}

	// ASSERT
	require.NoError(t, b.Join(500*time.Millisecond))
	require.Equal(t, 0, b.Len())

	for i := 0; i < 100000; i++ {
		_, ok := foundItems.Load(fmt.Sprintf("key_%d", i))
		require.True(t, ok)
	}
}

func TestCanCloseBatcher(t *testing.T) {
	// ARRANGE
	b := batcher.New[test.BatchItem]()

	// ACT
	b.Close()

	// ASSERT
	require.True(t, b.IsClosed())

	t.Run("Close - multiple calls", func(t *testing.T) {
		// ARRANGE
		b := batcher.New[test.BatchItem]()

		// ACT
		b.Close()
		b.Close()

		// ASSERT
		require.True(t, b.IsClosed())
	})
}

func TestCanHandleErrors(t *testing.T) {
	// ARRANGE
	b := batcher.New(
		batcher.WithProcessor(func(_ []test.BatchItem) error {
			return fmt.Errorf("error")
		}),
		batcher.WithBatchSize[test.BatchItem](10),
		batcher.WithBatchInterval[test.BatchItem](1*time.Millisecond),
	)

	for i := 0; i < 100; i++ {
		b.Add(test.BatchItem{})
	}

	// ACT
	defer b.Close()

	// ASSERT
	time.Sleep(5 * time.Millisecond)
	require.Equal(t, 0, b.Len())

	err := <-b.Errors()
	require.NotNil(t, err)
}

func TestFlushOnClose(t *testing.T) {
	// ARRANGE
	foundItems := sync.Map{}
	b := batcher.New(
		batcher.WithProcessor(func(items []test.BatchItem) error {
			for _, item := range items {
				foundItems.Store(item.Key, item)
			}

			return nil
		}),
		batcher.WithBatchSize[test.BatchItem](100),
		batcher.WithBatchInterval[test.BatchItem](1*time.Millisecond),
	)

	// ACT
	for i := 0; i < 1000; i++ {
		b.Add(test.BatchItem{Key: fmt.Sprintf("key_%d", i)})
	}

	// ASSERT
	b.Close()

	require.Equal(t, 0, b.Len())

	for i := 0; i < 1000; i++ {
		_, ok := foundItems.Load(fmt.Sprintf("key_%d", i))
		require.True(t, ok)
	}
}

func TestProcessesEntireBatchesIfTimerHasNotExpired(t *testing.T) {
	// ARRANGE
	batches := atomic.NewInt32(0)
	processed := atomic.NewInt32(0)

	b := batcher.New(
		batcher.WithBatchSize[test.BatchItem](5),
		batcher.WithBatchInterval[test.BatchItem](10*time.Second),
		batcher.WithProcessor(func(items []test.BatchItem) error {
			batches.Add(1)
			processed.Add(int32(len(items)))

			return nil
		}),
	)

	// ACT
	for range 10 {
		b.Add(test.BatchItem{})
	}

	require.NoError(t, b.Join(100*time.Millisecond))

	// ASSERT
	require.EqualValues(t, 2, batches.Load())
	require.EqualValues(t, 10, processed.Load())
}
