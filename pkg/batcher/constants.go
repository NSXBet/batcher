package batcher

import "time"

const (
	// DefaultBatchSize is the default batch size.
	DefaultBatchSize = 1000

	// DefaultBatchInterval is the maximum age of a partial batch.
	//
	// 10ms is the evidence-backed latency/coalescing default. The decision matrix
	// measured a 1s default adding up to ~1s p99 latency at 1k items/s, while 10ms
	// reduced it to ~22ms and still coalesced ~11 items per batch. At 10k items/s it
	// coalesced ~101 items per batch while reducing p99 ~100ms to ~10ms. See
	// docs/improvements/default-window.md for the complete matrix and caveats.
	//
	// This is a maximum partial-batch age, not overload protection. Services that
	// need larger batches at low traffic must set WithBatchInterval deliberately;
	// services that need process protection must use WithMaxQueueSize and Enqueue.
	DefaultBatchInterval = 10 * time.Millisecond

	// DefaultConcurrency is how many batches are processed at once by default.
	//
	// It is 1, not 3. The previous value of 3 was advertised but read nowhere, so
	// observed parallelism was always 1; changing the default to match reality
	// keeps existing users on the behaviour they already had. Raising it silently
	// would have parallelised every current user's processor without asking, which
	// is unsafe for a processor holding unsynchronised state.
	//
	// Values above 1 require WithoutOrderedProcessing, because they give up
	// cross-batch ordering and processor mutual exclusion.
	DefaultConcurrency = 1

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
