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
