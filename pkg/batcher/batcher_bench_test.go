package batcher_test

import (
	"fmt"
	"runtime"
	"testing"
	"time"

	"github.com/NSXBet/batcher/internal/test"
	"github.com/NSXBet/batcher/pkg/batcher"
)

func BenchmarkBatcherBatchSize10(b *testing.B) {
	runBench(b, 10)
}

func BenchmarkBatcherBatchSize100(b *testing.B) {
	runBench(b, 100)
}

func BenchmarkBatcherBatchSize1_000(b *testing.B) {
	runBench(b, 1000)
}

func BenchmarkBatcherBatchSize10_000(b *testing.B) {
	runBench(b, 10000)
}

func BenchmarkBatcherBatchSize100_000(b *testing.B) {
	runBench(b, 100000)
}

// Benchmark concurrent usage
func BenchmarkBatcherConcurrentAdd(b *testing.B) {
	batch := batcher.New(
		batcher.WithProcessor(func(_ []test.BatchItem) error {
			return nil
		}),
		batcher.WithBatchSize[test.BatchItem](1000),
		batcher.WithBatchInterval[test.BatchItem](100*time.Millisecond),
	)
	defer batch.Close()

	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			batch.Add(test.BatchItem{Key: fmt.Sprintf("key_%d", i)})
			i++
		}
	})
}

// Benchmark memory allocations specifically
func BenchmarkBatcherAddOnly(b *testing.B) {
	batch := batcher.New(
		batcher.WithProcessor(func(_ []test.BatchItem) error {
			return nil
		}),
		batcher.WithBatchSize[test.BatchItem](1000000), // Large batch to avoid processing
		batcher.WithBatchInterval[test.BatchItem](1*time.Hour), // Long interval
	)
	defer batch.Close()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		batch.Add(test.BatchItem{Key: fmt.Sprintf("key_%d", i)})
	}
}

// Benchmark with different intervals
func BenchmarkBatcherFastInterval(b *testing.B) {
	runBenchWithInterval(b, 1000, 1*time.Millisecond)
}

func BenchmarkBatcherSlowInterval(b *testing.B) {
	runBenchWithInterval(b, 1000, 100*time.Millisecond)
}

// Memory usage benchmark
func BenchmarkBatcherMemoryUsage(b *testing.B) {
	var m1, m2 runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&m1)

	batch := batcher.New(
		batcher.WithProcessor(func(_ []test.BatchItem) error {
			return nil
		}),
		batcher.WithBatchSize[test.BatchItem](1000),
		batcher.WithBatchInterval[test.BatchItem](10*time.Millisecond),
	)

	for i := 0; i < b.N; i++ {
		batch.Add(test.BatchItem{Key: fmt.Sprintf("key_%d", i)})
	}

	batch.Close()
	runtime.GC()
	runtime.ReadMemStats(&m2)

	b.ReportMetric(float64(m2.TotalAlloc-m1.TotalAlloc)/float64(b.N), "B/op")
}

// CPU profiling benchmark
func BenchmarkBatcherCPUProfile(b *testing.B) {
	batch := batcher.New(
		batcher.WithProcessor(func(_ []test.BatchItem) error {
			return nil
		}),
		batcher.WithBatchSize[test.BatchItem](1000),
		batcher.WithBatchInterval[test.BatchItem](10*time.Millisecond),
	)
	defer batch.Close()

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			batch.Add(test.BatchItem{Key: fmt.Sprintf("key_%d", i)})
			i++
		}
	})
}

func runBench(b *testing.B, batchSize int) {
	b.StopTimer()

	batch := batcher.New(
		batcher.WithProcessor(func(_ []test.BatchItem) error {
			return nil
		}),
		batcher.WithBatchSize[test.BatchItem](batchSize),
		batcher.WithBatchInterval[test.BatchItem](1*time.Second),
	)

	defer batch.Close()

	b.StartTimer()

	for i := 0; i < b.N; i++ {
		batch.Add(test.BatchItem{Key: fmt.Sprintf("key_%d", i)})
	}

	b.StopTimer()

	if err := batch.Join(5 * time.Second); err != nil {
		b.Fatalf("error: %v", err)
	}

	if batch.Len() > 0 {
		b.Fatalf("expected 0 items in batch, got %d", batch.Len())
	}
}

func runBenchWithInterval(b *testing.B, batchSize int, interval time.Duration) {
	b.StopTimer()

	batch := batcher.New(
		batcher.WithProcessor(func(_ []test.BatchItem) error {
			return nil
		}),
		batcher.WithBatchSize[test.BatchItem](batchSize),
		batcher.WithBatchInterval[test.BatchItem](interval),
	)

	defer batch.Close()

	b.StartTimer()

	for i := 0; i < b.N; i++ {
		batch.Add(test.BatchItem{Key: fmt.Sprintf("key_%d", i)})
	}

	b.StopTimer()

	if err := batch.Join(5 * time.Second); err != nil {
		b.Fatalf("error: %v", err)
	}
}
