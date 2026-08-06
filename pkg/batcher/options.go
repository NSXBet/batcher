package batcher

import (
	"time"
)

type Option[T any] func(*Batcher[T])

// WithProcessor sets the processor function to be called for each batch.
func WithProcessor[T any](fn Processor[T]) Option[T] {
	return func(b *Batcher[T]) {
		b.config.ProcessorFunc = fn
	}
}

// WithBatchSize sets the batch size.
func WithBatchSize[T any](batchSize int) Option[T] {
	return func(b *Batcher[T]) {
		if batchSize <= 0 {
			batchSize = 1000
		}

		b.config.BatchSize = batchSize
	}
}

// WithBatchInterval sets the batch interval.
func WithBatchInterval[T any](batchInterval time.Duration) Option[T] {
	return func(b *Batcher[T]) {
		if batchInterval <= 0 {
			batchInterval = 1 * time.Second
		}

		b.config.BatchInterval = batchInterval
	}
}

// WithSkipAutoStart skips the automatic start of the batcher.
func WithSkipAutoStart[T any]() Option[T] {
	return func(b *Batcher[T]) {
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
// bounded by N + BatchSize + publishers currently inside the publication window,
// because a batch being processed has already left the queue.
func WithMaxQueueSize[T any](maxQueueSize int) Option[T] {
	return func(b *Batcher[T]) {
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
		if grace <= 0 {
			grace = DefaultCloseGrace
		}

		b.config.CloseGrace = grace
	}
}

// WithErrorBufferSize sets how many diagnostics are retained before the oldest
// retained errors are kept and newer ones dropped.
//
// The buffer is bounded on purpose. A blocking error channel would deadlock the
// pipeline whenever a processor fails and nobody reads Errors(), and an unbounded
// one would grow without limit during an outage. Stats().DroppedErrors reports the
// loss.
func WithErrorBufferSize[T any](size int) Option[T] {
	return func(b *Batcher[T]) {
		if size <= 0 {
			size = DefaultErrorBufferSize
		}

		b.config.ErrorBufferSize = size
	}
}
