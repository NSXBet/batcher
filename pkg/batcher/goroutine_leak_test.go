package batcher_test

import (
	"errors"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/NSXBet/batcher/pkg/batcher"
	"github.com/stretchr/testify/require"
	"sync/atomic"
)

// TestBatcher_GoroutineCleanupOnStop is the main regression test for goroutine leaks.
// It reproduces the issue where goroutines remain blocked after Close() is called.
func TestBatcher_GoroutineCleanupOnStop(t *testing.T) {
	var processCount atomic.Int32

	processor := func(items []string) error {
		count := processCount.Add(1)
		time.Sleep(100 * time.Millisecond) // Simulate processing

		// Simulate failure after processing some batches
		if count > 2 {
			return errors.New("simulated processing failure")
		}
		return nil
	}

	b := batcher.New(
		batcher.WithBatchSize[string](5),
		batcher.WithBatchInterval[string](100*time.Millisecond),
		batcher.WithProcessor(processor),
	)

	// Add messages actively while batcher is running
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			b.Add(fmt.Sprintf("msg-%d", i))
			time.Sleep(10 * time.Millisecond)
		}
	}()

	// Let some batches process
	time.Sleep(500 * time.Millisecond)

	// Stop the batcher (simulating consumer crash/shutdown)
	startGoroutines := runtime.NumGoroutine()
	t.Logf("Goroutines before Close(): %d", startGoroutines)

	err := b.Close()
	if err != nil {
		t.Logf("Batcher close returned error: %v", err)
	}

	wg.Wait()

	// VERIFY: All goroutines should exit within reasonable time
	timeout := time.After(5 * time.Second)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-timeout:
			currentGoroutines := runtime.NumGoroutine()
			t.Fatalf("Goroutine leak detected: started with %d, still have %d after 5 seconds (leaked %d goroutines)",
				startGoroutines, currentGoroutines, currentGoroutines-startGoroutines)
		case <-ticker.C:
			currentGoroutines := runtime.NumGoroutine()
			t.Logf("Current goroutines: %d", currentGoroutines)
			// Allow small variance (±2 goroutines for test runtime)
			if currentGoroutines <= startGoroutines+2 {
				t.Logf("✅ All goroutines cleaned up successfully (start: %d, current: %d)", startGoroutines, currentGoroutines)
				return
			}
		}
	}
}

// TestBatcher_StopWithPendingMessages tests that pending messages don't block goroutine cleanup.
func TestBatcher_StopWithPendingMessages(t *testing.T) {
	var processedBatches atomic.Int32

	processor := func(items []string) error {
		processedBatches.Add(1)
		time.Sleep(50 * time.Millisecond)
		return nil
	}

	b := batcher.New(
		batcher.WithBatchSize[string](10),
		batcher.WithBatchInterval[string](1*time.Second), // Long interval
		batcher.WithProcessor(processor),
	)

	// Add only 5 items (less than batch size), so they won't trigger batch processing
	for i := 0; i < 5; i++ {
		b.Add(fmt.Sprintf("msg-%d", i))
	}

	time.Sleep(100 * time.Millisecond)

	startGoroutines := runtime.NumGoroutine()
	t.Logf("Goroutines before Close(): %d", startGoroutines)

	// Close should not block indefinitely even with pending messages
	closeStart := time.Now()
	err := b.Close()
	closeDuration := time.Since(closeStart)

	t.Logf("Close() took %v, returned error: %v", closeDuration, err)

	// Verify goroutine cleanup
	timeout := time.After(5 * time.Second)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-timeout:
			currentGoroutines := runtime.NumGoroutine()
			t.Fatalf("Goroutine leak detected: started with %d, still have %d after 5 seconds",
				startGoroutines, currentGoroutines)
		case <-ticker.C:
			currentGoroutines := runtime.NumGoroutine()
			if currentGoroutines <= startGoroutines+2 {
				t.Logf("✅ All goroutines cleaned up successfully")
				return
			}
		}
	}
}

