package batcher

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestAddCountsItemBeforeProcessorRuns(t *testing.T) {
	const (
		producers       = 8
		addsPerProducer = 2000
		totalAdds       = producers * addsPerProducer
	)

	observedStaleCount := make(chan int64, 1)
	var processed atomic.Int64

	var b *Batcher[int]
	b = New(
		WithSkipAutoStart[int](),
		WithBatchSize[int](1),
		WithBatchInterval[int](time.Hour),
		WithProcessor(func(items []int) error {
			if pending := b.itemCount.Read(); pending < int64(len(items)) {
				select {
				case observedStaleCount <- pending:
				default:
				}
			}

			processed.Add(int64(len(items)))

			return nil
		}),
	)
	b.Start()

	var wg sync.WaitGroup
	for producerID := 0; producerID < producers; producerID++ {
		wg.Add(1)
		go func(offset int) {
			defer wg.Done()

			for i := 0; i < addsPerProducer; i++ {
				b.Add(offset + i)
			}
		}(producerID * addsPerProducer)
	}

	wg.Wait()

	if err := b.Join(10 * time.Second); err != nil {
		t.Fatalf("join error: %v", err)
	}

	if got := processed.Load(); got != totalAdds {
		t.Fatalf("processed %d items, want %d", got, totalAdds)
	}

	select {
	case pending := <-observedStaleCount:
		t.Fatalf("processor observed stale pending count %d while processing", pending)
	default:
	}

	if got := b.Len(); got != 0 {
		t.Fatalf("expected len 0 after join, got %d", got)
	}

	if err := b.Close(); err != nil {
		t.Fatalf("close error: %v", err)
	}
}

func TestDoneChanFlushesCurrentBufferedBatch(t *testing.T) {
	processed := make(chan []int, 1)

	b := New(
		WithSkipAutoStart[int](),
		WithBatchSize[int](10),
		WithBatchInterval[int](time.Hour),
		WithProcessor(func(items []int) error {
			batchCopy := append([]int(nil), items...)
			processed <- batchCopy

			return nil
		}),
	)
	b.Start()

	b.Add(1)
	b.Add(2)
	b.Add(3)

	deadline := time.Now().Add(time.Second)
	for b.batchInputChan.Len() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for worker to buffer items, input len=%d", b.batchInputChan.Len())
		}

		time.Sleep(time.Millisecond)
	}

	close(b.doneChan)

	select {
	case items := <-processed:
		if len(items) != 3 || items[0] != 1 || items[1] != 2 || items[2] != 3 {
			t.Fatalf("processed items %v, want [1 2 3]", items)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for buffered batch to flush on shutdown")
	}

	if err := b.Join(time.Second); err != nil {
		t.Fatalf("join error after shutdown flush: %v", err)
	}

	select {
	case _, ok := <-b.Errors():
		if ok {
			t.Fatal("expected errors channel to be closed after shutdown")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for errors channel to close")
	}
}

func TestDoneChanFlushesBufferedBatchAfterJoinTimeout(t *testing.T) {
	processed := make(chan []int, 1)

	b := New(
		WithSkipAutoStart[int](),
		WithBatchSize[int](10),
		WithBatchInterval[int](time.Hour),
		WithProcessor(func(items []int) error {
			batchCopy := append([]int(nil), items...)
			processed <- batchCopy

			return nil
		}),
	)
	b.Start()

	b.Add(10)
	b.Add(20)
	b.Add(30)

	deadline := time.Now().Add(time.Second)
	for b.batchInputChan.Len() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for worker to buffer items before Join timeout, input len=%d", b.batchInputChan.Len())
		}

		time.Sleep(time.Millisecond)
	}

	if err := b.Join(20 * time.Millisecond); err != ErrTimeout {
		t.Fatalf("join error = %v, want %v", err, ErrTimeout)
	}

	close(b.doneChan)

	select {
	case items := <-processed:
		if len(items) != 3 || items[0] != 10 || items[1] != 20 || items[2] != 30 {
			t.Fatalf("processed items %v, want [10 20 30]", items)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for buffered batch to flush after Join timeout")
	}

	if err := b.Join(time.Second); err != nil {
		t.Fatalf("join error after timeout-triggered shutdown flush: %v", err)
	}
}
