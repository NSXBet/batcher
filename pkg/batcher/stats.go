package batcher

import "sync/atomic"

// Stats is a point-in-time snapshot of a Batcher's counters.
//
// Reading it is O(1) and allocation-free: it loads a fixed set of atomics and
// never takes a lock, reads a channel, or walks the queue. That is deliberate, so
// metrics scraping can run as often as needed without perturbing batching.
//
// The snapshot is eventually consistent, NOT transactional. Fields are individual
// atomic loads, so a state transition in flight can be observed partially. Two
// consequences worth stating plainly:
//
//   - Accepted == Completed + Failed + Panicked + Pending holds exactly only at
//     publisher quiescence (after shutdown observes an empty gate), not while load
//     is in flight.
//   - Pending is a conservative drain obligation. It counts publishers that have
//     reserved but not yet published, so it can briefly exceed accepted work, and
//     it is not a queue depth. Use Queued for depth.
type Stats struct {
	// Pending is accepted-or-reserved work that has not reached a terminal
	// outcome. The drain waits for this to reach zero.
	Pending int64

	// IntakePending is work the aggregator has not yet received. It is the
	// counter that decides when intake is exhausted during shutdown.
	IntakePending int64

	// PublishersInGate is how many goroutines are currently inside the
	// publication window. Zero means no publisher can still be mid-publish.
	PublishersInGate int64

	// Queued is items sitting in the intake queue: published, not yet received by
	// the aggregator. This is the queue depth to alert on.
	Queued int64

	// Accepted counts successful publications. A rejected or cancelled enqueue
	// never increments it.
	Accepted uint64

	// Completed, Failed and Panicked are mutually exclusive terminal outcomes,
	// counted in items. Failed means the processor returned a non-nil error;
	// Panicked means it panicked and the panic was recovered.
	Completed uint64
	Failed    uint64
	Panicked  uint64

	// Rejected counts enqueue attempts refused because admission was sealed, a
	// bounded queue was full for a non-blocking Add, or the caller's context
	// expired.
	Rejected uint64

	// DroppedErrors counts diagnostics discarded because the Errors() buffer was
	// full. A non-zero value means diagnostics are being lost, not that batches
	// failed: it is a signal that Errors() is not being drained fast enough.
	DroppedErrors uint64
}

// counters holds the live atomics behind Stats.
//
// Placement is a performance decision. Only the reservation and acceptance
// counters are touched per item on the enqueue path; every terminal counter is
// updated once per batch on the processing path, where the cost amortises. Four
// contended atomics per Add measured up to 4x the cost of one, so nothing is added
// to the hot path without a reason.
type counters struct {
	pending       atomic.Int64
	intakePending atomic.Int64
	accepted      atomic.Uint64
	completed     atomic.Uint64
	failed        atomic.Uint64
	panicked      atomic.Uint64
	rejected      atomic.Uint64
	droppedErrors atomic.Uint64
}

// reserve claims a drain obligation before publication.
//
// Reserving first is what stops the drain from concluding while a publisher is
// still between "decided to publish" and "published".
func (c *counters) reserve() {
	c.pending.Add(1)
	c.intakePending.Add(1)
}

// rollback releases a reservation that never became an accepted item.
//
// This MUST run before the publisher leaves the admission gate. If it ran after,
// the coordinator could see an empty gate while a phantom intake obligation
// remained, and the final drain would wait for an item that will never arrive.
func (c *counters) rollback() {
	c.pending.Add(-1)
	c.intakePending.Add(-1)
	c.rejected.Add(1)
}

// accept records a successful publication.
func (c *counters) accept() {
	c.accepted.Add(1)
}

// received records the aggregator taking an item out of the queue.
func (c *counters) received(n int) {
	c.intakePending.Add(int64(-n))
}

// terminal records exactly one outcome for a finished batch and releases its
// drain obligation.
func (c *counters) terminal(outcome outcomeKind, n int) {
	switch outcome {
	case outcomeCompleted:
		c.completed.Add(uint64(n))
	case outcomeFailed:
		c.failed.Add(uint64(n))
	case outcomePanicked:
		c.panicked.Add(uint64(n))
	}

	c.pending.Add(int64(-n))
}

// outcomeKind enumerates the mutually exclusive terminal outcomes for a batch.
type outcomeKind int

const (
	outcomeCompleted outcomeKind = iota
	outcomeFailed
	outcomePanicked
)
