package batcher

import (
	"time"
)

// Option configures a Batcher during New.
//
// Options are construction-time only. New applies them, validates the resulting
// configuration, and freezes it before any processing goroutine can start. Calling
// an Option on a Batcher after New returns is a deliberate no-op: runtime
// reconfiguration was never coherent (the pipeline snapshots its configuration at
// start), and allowing it would reintroduce races against workers.
//
// To change configuration, construct a new Batcher. In particular, WithSkipAutoStart
// delays lifecycle start; it does not leave the configuration mutable until Start.
type Option[T any] func(*Batcher[T])

// WithProcessor sets the processor function to be called for each batch.
func WithProcessor[T any](fn Processor[T]) Option[T] {
	return func(b *Batcher[T]) {
		if b.configFrozen.Load() {
			return
		}

		b.config.ProcessorFunc = fn
	}
}

// WithBatchSize sets how many items a batch holds before it is flushed immediately.
//
// A non-positive value falls back to DefaultBatchSize.
func WithBatchSize[T any](batchSize int) Option[T] {
	return func(b *Batcher[T]) {
		if b.configFrozen.Load() {
			return
		}

		if batchSize <= 0 {
			batchSize = DefaultBatchSize
		}

		b.config.BatchSize = batchSize
	}
}

// WithBatchInterval sets the maximum age of a partial batch.
//
// The timer starts when the first item enters an empty batch, so this is a per-item
// worst-case wait for sparse traffic rather than a periodic flush tick.
//
// Under steady traffic, expected batch size is approximately
// min(BatchSize, arrival rate x interval): the interval bounds how long a partial
// batch waits, but BatchSize can flush it sooner. At high arrival rates the size
// limit is the binding constraint and the interval never fires.
//
// A non-positive value falls back to DefaultBatchInterval.
func WithBatchInterval[T any](batchInterval time.Duration) Option[T] {
	return func(b *Batcher[T]) {
		if b.configFrozen.Load() {
			return
		}

		if batchInterval <= 0 {
			batchInterval = DefaultBatchInterval
		}

		b.config.BatchInterval = batchInterval
	}
}

// WithSkipAutoStart skips automatic lifecycle start.
//
// It does not defer configuration freeze: options are still applied only during New.
// Call Start when ready to process, or Shutdown to drain queued work without an
// explicit Start.
func WithSkipAutoStart[T any]() Option[T] {
	return func(b *Batcher[T]) {
		if b.configFrozen.Load() {
			return
		}

		b.config.SkipAutoStart = true
	}
}

// WithMaxQueueSize bounds the number of queued items.
//
// The default is unbounded, which absorbs bursts but converts a sustained
// overload into unbounded memory growth. Bounding it gives back-pressure instead:
// Add blocks while the queue is full, and Enqueue can report the condition.
//
// Note this bounds queued items only. Total accepted-but-unfinished work is
// bounded by:
//
//	N + 2*BatchSize + publishers currently inside the publication window
//
// Two batches, not one: the aggregator holds a partial batch that has left the
// queue but not yet been dispatched, and a second batch can be inside the
// processor at the same time. Sizing memory from N alone therefore understates
// the peak by up to two batches.
func WithMaxQueueSize[T any](maxQueueSize int) Option[T] {
	return func(b *Batcher[T]) {
		if b.configFrozen.Load() {
			return
		}

		if maxQueueSize < 0 {
			maxQueueSize = 0
		}

		b.config.MaxQueueSize = maxQueueSize
	}
}

// WithCloseGrace sets how long Close waits for the drain to complete before
// reporting ErrTimeout. The drain is never abandoned when the grace expires.
func WithCloseGrace[T any](grace time.Duration) Option[T] {
	return func(b *Batcher[T]) {
		if b.configFrozen.Load() {
			return
		}

		if grace <= 0 {
			grace = DefaultCloseGrace
		}

		b.config.CloseGrace = grace
	}
}

// WithErrorBufferSize sets how many diagnostics the Errors() channel buffers.
//
// When the buffer is full, already-retained errors are kept and newer ones are
// dropped. Keeping the oldest is deliberate: the first errors of a storm usually
// carry the root cause, and dropping the oldest instead would mean racing the
// consumer for the right to discard.
//
// The buffer is bounded on purpose. A blocking error channel would deadlock the
// pipeline whenever a processor fails and nobody reads Errors(), and an unbounded
// one would grow without limit during an outage. Stats().DroppedErrors reports the
// loss.
func WithErrorBufferSize[T any](size int) Option[T] {
	return func(b *Batcher[T]) {
		if b.configFrozen.Load() {
			return
		}

		if size <= 0 {
			size = DefaultErrorBufferSize
		}

		b.config.ErrorBufferSize = size
	}
}

// WithConcurrency sets how many batches may be processed at once.
//
// The default is 1, which guarantees the processor is never invoked concurrently
// and that batches are processed in publication order.
//
// Values above 1 require WithoutOrderedProcessing as well. Construction panics
// otherwise: raising concurrency silently discards two guarantees callers may be
// relying on, so the trade has to be acknowledged in the code that makes it, not
// discovered in production. See WithoutOrderedProcessing for what is given up.
//
// Concurrency above 1 is what stops a slow processor from bounding the effective
// batch interval. At n = 1 the steady-state flush interval is
// max(BatchInterval, processor duration), because one batch must finish before the
// next can start.
//
// Per-item worst case is worse than that maximum, and additive rather than a
// maximum. While the aggregator is blocked handing a batch to the busy worker, the
// interval timer is not running: it is armed only when the next batch takes its
// first item. An item that arrives during the blocked window therefore waits the
// remaining processor time *plus* a full BatchInterval.
func WithConcurrency[T any](concurrency int) Option[T] {
	return func(b *Batcher[T]) {
		if b.configFrozen.Load() {
			return
		}

		if concurrency < 1 {
			concurrency = 1
		}

		b.config.Concurrency = concurrency
	}
}

// WithoutOrderedProcessing acknowledges that concurrent processing gives up
// ordering guarantees. It changes no behaviour on its own.
//
// It exists purely so that enabling concurrency is explicit about its cost. With
// WithConcurrency(n) for n > 1:
//
//   - batches may start, interleave, and complete in any order;
//   - the processor may be invoked concurrently, so it must be goroutine-safe.
//
// What is retained at any concurrency:
//
//   - items keep publication order *within* each batch;
//   - every accepted item is processed exactly once by the pool itself.
func WithoutOrderedProcessing[T any]() Option[T] {
	return func(b *Batcher[T]) {
		if b.configFrozen.Load() {
			return
		}

		b.config.UnorderedProcessingAcknowledged = true
	}
}
