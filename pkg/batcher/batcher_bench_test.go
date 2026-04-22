package batcher_test

import (
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NSXBet/batcher/pkg/batcher"
)

type benchItem struct {
	ID int
}

func BenchmarkBatcherEnqueueOnly(b *testing.B) {
	for _, batchSize := range []int{10, 100, 1000} {
		b.Run("steady_high_volume_flushes_by_size_batch_"+strconv.Itoa(batchSize), func(b *testing.B) {
			benchmarkEnqueueOnlySteadyLoad(b, batchSize)
		})
	}
}

func BenchmarkBatcherEndToEnd(b *testing.B) {
	for _, batchSize := range []int{10, 100, 1000} {
		b.Run("steady_high_volume_processes_full_batch_of_"+strconv.Itoa(batchSize), func(b *testing.B) {
			benchmarkEndToEndSizeFlush(b, batchSize)
		})
	}

	b.Run("burst_of_1000_items_drains_in_batches_of_100", func(b *testing.B) {
		benchmarkBurstDrain(b, 1000, 100)
	})

	b.Run("sparse_trickle_flushes_single_item_on_interval", func(b *testing.B) {
		benchmarkEndToEndIntervalFlush(b, 2*time.Millisecond)
	})
}

func BenchmarkBatcherShutdown(b *testing.B) {
	b.Run("service_shutdown_drains_partial_batch_before_exit", func(b *testing.B) {
		benchmarkShutdownFlush(b, 25, 100, 5*time.Millisecond)
	})
}

// BenchmarkBatcherConcurrentProducers covers the dominant production usage:
// many goroutines in a service concurrently call Add on a single batcher while
// one consumer flushes batches. This exercises the contended enqueue path on
// batchInputChan and shows per-Add latency under GOMAXPROCS-way concurrency.
func BenchmarkBatcherConcurrentProducers(b *testing.B) {
	for _, batchSize := range []int{10, 100, 1000} {
		b.Run("many_producers_flush_by_size_batch_"+strconv.Itoa(batchSize), func(b *testing.B) {
			benchmarkConcurrentProducers(b, batchSize)
		})
	}
}

func benchmarkConcurrentProducers(b *testing.B, batchSize int) {
	b.ReportAllocs()

	var processed atomic.Int64

	batch := batcher.New(
		batcher.WithProcessor(func(items []benchItem) error {
			processed.Add(int64(len(items)))
			return nil
		}),
		batcher.WithBatchSize[benchItem](batchSize),
		batcher.WithBatchInterval[benchItem](time.Second),
	)
	b.Cleanup(func() {
		if err := batch.Close(); err != nil {
			b.Fatalf("close error: %v", err)
		}
	})

	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			batch.Add(benchItem{})
		}
	})

	b.StopTimer()

	if err := batch.Join(30 * time.Second); err != nil {
		b.Fatalf("join error: %v", err)
	}

	if got := processed.Load(); got != int64(b.N) {
		b.Fatalf("processed %d items, want %d", got, b.N)
	}

	b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "items/s")
}

// BenchmarkBatcherSingleItemLatency measures per-Add latency when a producer
// emits one item at a time and waits for the batch to be processed, modeling
// low-traffic request-scoped use where every Add pays the full round-trip.
func BenchmarkBatcherSingleItemLatency(b *testing.B) {
	b.ReportAllocs()

	processed := make(chan int, 1)

	batch := batcher.New(
		batcher.WithProcessor(func(items []benchItem) error {
			processed <- len(items)
			return nil
		}),
		batcher.WithBatchSize[benchItem](1),
		batcher.WithBatchInterval[benchItem](time.Second),
	)
	b.Cleanup(func() {
		if err := batch.Close(); err != nil {
			b.Fatalf("close error: %v", err)
		}
	})

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		batch.Add(benchItem{ID: i})
		if got := <-processed; got != 1 {
			b.Fatalf("processed batch size %d, want 1", got)
		}
	}

	b.StopTimer()

	if err := batch.Join(10 * time.Second); err != nil {
		b.Fatalf("join error: %v", err)
	}

	b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "items/s")
}

func benchmarkEnqueueOnlySteadyLoad(b *testing.B, batchSize int) {
	b.ReportAllocs()

	var processed atomic.Int64

	batch := batcher.New(
		batcher.WithProcessor(func(items []benchItem) error {
			processed.Add(int64(len(items)))
			return nil
		}),
		batcher.WithBatchSize[benchItem](batchSize),
		batcher.WithBatchInterval[benchItem](time.Second),
	)
	b.Cleanup(func() {
		if err := batch.Close(); err != nil {
			b.Fatalf("close error: %v", err)
		}
	})

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		batch.Add(benchItem{ID: i})
	}

	b.StopTimer()

	if err := batch.Join(10 * time.Second); err != nil {
		b.Fatalf("join error: %v", err)
	}

	if got := processed.Load(); got != int64(b.N) {
		b.Fatalf("processed %d items, want %d", got, b.N)
	}

	b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "items/s")
}

