package batcher_test

import (
	"strconv"
	"testing"
	"time"

	"github.com/NSXBet/batcher/internal/test"
	"github.com/NSXBet/batcher/pkg/batcher"
)

// benchmarkBatchInterval keeps the final partial batch drainable at the end of a
// benchmark run without making the timer a significant part of the measurement:
// flushes happen on the aggregator goroutine, not on the producer's Add path.
const benchmarkBatchInterval = 5 * time.Millisecond

// benchmarkBatchSizes covers the range advertised by the historical suite. The
// two largest cases previously ran with a batch size of 100 despite their names.
var benchmarkBatchSizes = []int{10, 100, 1_000, 10_000, 100_000}

// BenchmarkBatcherEnqueue measures producer-side Add overhead only.
//
// Batch completion and processor execution are deliberately excluded, so these
// numbers are enqueue overhead and must not be reported as end-to-end
// throughput. End-to-end latency, batching efficiency, and overload behaviour
// are covered by the scenario harness instead.
//
// Repeatable invocation:
//
//	go test -run='^$' -bench='BenchmarkBatcherEnqueue' -benchmem \
//		-benchtime=3s -count=10 -cpu=1,2,$(nproc) ./pkg/batcher
//
// Compare the resulting files with benchstat. On macOS, replace $(nproc) with
// the logical CPU count from sysctl -n hw.ncpu.
func BenchmarkBatcherEnqueue(b *testing.B) {
	for _, batchSize := range benchmarkBatchSizes {
		b.Run("batch_size="+formatCount(batchSize), func(b *testing.B) {
			batch := newBenchmarkBatcher(batchSize)
			defer closeBenchmarkBatcher(b, batch)

			// Built before the timed region: no payload construction is measured.
			item := test.BatchItem{Key: "benchmark"}

			b.ReportAllocs()
			b.ResetTimer()

			for range b.N {
				batch.Add(item)
			}

			b.StopTimer()
		})
	}
}

// BenchmarkBatcherEnqueueParallel measures Add under producer contention.
// Run with -cpu=1,2,<GOMAXPROCS> to observe how the shared input queue and the
// shared item counter scale with concurrent producers.
func BenchmarkBatcherEnqueueParallel(b *testing.B) {
	for _, batchSize := range benchmarkBatchSizes {
		b.Run("batch_size="+formatCount(batchSize), func(b *testing.B) {
			batch := newBenchmarkBatcher(batchSize)
			defer closeBenchmarkBatcher(b, batch)

			b.ReportAllocs()
			b.ResetTimer()

			b.RunParallel(func(pb *testing.PB) {
				item := test.BatchItem{Key: "benchmark"}
				for pb.Next() {
					batch.Add(item)
				}
			})

			b.StopTimer()
		})
	}
}

func newBenchmarkBatcher(batchSize int) *batcher.Batcher[test.BatchItem] {
	return batcher.New(
		batcher.WithProcessor(func(_ []test.BatchItem) error {
			return nil
		}),
		batcher.WithBatchSize[test.BatchItem](batchSize),
		batcher.WithBatchInterval[test.BatchItem](benchmarkBatchInterval),
	)
}

func closeBenchmarkBatcher(b *testing.B, batch *batcher.Batcher[test.BatchItem]) {
	b.Helper()

	if err := batch.Join(60 * time.Second); err != nil {
		b.Fatalf("draining benchmark batcher: %v", err)
	}

	if remaining := batch.Len(); remaining != 0 {
		b.Fatalf("expected 0 items after drain, got %d", remaining)
	}

	if err := batch.Close(); err != nil {
		b.Fatalf("closing benchmark batcher: %v", err)
	}
}

func formatCount(value int) string {
	if value < 1_000 {
		return strconv.Itoa(value)
	}

	return strconv.Itoa(value/1_000) + "k"
}