// TestBatcher_StopWhileProcessing tests cleanup when the processor is actively working.
func TestBatcher_StopWhileProcessing(t *testing.T) {
	var processing atomic.Bool
	processingDone := make(chan struct{})

	processor := func(items []string) error {
		processing.Store(true)
		time.Sleep(2 * time.Second) // Long processing time
		processing.Store(false)
		close(processingDone)
		return nil
	}

	b := batcher.New(
		batcher.WithBatchSize[string](5),
		batcher.WithBatchInterval[string](10*time.Millisecond),
		batcher.WithProcessor(processor),
	)

	// Add enough items to trigger processing
	for i := 0; i < 5; i++ {
		b.Add(fmt.Sprintf("msg-%d", i))
	}

	// Wait for processing to start
	time.Sleep(100 * time.Millisecond)
	require.True(t, processing.Load(), "Processing should have started")

	startGoroutines := runtime.NumGoroutine()
	t.Logf("Goroutines before Close(): %d", startGoroutines)

	// Close while processing is ongoing
	closeStart := time.Now()
	err := b.Close()
	closeDuration := time.Since(closeStart)

	t.Logf("Close() took %v, returned error: %v", closeDuration, err)

	// Wait for processing to complete
	select {
	case <-processingDone:
		t.Logf("Processing completed")
	case <-time.After(3 * time.Second):
		t.Logf("Processing did not complete within timeout")
	}

	// Verify goroutine cleanup
	timeout := time.After(5 * time.Second)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-timeout:
			currentGoroutines := runtime.NumGoroutine()
			t.Fatalf("Goroutine leak detected: started with %d, still have %d after 5 seconds",
				startGoroutines, currentGoroutines)
		case <-ticker.C:
			currentGoroutines := runtime.NumGoroutine()
			if currentGoroutines <= startGoroutines+2 {
				t.Logf("✅ All goroutines cleaned up successfully")
				return
			}
		}
	}
}

// TestBatcher_StopWithFailingProcessor tests cleanup when the processor consistently fails.
func TestBatcher_StopWithFailingProcessor(t *testing.T) {
	var processCount atomic.Int32

	processor := func(items []string) error {
		processCount.Add(1)
		time.Sleep(50 * time.Millisecond)
		return errors.New("processor always fails")
	}

	b := batcher.New(
		batcher.WithBatchSize[string](5),
		batcher.WithBatchInterval[string](50*time.Millisecond),
		batcher.WithProcessor(processor),
	)

	// Add many items that will all fail processing
	for i := 0; i < 50; i++ {
		b.Add(fmt.Sprintf("msg-%d", i))
	}

	// Let some failures occur
	time.Sleep(200 * time.Millisecond)

	startGoroutines := runtime.NumGoroutine()
	t.Logf("Goroutines before Close(): %d, processed %d batches", startGoroutines, processCount.Load())

	err := b.Close()
	t.Logf("Close() returned error: %v", err)

	// Verify goroutine cleanup
	timeout := time.After(5 * time.Second)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-timeout:
			currentGoroutines := runtime.NumGoroutine()
			t.Fatalf("Goroutine leak detected: started with %d, still have %d after 5 seconds",
				startGoroutines, currentGoroutines)
		case <-ticker.C:
			currentGoroutines := runtime.NumGoroutine()
			if currentGoroutines <= startGoroutines+2 {
				t.Logf("✅ All goroutines cleaned up successfully")
				return
			}
		}
	}
}

// TestBatcher_RapidStartStopCycles tests multiple rapid create/close cycles for leaks.
func TestBatcher_RapidStartStopCycles(t *testing.T) {
	initialGoroutines := runtime.NumGoroutine()
	t.Logf("Initial goroutines: %d", initialGoroutines)

	processor := func(items []string) error {
		time.Sleep(10 * time.Millisecond)
		return nil
	}

	// Perform multiple rapid start/stop cycles
	for cycle := 0; cycle < 10; cycle++ {
		b := batcher.New(
			batcher.WithBatchSize[string](5),
			batcher.WithBatchInterval[string](50*time.Millisecond),
			batcher.WithProcessor(processor),
		)

		// Add some items
		for i := 0; i < 10; i++ {
			b.Add(fmt.Sprintf("cycle-%d-msg-%d", cycle, i))
		}

		time.Sleep(50 * time.Millisecond)

		err := b.Close()
		if err != nil {
			t.Logf("Cycle %d: Close() returned error: %v", cycle, err)
		}

		// Give a bit of time for cleanup
		time.Sleep(50 * time.Millisecond)
	}

	// Verify no goroutine accumulation after all cycles
	timeout := time.After(5 * time.Second)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-timeout:
			currentGoroutines := runtime.NumGoroutine()
			// Allow some growth but not excessive (10 cycles shouldn't leak 50+ goroutines)
			if currentGoroutines > initialGoroutines+10 {
				t.Fatalf("Goroutine accumulation detected: started with %d, now have %d after 10 cycles (leaked %d)",
					initialGoroutines, currentGoroutines, currentGoroutines-initialGoroutines)
			}
			t.Logf("✅ Acceptable goroutine count after cycles: %d (started with %d)", currentGoroutines, initialGoroutines)
			return
		case <-ticker.C:
			currentGoroutines := runtime.NumGoroutine()
			t.Logf("Current goroutines: %d", currentGoroutines)
			// If we're back to initial (±5 for test overhead), we're good
			if currentGoroutines <= initialGoroutines+5 {
				t.Logf("✅ All goroutines cleaned up successfully after cycles: %d (started with %d)", currentGoroutines, initialGoroutines)
				return
			}
		}
	}
}

