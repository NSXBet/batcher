package batcher_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NSXBet/batcher/pkg/batcher"
	"github.com/stretchr/testify/require"
	"go.uber.org/fx"
	"go.uber.org/fx/fxtest"
)

type optionsProcessor struct {
	batches atomic.Int64
	items   atomic.Int64
}

func newOptionsProcessor() *optionsProcessor {
	return &optionsProcessor{}
}

// optionsProcessorFunc is the factory Fx injects: it depends on the processor
// struct and returns the Processor[T] the batcher needs.
func optionsProcessorFunc(p *optionsProcessor) batcher.Processor[string] {
	return p.Process
}

func (p *optionsProcessor) Process(items []string) error {
	p.batches.Add(1)
	p.items.Add(int64(len(items)))

	return nil
}

// TestProvideBatcherInFXWithOptionsAppliesEveryOption pins that the options
// variant is a full replacement for the positional one: anything New accepts,
// Fx accepts.
//
// The positional ProvideBatcherInFX can only express batch size and interval, so
// bounded queues and worker concurrency were previously unreachable from Fx
// without constructing the batcher by hand and losing lifecycle integration.
func TestProvideBatcherInFXWithOptionsAppliesEveryOption(t *testing.T) {
	t.Parallel()

	var b *batcher.Batcher[string]

	app := fxtest.New(t,
		fx.Provide(newOptionsProcessor),
		batcher.ProvideBatcherInFXWithOptions[string](
			optionsProcessorFunc,
			batcher.WithBatchSize[string](7),
			batcher.WithBatchInterval[string](11*time.Millisecond),
			batcher.WithMaxQueueSize[string](64),
			batcher.WithCloseGrace[string](3*time.Second),
			batcher.WithErrorBufferSize[string](16),
			batcher.WithConcurrency[string](3),
			batcher.WithoutOrderedProcessing[string](),
		),
		fx.Populate(&b),
	)

	app.RequireStart()

	config := b.Config()

	require.Equal(t, 7, config.BatchSize)
	require.Equal(t, 11*time.Millisecond, config.BatchInterval)
	require.Equal(t, 64, config.MaxQueueSize)
	require.Equal(t, 3*time.Second, config.CloseGrace)
	require.Equal(t, 16, config.ErrorBufferSize)
	require.Equal(t, 3, config.Concurrency)
	require.True(t, config.UnorderedProcessingAcknowledged)

	app.RequireStop()
}

// TestProvideBatcherInFXWithOptionsUsesInjectedProcessor pins that the processor
// still comes from dependency injection rather than from the option list.
func TestProvideBatcherInFXWithOptionsUsesInjectedProcessor(t *testing.T) {
	t.Parallel()

	var (
		b         *batcher.Batcher[string]
		processor *optionsProcessor
	)

	app := fxtest.New(t,
		fx.Provide(newOptionsProcessor),
		batcher.ProvideBatcherInFXWithOptions[string](
			optionsProcessorFunc,
			batcher.WithBatchSize[string](2),
			batcher.WithBatchInterval[string](5*time.Millisecond),
		),
		fx.Populate(&b, &processor),
	)

	app.RequireStart()

	for i := range 6 {
		b.Add(string(rune('a' + i)))
	}

	require.NoError(t, b.Join(10*time.Second))

	app.RequireStop()

	require.Equal(t, int64(6), processor.items.Load(),
		"the injected processor must receive every item")
	require.Positive(t, processor.batches.Load())
}

// TestFxStopHookDrainsQueuedWork pins the stop-hook contract: work accepted before
// shutdown is drained, not discarded, and a clean drain reports no error.
func TestFxStopHookDrainsQueuedWork(t *testing.T) {
	t.Parallel()

	var (
		b         *batcher.Batcher[string]
		processor *optionsProcessor
	)

	app := fxtest.New(t,
		fx.Provide(newOptionsProcessor),
		batcher.ProvideBatcherInFXWithOptions[string](
			optionsProcessorFunc,
			// Large batch and long interval: only the stop hook can flush this.
			batcher.WithBatchSize[string](1_000),
			batcher.WithBatchInterval[string](time.Hour),
		),
		fx.Populate(&b, &processor),
	)

	app.RequireStart()

	for i := range 5 {
		b.Add(string(rune('a' + i)))
	}

	app.RequireStop()

	require.Equal(t, int64(5), processor.items.Load(),
		"the stop hook must flush the partial batch rather than discard it")
	require.True(t, b.IsClosed())
}

