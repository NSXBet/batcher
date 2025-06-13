package batcher

import "time"

const (
	// DefaultBatchSize is the default batch size.
	DefaultBatchSize = 1000

	// DefaultBatchInterval is the default batch interval.
	DefaultBatchInterval = 1 * time.Second

	// DefaultConcurrency is the default concurrency.
	DefaultConcurrency = 3

	// DefaultBatchSizeBytes is the default batch size in bytes.
	DefaultBatchSizeBytes = 10 * 1024 * 1024 // 10MB
)
