package batcher

import (
	"context"
	"sync/atomic"
	"testing"
)

// BenchmarkQueueBatchDrainMPSC measures Batcher's queue in its real access shape:
// multiple publishers and exactly one greedy-draining aggregator.
//
// It deliberately exercises the queue directly instead of BenchmarkBatcherEnqueue:
// the latter measures the public producer API (admission, lifecycle counters and the
// processor) and is the right compatibility benchmark, but it cannot attribute a
// CPU or mutex profile to the intake queue itself.
//
// Capture profiles with:
//
//	go test -run '^$' -bench '^BenchmarkQueueBatchDrainMPSC$' -benchtime=5s \
//	  -cpuprofile cpu.out -memprofile mem.out -mutexprofile mutex.out ./pkg/batcher
//
// Inspect with `go tool pprof -http=:0 cpu.out` (and the other profiles). In
// particular, compare mutex delay in queue.push against earlier profiles before
// reintroducing any per-item locking or replacing the queue with an atomic structure.
func BenchmarkQueueBatchDrainMPSC(b *testing.B) {
	q := newQueue[int](0)
	sealCh := make(chan struct{})
	ctx := context.Background()

	var received atomic.Int64

	done := make(chan struct{})

	go func() {
		defer close(done)

		buf := make([]int, 0, DefaultBatchSize)

		for received.Load() < int64(b.N) {
			<-q.ready()

			for {
				buf = q.popBatch(buf[:0], DefaultBatchSize)
				if len(buf) == 0 {
					break
				}

				received.Add(int64(len(buf)))
			}
		}
	}()

	b.ReportAllocs()
	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if err := q.push(ctx, 1, sealCh); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.StopTimer()
	<-done
}
