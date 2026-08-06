package batcher_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NSXBet/batcher/pkg/batcher"
	"github.com/stretchr/testify/require"
)

// Diagnostics and panic-recovery tests.
//
// Errors are diagnostic, not payload. The tests here make the deliberate trade
// explicit: Batcher must keep processing when diagnostics are not consumed, and
// must report how many diagnostics it had to drop rather than turning an outage
// into a deadlock or an OOM.

// TestErrorStormWithoutConsumerIsBoundedAndNonBlocking proves the error channel
// cannot grow without limit or stall the processor when nobody reads it.
func TestErrorStormWithoutConsumerIsBoundedAndNonBlocking(t *testing.T) {
	t.Parallel()

	const (
		bufferSize = 3
		items      = 100
	)

	failure := errors.New("downstream unavailable")

	var calls atomic.Int64

	b := batcher.New(
		batcher.WithBatchSize[int](1),
		batcher.WithBatchInterval[int](time.Millisecond),
		batcher.WithErrorBufferSize[int](bufferSize),
		batcher.WithProcessor(func([]int) error {
			calls.Add(1)

			return failure
		}),
	)

	for i := range items {
		b.Add(i)
	}

	// If publishError blocked on the full channel, this wait would hang after
	// bufferSize failures. It must instead drain all work.
	require.NoError(t, b.Join(10*time.Second))

	stats := b.Stats()

	require.Equal(t, int64(items), calls.Load(),
		"a full diagnostics buffer must not stop later batches")
	require.Equal(t, uint64(items), stats.Failed)
	require.Equal(t, uint64(items-bufferSize), stats.DroppedErrors,
		"drop-newest must retain the first bufferSize diagnostics and count the rest")
	require.Zero(t, stats.Pending, "errors must still release their drain obligations")

	// Closing must not hang even though nobody has drained Errors().
	require.NoError(t, b.Close())
}

// TestProcessorPanicIsReportedAndPipelineSurvives proves a poison batch cannot
// crash the process, take the consumer down, or strand every item behind it.
func TestProcessorPanicIsReportedAndPipelineSurvives(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64

	b := batcher.New(
		batcher.WithBatchSize[int](1),
		batcher.WithBatchInterval[int](time.Millisecond),
		batcher.WithProcessor(func([]int) error {
			if calls.Add(1) == 1 {
				panic("poison batch")
			}

			return nil
		}),
	)

	b.Add(1) // panics
	b.Add(2) // must still be processed

	var diagnostic error

	select {
	case diagnostic = <-b.Errors():
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for recovered panic diagnostic")
	}

	var panicErr *batcher.ProcessorPanicError

	require.ErrorAs(t, diagnostic, &panicErr)
	require.Equal(t, "poison batch", panicErr.Value)
	require.NotEmpty(t, panicErr.Stack, "the recovered panic must retain a stack trace")
	require.True(t, strings.Contains(string(panicErr.Stack), "TestProcessorPanic"),
		"the stack trace must identify the caller that panicked")

	require.NoError(t, b.Join(5*time.Second))

	stats := b.Stats()

	require.Equal(t, int64(2), calls.Load(), "the consumer must survive the first panic")
	require.Equal(t, uint64(1), stats.Panicked)
	require.Equal(t, uint64(1), stats.Completed)
	require.Zero(t, stats.Failed, "a panic is not a processor error")
	require.Zero(t, stats.Pending, "a panic must not strand its drain obligation")

	require.NoError(t, b.Shutdown(context.Background()),
		"a recovered panic must not make the shutdown result fail")
}

// TestPanicInFinalShutdownBatchDrains proves recovery works for the batch most
// likely to regress: the partial batch flushed only by shutdown.
func TestPanicInFinalShutdownBatchDrains(t *testing.T) {
	t.Parallel()

	b := batcher.New(
		batcher.WithBatchSize[int](100),
		batcher.WithBatchInterval[int](time.Hour),
		batcher.WithProcessor(func([]int) error {
			panic("shutdown batch")
		}),
	)

	b.Add(1)

	// Drain the diagnostic concurrently, so this test also covers the final
	// diagnostic publication racing the coordinator's error-channel close.
	diagnostic := make(chan error, 1)

	go func() { diagnostic <- <-b.Errors() }()

	require.NoError(t, b.Shutdown(context.Background()))

	var err error

	select {
	case err = <-diagnostic:
	case <-time.After(5 * time.Second):
		t.Fatal("the final panic diagnostic was never published")
	}

	var panicErr *batcher.ProcessorPanicError
	require.ErrorAs(t, err, &panicErr)

	stats := b.Stats()

	require.Equal(t, uint64(1), stats.Panicked)
	require.Zero(t, stats.Pending, "the final panic must not hang shutdown")
	require.True(t, b.IsClosed())
}

// TestRecoveredPanicAddsNoSteadyStateAllocations makes the recovery guard's cost
// explicit. A defer/recover guard must not allocate when nothing panics; otherwise
// a safety feature would tax every batch.
func TestRecoveredPanicAddsNoSteadyStateAllocations(t *testing.T) {
	// Deliberately not parallel: AllocsPerRun requires exclusive GOMAXPROCS.
	b := batcher.New(
		batcher.WithBatchSize[int](1),
		batcher.WithBatchInterval[int](time.Millisecond),
		batcher.WithProcessor(batcher.NoOpProcessor[int]),
	)

	t.Cleanup(func() { _ = b.Close() })

	// Warm up the queue and processing path.
	for range 1_000 {
		b.Add(1)
	}

	require.NoError(t, b.Join(10*time.Second))

	// This measures Add's producer-side cost, which must remain allocation-free
	// despite batches being guarded by recover in another goroutine.
	allocs := testing.AllocsPerRun(2_000, func() { b.Add(1) })

	require.Zero(t, allocs,
		"the normal non-panic path must not allocate because recovery is enabled")
}
