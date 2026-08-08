package batcher

import "time"

const (
	// DefaultBatchSize is the default batch size.
	DefaultBatchSize = 1000

	// DefaultBatchInterval is the default batch interval.
	DefaultBatchInterval = 1 * time.Second

	// DefaultConcurrency is the default concurrency.
	DefaultConcurrency = 3

	// DefaultCloseGrace is how long Close waits for the drain to finish before
	// reporting that it is incomplete. The drain continues regardless.
	//
	// A flat value replaces the previous formula derived from queue size and batch
	// interval, which was impossible to reason about and was the direct cause of
	// accepted work being discarded when the interval exceeded its 10s cap.
	DefaultCloseGrace = 30 * time.Second

	// DefaultErrorBufferSize bounds retained diagnostics. The channel must never
	// block the pipeline, so a full buffer drops rather than waits.
	DefaultErrorBufferSize = 1024
)
