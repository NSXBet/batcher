package batcher_test

import (
	"runtime"
	"testing"
	"time"

	"github.com/NSXBet/batcher/internal/test"
	"github.com/NSXBet/batcher/pkg/batcher"
	"github.com/stretchr/testify/require"
)

// goroutinesPerRunningBatcher is the owned-goroutine budget after Phase 2.2:
//
//  1. the aggregator
//  2. the processing loop
//
// The input queue and diagnostics queue are owned data structures, not relay
// goroutines. Aggregation and processing remain separate because merging them
// changes observable latency; Phase 3 may change this topology when it introduces
// explicit worker concurrency.
const goroutinesPerRunningBatcher = 2

// TestGoroutineBudgetPerRunningBatcher pins the owned-goroutine count.
//
// The count started at 6 (rill plus chann relays), dropped to 5 when rill was
// replaced in 2.1, and reaches 2 in 2.2 once both relay-backed queues become owned
// data structures. Per-batcher goroutine cost matters because callers create one
// batcher per tenant or per key, so this is asserted rather than left to a
// measurement someone runs by hand.
func TestGoroutineBudgetPerRunningBatcher(t *testing.T) {
	// Not parallel: goroutine counting requires no other test starting or
	// stopping batchers concurrently.
	const batchers = 20

	baseline := stableGoroutineCount(t)

	live := make([]*batcher.Batcher[test.BatchItem], 0, batchers)

	for range batchers {
		live = append(live, batcher.New(
			batcher.WithBatchSize[test.BatchItem](100),
			batcher.WithBatchInterval[test.BatchItem](time.Hour), // stay idle
			batcher.WithProcessor(batcher.NoOpProcessor[test.BatchItem]),
		))
	}

	// Give every owned goroutine time to be scheduled and counted.
	running := 0

	for range 100 {
		running = runtime.NumGoroutine()

		if running >= baseline+batchers*goroutinesPerRunningBatcher {
			break
		}

		time.Sleep(20 * time.Millisecond)
	}

	perBatcher := float64(running-baseline) / float64(batchers)

	require.InDelta(t, float64(goroutinesPerRunningBatcher), perBatcher, 0.25,
		"expected %d goroutines per running batcher, measured %.2f "+
			"(baseline %d, running %d). Changing this budget is a design decision: "+
			"update the constant and the plan's goroutine table together.",
		goroutinesPerRunningBatcher, perBatcher, baseline, running)

	for _, b := range live {
		require.NoError(t, b.Close())
	}

	// Every owned goroutine must exit, or a long-lived process leaks one set per
	// batcher it ever created.
	settled := stableGoroutineCount(t)

	require.LessOrEqual(t, settled, baseline,
		"goroutines must return to the pre-construction baseline after Close "+
			"(baseline %d, settled %d)", baseline, settled)
}

// stableGoroutineCount waits for the goroutine count to stop changing, so a
// scheduler that has not yet reaped finished goroutines cannot be mistaken for a
// leak.
func stableGoroutineCount(t *testing.T) int {
	t.Helper()

	previous := -1

	for range 100 {
		runtime.GC()
		time.Sleep(20 * time.Millisecond)

		current := runtime.NumGoroutine()
		if current == previous {
			return current
		}

		previous = current
	}

	return previous
}
