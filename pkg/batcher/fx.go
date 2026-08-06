package batcher

import (
	"context"
	"time"

	"go.uber.org/fx"
)

// ProvideBatcherInFX wires a Batcher into an Fx application using the three
// settings most applications configure.
//
// The signature is deliberately unchanged: every existing call site keeps
// compiling. Anything beyond batch size and interval — bounded queues, worker
// concurrency, close grace, diagnostics buffer — needs
// ProvideBatcherInFXWithOptions instead, because adding positional parameters here
// would break callers for options most of them do not use.
//
// Lifecycle behaviour is identical in both variants; see the stop-hook notes on
// provideBatcherModule.
func ProvideBatcherInFX[T any](
	processorFactory any,
	batchSize int,
	batchInterval time.Duration,
) fx.Option {
	return provideBatcherModule[T](
		processorFactory,
		func(processorFunc Processor[T]) []Option[T] {
			return []Option[T]{
				WithProcessor(processorFunc),
				WithBatchSize[T](batchSize),
				WithBatchInterval[T](batchInterval),
			}
		},
	)
}

// ProvideBatcherInFXWithOptions wires a Batcher into an Fx application with the
// full option set.
//
// The processor comes from the injected factory, so callers must not pass
// WithProcessor: it is applied first and then overridden by anything supplied
// here, which would silently bypass dependency injection. Every other option
// behaves exactly as it does with New.
//
// WithSkipAutoStart is always applied last and cannot be overridden, because Fx
// owns the lifecycle: the batcher must not begin processing until the start hook
// runs.
func ProvideBatcherInFXWithOptions[T any](
	processorFactory any,
	options ...Option[T],
) fx.Option {
	return provideBatcherModule[T](
		processorFactory,
		func(processorFunc Processor[T]) []Option[T] {
			// The injected processor goes first so an explicit WithProcessor in
			// options would win. That is a caller mistake rather than something to
			// silently correct, and it is documented above.
			return append([]Option[T]{WithProcessor(processorFunc)}, options...)
		},
	)
}

// provideBatcherModule builds the Fx module shared by both entry points.
//
// Lifecycle contract:
//
//   - Start hook: starts processing. The batcher is constructed with
//     WithSkipAutoStart so nothing is processed before Fx says so.
//   - Stop hook: forwards the hook's context to Shutdown, so the application's
//     shutdown deadline governs the drain rather than the batcher's own grace
//     period. A normal drain returns nil.
//   - If the stop context expires, Shutdown returns *ShutdownIncompleteError and
//     Fx reports it as a lifecycle error, while the drain continues in the
//     background. Accepted work is never discarded to make shutdown look clean.
//   - A batcher that never started but holds queued work still drains, because
//     Shutdown starts the consumer when one is needed.
//   - Repeated application stop is idempotent: later Shutdown calls observe the
//     same terminal result.
func provideBatcherModule[T any](
	processorFactory any,
	buildOptions func(Processor[T]) []Option[T],
) fx.Option {
	return fx.Module(
		"batcher",
		fx.Provide(
			processorFactory,
			fx.Private,
		),
		fx.Provide(
			func(processorFunc Processor[T]) *Batcher[T] {
				options := buildOptions(processorFunc)

				// Fx owns the lifecycle, so auto-start is forced off last and cannot
				// be re-enabled by a caller-supplied option.
				options = append(options, WithSkipAutoStart[T]())

				return New(options...)
			},
		),
		fx.Invoke(
			func(lifecycle fx.Lifecycle, batcher *Batcher[T]) {
				lifecycle.Append(fx.StartHook(func(context.Context) error {
					batcher.Start()

					return nil
				}))

				lifecycle.Append(fx.StopHook(func(ctx context.Context) error {
					return batcher.Shutdown(ctx)
				}))
			},
		),
	)
}
