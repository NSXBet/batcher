package batcher_test

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/NSXBet/batcher/internal/test"
	"github.com/NSXBet/batcher/pkg/batcher"
	"github.com/stretchr/testify/require"
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

func TestCanSetBatchSizeBytes(t *testing.T) {
	// ARRANGE & ACT
	b := batcher.New(
		batcher.WithBatchSizeBytes[test.BatchItem](1024),
	)

	// ASSERT
	require.Equal(t, int64(1024), b.Config().BatchSizeBytes)
}

func TestFlushesWhenByteLimitReached(t *testing.T) {
	// ARRANGE
	var processedBatches [][]test.BatchItem
	var mu sync.Mutex

	b := batcher.New(
		batcher.WithBatchSizeBytes[test.BatchItem](100),
		batcher.WithBatchSize[test.BatchItem](1000),
		batcher.WithBatchInterval[test.BatchItem](1*time.Second),
		batcher.WithProcessor(func(items []test.BatchItem) error {
			mu.Lock()
			defer mu.Unlock()
			processedBatches = append(processedBatches, items)
			return nil
		}),
	)

	// ACT
	for i := 0; i < 10; i++ {
		b.Add(test.BatchItem{Key: fmt.Sprintf("very_long_key_that_should_exceed_byte_limit_%d", i)})
	}

	// ASSERT
	require.NoError(t, b.Join(200*time.Millisecond))

	require.Greater(t, len(processedBatches), 1, "Should have created multiple batches due to byte limit")

	totalItems := 0
	for _, batch := range processedBatches {
		totalItems += len(batch)
	}
	require.Equal(t, 10, totalItems, "All items should be processed")
}

func TestRespectsItemCountLimitWithByteLimit(t *testing.T) {
	// ARRANGE
	var processedBatches [][]test.BatchItem
	var mu sync.Mutex

	b := batcher.New(
		batcher.WithBatchSizeBytes[test.BatchItem](10000),
		batcher.WithBatchSize[test.BatchItem](3),
		batcher.WithBatchInterval[test.BatchItem](1*time.Second),
		batcher.WithProcessor(func(items []test.BatchItem) error {
			mu.Lock()
			defer mu.Unlock()
			processedBatches = append(processedBatches, items)
			return nil
		}),
	)

	// ACT
	for i := 0; i < 10; i++ {
		b.Add(test.BatchItem{Key: fmt.Sprintf("key_%d", i)})
	}

	// ASSERT
	require.NoError(t, b.Join(200*time.Millisecond))

	require.Greater(t, len(processedBatches), 1, "Should have created multiple batches due to item count limit")

	for i, batch := range processedBatches[:len(processedBatches)-1] {
		require.LessOrEqual(t, len(batch), 3, "Batch %d should not exceed item count limit", i)
	}

	totalItems := 0
	for _, batch := range processedBatches {
		totalItems += len(batch)
	}
	require.Equal(t, 10, totalItems, "All items should be processed")
}

func TestHandlesLargeItemsCorrectly(t *testing.T) {
	// ARRANGE
	var processedBatches [][]test.BatchItem
	var mu sync.Mutex

	b := batcher.New(
		batcher.WithBatchSizeBytes[test.BatchItem](50),
		batcher.WithBatchSize[test.BatchItem](100),
		batcher.WithBatchInterval[test.BatchItem](1*time.Second),
		batcher.WithProcessor(func(items []test.BatchItem) error {
			mu.Lock()
			defer mu.Unlock()
			processedBatches = append(processedBatches, items)
			return nil
		}),
	)

	// ACT
	largeKey := fmt.Sprintf("this_is_a_very_large_key_that_exceeds_the_byte_limit_%s",
		"padding_to_make_it_even_larger_and_ensure_single_item_batches")

	for i := 0; i < 5; i++ {
		b.Add(test.BatchItem{Key: fmt.Sprintf("%s_%d", largeKey, i)})
	}

	// ASSERT
	require.NoError(t, b.Join(200*time.Millisecond))

	require.Equal(t, 5, len(processedBatches), "Should create one batch per large item")

	for i, batch := range processedBatches {
		require.Equal(t, 1, len(batch), "Batch %d should contain exactly one large item", i)
	}
}

