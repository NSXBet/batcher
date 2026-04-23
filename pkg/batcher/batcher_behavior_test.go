package batcher_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/NSXBet/batcher/internal/test"
	"github.com/NSXBet/batcher/pkg/batcher"
	"github.com/stretchr/testify/require"
)

func TestJoinTimeoutWhenWorkCannotDrain(t *testing.T) {
	b := batcher.New(
		batcher.WithSkipAutoStart[test.BatchItem](),
		batcher.WithBatchSize[test.BatchItem](10),
		batcher.WithBatchInterval[test.BatchItem](5*time.Millisecond),
	)

	b.Add(test.BatchItem{Key: "pending"})

	require.ErrorIs(t, b.Join(20*time.Millisecond), batcher.ErrTimeout)
	require.Equal(t, 1, b.Len())

	b.Start()

	require.NoError(t, b.Join(500*time.Millisecond))
	require.NoError(t, b.Close())
}

func TestSkipAutoStartQueuesItemsUntilStart(t *testing.T) {
	processedCh := make(chan []test.BatchItem, 1)

	b := batcher.New(
		batcher.WithSkipAutoStart[test.BatchItem](),
		batcher.WithBatchSize[test.BatchItem](10),
		batcher.WithBatchInterval[test.BatchItem](20*time.Millisecond),
		batcher.WithProcessor(func(items []test.BatchItem) error {
			batchCopy := append([]test.BatchItem(nil), items...)
			processedCh <- batchCopy

			return nil
		}),
	)

	b.Add(test.BatchItem{Key: "first"})
	b.Add(test.BatchItem{Key: "second"})

	select {
	case batch := <-processedCh:
		t.Fatalf("received batch before Start: %+v", batch)
	case <-time.After(50 * time.Millisecond):
	}

	require.Equal(t, 2, b.Len())

	b.Start()

	select {
	case batch := <-processedCh:
		require.Equal(t, []test.BatchItem{{Key: "first"}, {Key: "second"}}, batch)
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timed out waiting for queued items to process after Start")
	}

	require.NoError(t, b.Join(500*time.Millisecond))
	require.NoError(t, b.Close())
}

func TestBatchIntervalStartsWhenFirstItemArrives(t *testing.T) {
	processedCh := make(chan time.Time, 1)

	b := batcher.New(
		batcher.WithBatchSize[test.BatchItem](10),
		batcher.WithBatchInterval[test.BatchItem](80*time.Millisecond),
		batcher.WithProcessor(func(items []test.BatchItem) error {
			processedCh <- time.Now()

			return nil
		}),
	)
	defer b.Close()

	time.Sleep(160 * time.Millisecond)

	startedAt := time.Now()
	b.Add(test.BatchItem{Key: "delayed"})

	select {
	case processedAt := <-processedCh:
		t.Fatalf("batch flushed too early after first item: %s", processedAt.Sub(startedAt))
	case <-time.After(40 * time.Millisecond):
	}

	select {
	case <-processedCh:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timed out waiting for interval flush")
	}

	require.NoError(t, b.Join(500*time.Millisecond))
}

func TestDoesNotEmitEmptyBatchesWhileIdle(t *testing.T) {
	callCh := make(chan int, 2)

	b := batcher.New(
		batcher.WithBatchSize[test.BatchItem](10),
		batcher.WithBatchInterval[test.BatchItem](20*time.Millisecond),
		batcher.WithProcessor(func(items []test.BatchItem) error {
			callCh <- len(items)

			return nil
		}),
	)
	defer b.Close()

	time.Sleep(70 * time.Millisecond)

	select {
	case size := <-callCh:
		t.Fatalf("received unexpected idle batch of size %d", size)
	default:
	}

	b.Add(test.BatchItem{Key: "only"})

	select {
	case size := <-callCh:
		require.Equal(t, 1, size)
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timed out waiting for non-empty batch")
	}

	require.NoError(t, b.Join(500*time.Millisecond))

	time.Sleep(70 * time.Millisecond)

	select {
	case size := <-callCh:
		t.Fatalf("received unexpected extra batch of size %d", size)
	default:
	}
}

