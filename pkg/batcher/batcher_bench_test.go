package batcher_test

import (
	"fmt"
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
	runBench(b, 100)
}

func BenchmarkBatcherBatchSize100_000(b *testing.B) {
	runBench(b, 100)
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
