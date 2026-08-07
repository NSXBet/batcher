package batcher

import "time"

const (
	// DefaultBatchSize is the default batch size.
	DefaultBatchSize = 1000

	// DefaultBatchInterval is the maximum age of a partial batch.
	//
	// It stays at 1s. A smaller default would lower latency for callers who never
	// set an interval, but it raises the downstream call rate for all of them: the
	// measured matrix shows ~10 calls/s at 1s versus ~94-100 calls/s at 10ms once
	// the timer rather than BatchSize closes each batch. That is a CPU and
	// downstream-load change imposed on existing users who did not ask for it, so
	// the low-latency setting is opt-in rather than the default.
	//
	// Callers who want lower latency should set WithBatchInterval explicitly; 10ms
	// measured p99 ~12ms at 1k items/s while still coalescing ~11 items per batch.
	// docs/improvements/default-window.md holds the full matrix and its caveats, and
	// the README's "Choosing a batch interval" section carries the call-rate columns
	// for picking a value.
	//
	// This is a maximum partial-batch age, not overload protection. Services that
	// need process protection must use WithMaxQueueSize and Enqueue.
	DefaultBatchInterval = 1 * time.Second

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