// TestBatcher_CloseTimeoutBehavior tests that Close() doesn't block forever.
func TestBatcher_CloseTimeoutBehavior(t *testing.T) {
	// A processor that never returns. The library cannot cancel user code, so the
	// contract is that Close reports an incomplete drain rather than hanging
	// forever or pretending the work finished.
	release := make(chan struct{})
	defer close(release)

	// Signalled by the processor itself. Waiting on Stats().Pending would only prove
	// work is pending, not that the processor has been entered, so Close could race
	// ahead of processing and the in-flight case would go uncovered.
	entered := make(chan struct{})

	var enterOnce sync.Once

	processor := func(items []string) error {
		enterOnce.Do(func() { close(entered) })

		<-release

		return nil
	}

	b := batcher.New(
		batcher.WithBatchSize[string](5),
		batcher.WithBatchInterval[string](10*time.Millisecond),
		batcher.WithProcessor(processor),
		// Keep the test fast: the grace period is what bounds Close, and its exact
		// value is configuration rather than behaviour.
		batcher.WithCloseGrace[string](500*time.Millisecond),
	)

	for i := 0; i < 5; i++ {
		b.Add(fmt.Sprintf("msg-%d", i))
	}

	// Wait for the processor to be entered, so the batch really is in flight.
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the processor was never entered")
	}

	closeStart := time.Now()
	closeDone := make(chan error, 1)

	go func() {
		closeDone <- b.Close()
	}()

	select {
	case err := <-closeDone:
		closeDuration := time.Since(closeStart)
		t.Logf("Close() completed in %v with error: %v", closeDuration, err)

		require.ErrorIs(t, err, batcher.ErrTimeout,
			"a blocked processor must be reported as an incomplete drain")
		require.Less(t, closeDuration, 5*time.Second,
			"Close must be bounded by its grace period, not by the processor")

		// The drain was reported incomplete, not abandoned: the batcher is still
		// draining, so it must not claim to be closed.
		require.True(t, b.IsClosing(), "admission must be sealed")
		require.False(t, b.IsClosed(), "a batcher with work still in flight is not closed")

	case <-time.After(15 * time.Second):
		t.Fatal("Close() blocked for more than 15 seconds - this is unacceptable")
	}
}

// TestBatcher_MultipleCloseCallsNoDeadlock tests that multiple Close() calls don't deadlock.
func TestBatcher_MultipleCloseCallsNoDeadlock(t *testing.T) {
	processor := func(items []string) error {
		time.Sleep(10 * time.Millisecond)
		return nil
	}

	b := batcher.New(
		batcher.WithBatchSize[string](5),
		batcher.WithBatchInterval[string](50*time.Millisecond),
		batcher.WithProcessor(processor),
	)

	for i := 0; i < 10; i++ {
		b.Add(fmt.Sprintf("msg-%d", i))
	}

	time.Sleep(100 * time.Millisecond)

	// Call Close() concurrently from multiple goroutines
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			err := b.Close()
			t.Logf("Close() call %d returned: %v", id, err)
		}(i)
	}

	// Wait for all Close() calls to complete
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		t.Logf("✅ All Close() calls completed without deadlock")
	case <-time.After(10 * time.Second):
		t.Fatal("Multiple Close() calls resulted in deadlock")
	}
}

// TestBatcher_NoLeakWithEmptyBatcher tests that even an empty batcher cleans up properly.
func TestBatcher_NoLeakWithEmptyBatcher(t *testing.T) {
	processor := func(items []string) error {
		return nil
	}

	startGoroutines := runtime.NumGoroutine()
	t.Logf("Goroutines before batcher creation: %d", startGoroutines)

	b := batcher.New(
		batcher.WithBatchSize[string](5),
		batcher.WithBatchInterval[string](100*time.Millisecond),
		batcher.WithProcessor(processor),
	)

	// Don't add any items, just close immediately
	time.Sleep(50 * time.Millisecond)

	err := b.Close()
	t.Logf("Close() returned: %v", err)

	// Verify goroutine cleanup
	timeout := time.After(5 * time.Second)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-timeout:
			currentGoroutines := runtime.NumGoroutine()
			t.Fatalf("Goroutine leak detected: started with %d, still have %d after 5 seconds",
				startGoroutines, currentGoroutines)
		case <-ticker.C:
			currentGoroutines := runtime.NumGoroutine()
			if currentGoroutines <= startGoroutines+2 {
				t.Logf("✅ All goroutines cleaned up successfully (empty batcher)")
				return
			}
		}
	}
}