func TestPreservesOrderingAcrossFullAndPartialBatches(t *testing.T) {
	var (
		mu         sync.Mutex
		keys       []string
		batchSizes []int
	)

	b := batcher.New(
		batcher.WithBatchSize[test.BatchItem](3),
		batcher.WithBatchInterval[test.BatchItem](20*time.Millisecond),
		batcher.WithProcessor(func(items []test.BatchItem) error {
			mu.Lock()
			defer mu.Unlock()

			batchSizes = append(batchSizes, len(items))
			for _, item := range items {
				keys = append(keys, item.Key)
			}

			return nil
		}),
	)
	defer b.Close()

	expectedKeys := make([]string, 0, 8)
	for i := 0; i < 8; i++ {
		key := fmt.Sprintf("key_%d", i)
		expectedKeys = append(expectedKeys, key)
		b.Add(test.BatchItem{Key: key})
	}

	require.NoError(t, b.Join(500*time.Millisecond))

	mu.Lock()
	defer mu.Unlock()

	require.Equal(t, []int{3, 3, 2}, batchSizes)
	require.Equal(t, expectedKeys, keys)
}

func TestLenReturnsToZeroAfterProcessorErrors(t *testing.T) {
	b := batcher.New(
		batcher.WithBatchSize[test.BatchItem](10),
		batcher.WithBatchInterval[test.BatchItem](10*time.Millisecond),
		batcher.WithProcessor(func(items []test.BatchItem) error {
			return fmt.Errorf("processor failed for %d items", len(items))
		}),
	)
	defer b.Close()

	b.Add(test.BatchItem{Key: "first"})
	b.Add(test.BatchItem{Key: "second"})
	b.Add(test.BatchItem{Key: "third"})

	require.NoError(t, b.Join(500*time.Millisecond))
	require.Equal(t, 0, b.Len())

	select {
	case err := <-b.Errors():
		require.EqualError(t, err, "processor failed for 3 items")
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timed out waiting for processor error")
	}
}

func TestErrorsChannelClosesWhenBatcherStops(t *testing.T) {
	b := batcher.New[test.BatchItem]()

	require.NoError(t, b.Close())

	require.Eventually(t, func() bool {
		select {
		case _, ok := <-b.Errors():
			return !ok
		default:
			return false
		}
	}, time.Second, 10*time.Millisecond)
}

func TestCloseFlushesPartialBatchAndIsIdempotent(t *testing.T) {
	var (
		mu   sync.Mutex
		keys []string
	)

	b := batcher.New(
		batcher.WithBatchSize[test.BatchItem](100),
		batcher.WithBatchInterval[test.BatchItem](50*time.Millisecond),
		batcher.WithProcessor(func(items []test.BatchItem) error {
			mu.Lock()
			defer mu.Unlock()

			for _, item := range items {
				keys = append(keys, item.Key)
			}

			return nil
		}),
	)

	b.Add(test.BatchItem{Key: "first"})
	b.Add(test.BatchItem{Key: "second"})
	b.Add(test.BatchItem{Key: "third"})

	require.NoError(t, b.Close())
	require.NoError(t, b.Close())
	require.Equal(t, 0, b.Len())

	mu.Lock()
	defer mu.Unlock()

	require.Equal(t, []string{"first", "second", "third"}, keys)
}

func TestEnqueueReturnsContextErrorWhenAlreadyCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	b := batcher.New[test.BatchItem]()
	defer b.Close()

	err := b.Enqueue(ctx, test.BatchItem{Key: "canceled"})

	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, 0, b.Len())
}

func TestEnqueueReturnsClosingErrorAfterClose(t *testing.T) {
	b := batcher.New(
		batcher.WithSkipAutoStart[test.BatchItem](),
	)

	require.NoError(t, b.Close())

	err := b.Enqueue(context.Background(), test.BatchItem{Key: "late"})

	require.ErrorIs(t, err, batcher.ErrClosing)
	require.Equal(t, 0, b.Len())
}

func TestAddSilentlyDropsAfterClose(t *testing.T) {
	b := batcher.New(
		batcher.WithSkipAutoStart[test.BatchItem](),
	)

	require.NoError(t, b.Close())

	b.Add(test.BatchItem{Key: "late"})

	require.Equal(t, 0, b.Len())
}