// TestFxStopHookReportsIncompleteDrain pins that a stop context which expires is
// reported rather than hidden, and that the drain continues afterwards.
//
// This is the honest-failure case: Fx surfaces a lifecycle error, and the batcher
// stays in draining rather than claiming to be closed.
func TestFxStopHookReportsIncompleteDrain(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	defer close(release)

	blocking := func() batcher.Processor[string] {
		return func([]string) error {
			<-release

			return nil
		}
	}

	var b *batcher.Batcher[string]

	app := fx.New(
		batcher.ProvideBatcherInFXWithOptions[string](
			blocking,
			batcher.WithBatchSize[string](1),
			batcher.WithBatchInterval[string](time.Millisecond),
		),
		fx.Populate(&b),
		fx.NopLogger,
	)

	startCtx, cancelStart := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelStart()

	require.NoError(t, app.Start(startCtx))

	b.Add("blocked")

	require.Eventually(t, func() bool {
		return b.Stats().InFlight > 0
	}, 5*time.Second, time.Millisecond)

	stopCtx, cancelStop := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancelStop()

	err := app.Stop(stopCtx)

	require.Error(t, err,
		"an expired stop context must be reported, not silently treated as a clean stop")
	require.True(t, b.IsClosing(), "admission must be sealed")
	require.False(t, b.IsClosed(),
		"a batcher whose processor is still running has not finished draining")
}

// TestFxRepeatedStopIsIdempotent pins that stopping twice is safe, since a
// supervisor or test harness may do so.
func TestFxRepeatedStopIsIdempotent(t *testing.T) {
	t.Parallel()

	var b *batcher.Batcher[string]

	app := fx.New(
		fx.Provide(newOptionsProcessor),
		batcher.ProvideBatcherInFXWithOptions[string](
			optionsProcessorFunc,
			batcher.WithBatchSize[string](4),
			batcher.WithBatchInterval[string](2*time.Millisecond),
		),
		fx.Populate(&b),
		fx.NopLogger,
	)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	require.NoError(t, app.Start(ctx))

	b.Add("one")

	require.NoError(t, app.Stop(ctx))
	require.True(t, b.IsClosed())

	// Fx itself will not run the stop hooks twice, so call Shutdown directly to
	// prove the underlying operation is idempotent.
	require.NoError(t, b.Shutdown(ctx))
	require.True(t, b.IsClosed())
}

// TestConfigSnapshotCannotMutateRunningBatcher is the race proof required for the
// Config() change.
//
// Config() previously returned the live *Config, so a caller could change batch
// size, interval or the processor while the aggregation goroutine was reading them.
// It now returns a value copy: mutating the returned struct must be inert, and
// doing so concurrently with a running batcher must not be a data race.
//
// Run under -race, this fails if Config() ever returns shared state again.
func TestConfigSnapshotCannotMutateRunningBatcher(t *testing.T) {
	t.Parallel()

	var processed atomic.Int64

	b := batcher.New(
		batcher.WithBatchSize[int](8),
		batcher.WithBatchInterval[int](2*time.Millisecond),
		batcher.WithProcessor(func(items []int) error {
			processed.Add(int64(len(items)))

			return nil
		}),
	)

	original := b.Config()

	var wg sync.WaitGroup

	// Hammer Config() and mutate every returned copy while the batcher is actively
	// aggregating and processing.
	for range 4 {
		wg.Add(1)

		go func() {
			defer wg.Done()

			for range 200 {
				snapshot := b.Config()

				snapshot.BatchSize = 1
				snapshot.BatchInterval = time.Hour
				snapshot.Concurrency = 99
				snapshot.MaxQueueSize = 7
				snapshot.ProcessorFunc = nil
			}
		}()
	}

	wg.Add(1)

	go func() {
		defer wg.Done()

		for i := range 400 {
			b.Add(i)
		}
	}()

	wg.Wait()

	require.NoError(t, b.Shutdown(context.Background()))

	// The live configuration is untouched by any of those writes.
	current := b.Config()

	require.Equal(t, original.BatchSize, current.BatchSize)
	require.Equal(t, original.BatchInterval, current.BatchInterval)
	require.Equal(t, original.Concurrency, current.Concurrency)
	require.Equal(t, original.MaxQueueSize, current.MaxQueueSize)
	require.NotNil(t, current.ProcessorFunc,
		"nilling the snapshot's processor must not disarm the running batcher")

	require.Equal(t, int64(400), processed.Load(),
		"every item must still be processed with the original configuration")
}
