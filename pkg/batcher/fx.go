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

				lifecycle.Append(fx.StopHook(func(context.Context) error {
					if err := batcher.Close(); err != nil {
						return err
					}

					return nil
				}))
			},
		),
	)
}
