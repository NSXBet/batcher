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
// The processor always comes from the injected factory. A caller-supplied
// WithProcessor is ignored rather than honoured: the injected processor is applied
// last, so dependency injection cannot be bypassed by accident. Every other option
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
			// The injected processor goes LAST so it cannot be overridden. Placing it
			// first meant a caller-supplied WithProcessor silently replaced it and
			// bypassed dependency injection entirely — the documented restriction was
			// not enforced, so the failure was invisible rather than loud.
			return append(append([]Option[T]{}, options...), WithProcessor(processorFunc))
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
//   - Repeated application stop is idempotent: later Shutdown calls wait on the
//     same drain and return their own wait result. A caller that waits long enough
//     sees nil even if an earlier caller timed out; an expired deadline is never
//     stored and replayed to later callers.
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

				// Fx owns the lifecycle, so auto-start is forced off last: a
				// caller-supplied WithSkipAutoStart cannot re-enable it, because the
				// flag is only read after every option has run.
				//
				// Option is an arbitrary closure rather than a declarative value, so
				// a caller could also try to start the batcher directly from an
				// option. That is handled in Start, which is inert until New has
				// finished wiring the batcher, not here.
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