func TestCalculatesStringSizeCorrectly(t *testing.T) {
	// ARRANGE
	b := batcher.New[string]()

	// ACT & ASSERT
	shortString := "hello"
	longString := "this is a much longer string that should have a larger calculated size"

	shortSize := b.CalculateItemSize(shortString)
	longSize := b.CalculateItemSize(longString)

	require.Greater(t, longSize, shortSize, "Longer string should have larger calculated size")
	require.Greater(t, shortSize, int64(0), "String size should be positive")
}

func TestCalculatesStructSizeCorrectly(t *testing.T) {
	// ARRANGE
	type TestStruct struct {
		Name        string
		Value       int
		Description string
	}

	b := batcher.New[TestStruct]()

	// ACT & ASSERT
	smallStruct := TestStruct{Name: "a", Value: 1, Description: "b"}
	largeStruct := TestStruct{
		Name:        "very_long_name_field",
		Value:       12345,
		Description: "this is a very long description field that should make the struct larger",
	}

	smallSize := b.CalculateItemSize(smallStruct)
	largeSize := b.CalculateItemSize(largeStruct)

	require.Greater(t, largeSize, smallSize, "Larger struct should have larger calculated size")
	require.Greater(t, smallSize, int64(0), "Struct size should be positive")
}

func TestCalculatesSliceSizeCorrectly(t *testing.T) {
	// ARRANGE
	b := batcher.New[[]string]()

	// ACT & ASSERT
	smallSlice := []string{"a", "b"}
	largeSlice := []string{"longer", "slice", "with", "more", "elements", "and", "longer", "strings"}

	smallSize := b.CalculateItemSize(smallSlice)
	largeSize := b.CalculateItemSize(largeSlice)

	require.Greater(t, largeSize, smallSize, "Larger slice should have larger calculated size")
	require.Greater(t, smallSize, int64(0), "Slice size should be positive")
}

func TestCalculatesMapSizeCorrectly(t *testing.T) {
	// ARRANGE
	b := batcher.New[map[string]int]()

	// ACT & ASSERT
	smallMap := map[string]int{"a": 1, "b": 2}
	largeMap := map[string]int{
		"longer_key_1": 100,
		"longer_key_2": 200,
		"longer_key_3": 300,
		"longer_key_4": 400,
		"longer_key_5": 500,
	}

	smallSize := b.CalculateItemSize(smallMap)
	largeSize := b.CalculateItemSize(largeMap)

	require.Greater(t, largeSize, smallSize, "Larger map should have larger calculated size")
	require.Greater(t, smallSize, int64(0), "Map size should be positive")
}

func TestFlushesAfterTimeoutWithByteLimit(t *testing.T) {
	// ARRANGE
	var processedBatches [][]test.BatchItem
	var mu sync.Mutex

	b := batcher.New(
		batcher.WithBatchSizeBytes[test.BatchItem](10000),
		batcher.WithBatchSize[test.BatchItem](1000),
		batcher.WithBatchInterval[test.BatchItem](50*time.Millisecond),
		batcher.WithProcessor(func(items []test.BatchItem) error {
			mu.Lock()
			defer mu.Unlock()
			processedBatches = append(processedBatches, items)
			return nil
		}),
	)

	// ACT
	b.Add(test.BatchItem{Key: "item1"})
	b.Add(test.BatchItem{Key: "item2"})

	time.Sleep(100 * time.Millisecond)

	// ASSERT
	require.Equal(t, 1, len(processedBatches), "Should have flushed one batch due to timeout")
	require.Equal(t, 2, len(processedBatches[0]), "Batch should contain both items")
}

func TestHandlesConcurrentAddsWithByteLimits(t *testing.T) {
	// ARRANGE
	var processedItems sync.Map

	b := batcher.New(
		batcher.WithBatchSizeBytes[test.BatchItem](200),
		batcher.WithBatchSize[test.BatchItem](50),
		batcher.WithBatchInterval[test.BatchItem](10*time.Millisecond),
		batcher.WithProcessor(func(items []test.BatchItem) error {
			for _, item := range items {
				processedItems.Store(item.Key, true)
			}
			return nil
		}),
	)

	// ACT
	var wg sync.WaitGroup
	numGoroutines := 10
	itemsPerGoroutine := 100

	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func(goroutineID int) {
			defer wg.Done()
			for j := 0; j < itemsPerGoroutine; j++ {
				key := fmt.Sprintf("goroutine_%d_item_%d", goroutineID, j)
				b.Add(test.BatchItem{Key: key})
			}
		}(i)
	}

	wg.Wait()

	// ASSERT
	require.NoError(t, b.Join(500*time.Millisecond))

	expectedTotal := numGoroutines * itemsPerGoroutine
	actualTotal := 0
	processedItems.Range(func(_, _ interface{}) bool {
		actualTotal++
		return true
	})

	require.Equal(t, expectedTotal, actualTotal, "All items should be processed exactly once")
}

