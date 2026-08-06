package batcher

import (
	"context"
	"time"

	"go.uber.org/fx"
)

func ProvideBatcherInFX[T any](
	processorFactory any,
	batchSize int,
	batchInterval time.Duration,
) fx.Option {
	return fx.Module(
		"batcher",
		fx.Provide(
			processorFactory,
			fx.Private,
		),
		fx.Provide(
			func(processorFunc Processor[T]) *Batcher[T] {
				b := New(
					WithProcessor(processorFunc),
					WithBatchSize[T](batchSize),
					WithBatchInterval[T](batchInterval),
					WithSkipAutoStart[T](),
				)

				return b
			},
		),
		fx.Invoke(
			func(lifecycle fx.Lifecycle, batcher *Batcher[T]) {
				lifecycle.Append(fx.StartHook(func(context.Context) error {
					batcher.Start()

					return nil
				}))

				// Forward the stop-hook context instead of discarding it, so the
				// application's shutdown deadline governs the drain. Previously this
				// used Close's own timeout, which meant an app with a longer grace
				// period could not use it, and one with a shorter deadline could not
				// bound it.
				//
				// If the hook's context expires the drain continues in the background
				// and the error describes what remained, rather than silently
				// discarding accepted work.
				lifecycle.Append(fx.StopHook(func(ctx context.Context) error {
					return batcher.Shutdown(ctx)
				}))
			},
		),
	)
}
