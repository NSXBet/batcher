package batcher_test

import (
	"runtime"
	"testing"
	"time"

	"github.com/NSXBet/batcher/internal/test"
	"github.com/NSXBet/batcher/pkg/batcher"
	"github.com/stretchr/testify/require"
)

// goroutinesPerRunningBatcher is the owned-goroutine budget for a constructed,
// auto-started batcher:
//
//  1. the unbounded input queue's relay
//  2. the input forwarder
//  3. the aggregator
//  4. the processing loop
//  5. the unbounded error queue's relay
//
// Phase 2.2 removes both queue relays and the forwarder with them, which will
// lower this to 2. Any change to this number is a design change and must be
// deliberate, so it is asserted rather than left to a measurement someone runs by
// hand.
const goroutinesPerRunningBatcher = 5

// TestGoroutineBudgetPerRunningBatcher pins the owned-goroutine count.
//
// Milestone 2.1's headline claim is that removing rill reduces goroutines per
// batcher from 6 to 5 without changing behaviour. That claim is only meaningful if
// something enforces it, so this test measures the real cost of many live
// batchers and fails if the budget drifts in either direction.
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