func TestHandlesZeroByteLimit(t *testing.T) {
	// ARRANGE
	var processedBatches [][]test.BatchItem
	var mu sync.Mutex

	b := batcher.New(
		batcher.WithBatchSizeBytes[test.BatchItem](0),
		batcher.WithBatchSize[test.BatchItem](10),
		batcher.WithBatchInterval[test.BatchItem](50*time.Millisecond),
		batcher.WithProcessor(func(items []test.BatchItem) error {
			mu.Lock()
			defer mu.Unlock()
			processedBatches = append(processedBatches, items)
			return nil
		}),
	)

	// ACT
	for i := 0; i < 5; i++ {
		b.Add(test.BatchItem{Key: fmt.Sprintf("key_%d", i)})
	}

	// ASSERT
	require.NoError(t, b.Join(200*time.Millisecond))

	totalItems := 0
	for _, batch := range processedBatches {
		totalItems += len(batch)
	}
	require.Equal(t, 5, totalItems, "All items should be processed despite zero byte limit")
}

func TestHandlesEmptyItems(t *testing.T) {
	// ARRANGE
	var processedBatches [][]test.BatchItem
	var mu sync.Mutex

	b := batcher.New(
		batcher.WithBatchSizeBytes[test.BatchItem](100),
		batcher.WithBatchSize[test.BatchItem](10),
		batcher.WithBatchInterval[test.BatchItem](50*time.Millisecond),
		batcher.WithProcessor(func(items []test.BatchItem) error {
			mu.Lock()
			defer mu.Unlock()
			processedBatches = append(processedBatches, items)
			return nil
		}),
	)

	// ACT
	for i := 0; i < 5; i++ {
		b.Add(test.BatchItem{Key: ""})
	}

	// ASSERT
	require.NoError(t, b.Join(200*time.Millisecond))

	totalItems := 0
	for _, batch := range processedBatches {
		totalItems += len(batch)
	}
	require.Equal(t, 5, totalItems, "All empty items should be processed")
}

func TestTimerDoesNotFlushWrongBatch(t *testing.T) {
	// ARRANGE
	var processedBatches [][]test.BatchItem
	var mu sync.Mutex

	b := batcher.New(
		batcher.WithBatchSizeBytes[test.BatchItem](50),
		batcher.WithBatchSize[test.BatchItem](100),
		batcher.WithBatchInterval[test.BatchItem](100*time.Millisecond),
		batcher.WithProcessor(func(items []test.BatchItem) error {
			mu.Lock()
			defer mu.Unlock()
			processedBatches = append(processedBatches, items)
			return nil
		}),
	)

	// ACT
	b.Add(test.BatchItem{Key: "item1"})

	time.Sleep(50 * time.Millisecond)

	b.Add(test.BatchItem{Key: "very_long_key_that_should_trigger_byte_limit_flush_immediately"})

	time.Sleep(80 * time.Millisecond)

	b.Add(test.BatchItem{Key: "item3"})

	time.Sleep(150 * time.Millisecond)

	// ASSERT
	require.Equal(t, 3, len(processedBatches), "Should have exactly 3 batches")
	require.Equal(t, 1, len(processedBatches[0]), "First batch should have 1 item (timer flush)")
	require.Equal(t, 1, len(processedBatches[1]), "Second batch should have 1 item (byte limit flush)")
	require.Equal(t, 1, len(processedBatches[2]), "Third batch should have 1 item (timer flush)")

	require.Equal(t, "item1", processedBatches[0][0].Key)
	require.Contains(t, processedBatches[1][0].Key, "very_long_key")
	require.Equal(t, "item3", processedBatches[2][0].Key)
}