func benchmarkEndToEndSizeFlush(b *testing.B, batchSize int) {
	b.ReportAllocs()

	processed := make(chan int, 1)

	batch := batcher.New(
		batcher.WithProcessor(func(items []benchItem) error {
			processed <- len(items)
			return nil
		}),
		batcher.WithBatchSize[benchItem](batchSize),
		batcher.WithBatchInterval[benchItem](time.Second),
	)
	b.Cleanup(func() {
		if err := batch.Close(); err != nil {
			b.Fatalf("close error: %v", err)
		}
	})

	totalItems := 0

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		for j := 0; j < batchSize; j++ {
			batch.Add(benchItem{ID: totalItems + j})
		}

		totalItems += batchSize

		if got := <-processed; got != batchSize {
			b.Fatalf("processed batch size %d, want %d", got, batchSize)
		}
	}

	b.StopTimer()

	if err := batch.Join(10 * time.Second); err != nil {
		b.Fatalf("join error: %v", err)
	}

	if batch.Len() > 0 {
		b.Fatalf("expected 0 items in batch, got %d", batch.Len())
	}

	b.ReportMetric(float64(totalItems)/b.Elapsed().Seconds(), "items/s")
}

func benchmarkEndToEndIntervalFlush(b *testing.B, interval time.Duration) {
	b.ReportAllocs()

	processed := make(chan int, 1)

	batch := batcher.New(
		batcher.WithProcessor(func(items []benchItem) error {
			processed <- len(items)
			return nil
		}),
		batcher.WithBatchSize[benchItem](1000),
		batcher.WithBatchInterval[benchItem](interval),
	)
	b.Cleanup(func() {
		if err := batch.Close(); err != nil {
			b.Fatalf("close error: %v", err)
		}
	})

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		batch.Add(benchItem{ID: i})

		if got := <-processed; got != 1 {
			b.Fatalf("processed batch size %d, want 1", got)
		}
	}

	b.StopTimer()

	if err := batch.Join(10 * time.Second); err != nil {
		b.Fatalf("join error: %v", err)
	}

	b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "items/s")
}

func benchmarkBurstDrain(b *testing.B, burstSize int, batchSize int) {
	b.ReportAllocs()

	processed := make(chan int, burstSize/batchSize+1)

	batch := batcher.New(
		batcher.WithProcessor(func(items []benchItem) error {
			processed <- len(items)
			return nil
		}),
		batcher.WithBatchSize[benchItem](batchSize),
		batcher.WithBatchInterval[benchItem](time.Second),
	)
	b.Cleanup(func() {
		if err := batch.Close(); err != nil {
			b.Fatalf("close error: %v", err)
		}
	})

	nextID := 0

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		for j := 0; j < burstSize; j++ {
			batch.Add(benchItem{ID: nextID})
			nextID++
		}

		drained := 0
		for drained < burstSize {
			drained += <-processed
		}
	}

	b.StopTimer()

	if err := batch.Join(10 * time.Second); err != nil {
		b.Fatalf("join error: %v", err)
	}

	b.ReportMetric(float64(b.N*burstSize)/b.Elapsed().Seconds(), "items/s")
}

func benchmarkShutdownFlush(b *testing.B, pendingItems int, batchSize int, interval time.Duration) {
	b.ReportAllocs()

	totalItems := 0
	b.StopTimer()

	for i := 0; i < b.N; i++ {
		processed := make(chan int, 1)

		batch := batcher.New(
			batcher.WithProcessor(func(items []benchItem) error {
				processed <- len(items)
				return nil
			}),
			batcher.WithBatchSize[benchItem](batchSize),
			batcher.WithBatchInterval[benchItem](interval),
		)

		b.StartTimer()

		for j := 0; j < pendingItems; j++ {
			batch.Add(benchItem{ID: j})
		}

		if err := batch.Close(); err != nil {
			b.Fatalf("close error: %v", err)
		}

		b.StopTimer()

		if got := <-processed; got != pendingItems {
			b.Fatalf("processed batch size %d, want %d", got, pendingItems)
		}

		totalItems += pendingItems
	}

	b.ReportMetric(float64(totalItems)/b.Elapsed().Seconds(), "items/s")
}
