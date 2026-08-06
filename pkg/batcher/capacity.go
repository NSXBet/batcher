package batcher

// capacityEstimator chooses the initial capacity for the next batch slice.
//
// The aggregator allocates one slice per batch. Reserving BatchSize every time is
// correct but wasteful whenever batches close on the timer rather than on size: a
// 1ms window with BatchSize=1000 was measured reserving ~56KB per flush to hold a
// single item, and ~560KB at BatchSize=10000.
//
// The estimator tracks the maximum batch size seen in a recent window and rounds it
// up to a power of two, so capacity follows observed demand instead of the
// configured ceiling.
//
// Why recent-max rather than a mean:
//
// An EWMA of the mean was evaluated during planning and rejected. On traffic
// alternating between tiny and full batches it allocated *more* than doing nothing,
// because the mean sits between the two modes: every large batch then grows from a
// too-small start, and each growth reallocates and copies. Tracking the maximum
// means a workload containing large batches keeps capacity sized for them, so
// alternating traffic behaves like the full-capacity strategy rather than worse.
//
// Concurrency: this is owned exclusively by the aggregator goroutine and updated
// once per flush. It deliberately has no locks and no atomics; nothing else may
// touch it.
type capacityEstimator struct {
	// batchSize is the configured ceiling. Capacity never exceeds it, so a
	// processor that retains its slice never holds more than today's worst case.
	batchSize int

	// ceiling is the current power-of-two capacity, derived from recentMax. It
	// starts at batchSize for the first batch so a full-batch workload never pays
	// append growth before the estimator has any evidence. The first observed
	// sparse batch then adapts it downward.
	ceiling int

	// initialized records whether the first observed batch has set a data-driven
	// ceiling. It distinguishes the initial full-capacity safety value from a
	// ceiling reached because recent demand genuinely saturated BatchSize.
	initialized bool

	// recentMax is the largest batch observed in the current evaluation window.
	recentMax int

	// seen counts observations in the current window.
	seen int
}

const (
	// capacityFloor keeps very sparse traffic from reallocating on a batch that
	// happens to be slightly larger than the last. 16 items is small enough that
	// the floor itself is not meaningful waste.
	capacityFloor = 16

	// capacityWindow is how many flushes are observed before the ceiling is
	// re-derived. Long enough that a single small batch cannot collapse capacity
	// for a busy batcher, short enough that a batcher which quiets down releases
	// its oversized reservation promptly.
	capacityWindow = 32

	// capacityHeadroom scales the observed maximum before rounding, so a workload
	// sitting just above a power of two does not reallocate on every batch.
	capacityHeadroom = 5
	capacityDivisor  = 4 // headroom/divisor == 1.25x
)

func newCapacityEstimator(batchSize int) *capacityEstimator {
	if batchSize < 1 {
		batchSize = 1
	}

	return &capacityEstimator{
		batchSize: batchSize,
		// Start pessimistic: reserve the configured ceiling until there is evidence
		// that batches are smaller. Starting small instead made a size-triggered
		// workload pay repeated append growth on its very first batch, measured as a
		// 2.2% allocation regression against the full-capacity strategy.
		ceiling: batchSize,
	}
}

// capacity returns the initial capacity to allocate for the next batch.
func (e *capacityEstimator) capacity() int {
	return e.ceiling
}

// observe records the size of a completed batch.
//
// The ceiling rises immediately when a batch exceeds it, because under-allocating a
// large batch costs a reallocation and copy right away. It falls only at window
// boundaries, so capacity decays gradually rather than oscillating.
func (e *capacityEstimator) observe(size int) {
	if size > e.recentMax {
		e.recentMax = size
	}

	if !e.initialized {
		// First evidence: replace the pessimistic starting ceiling outright, so a
		// sparse workload stops reserving BatchSize after a single batch rather than
		// waiting for a full evaluation window.
		e.initialized = true
		e.ceiling = clampCapacity(roundUpPow2(scaleHeadroom(size)), e.batchSize)
	} else if size > e.ceiling {
		e.ceiling = clampCapacity(roundUpPow2(scaleHeadroom(size)), e.batchSize)
	}

	e.seen++

	if e.seen < capacityWindow {
		return
	}

	e.ceiling = clampCapacity(roundUpPow2(scaleHeadroom(e.recentMax)), e.batchSize)
	e.recentMax = 0
	e.seen = 0
}

// scaleHeadroom applies the headroom factor with integer arithmetic.
func scaleHeadroom(n int) int {
	if n <= 0 {
		return 0
	}

	return n * capacityHeadroom / capacityDivisor
}

// roundUpPow2 returns the smallest power of two at or above n.
func roundUpPow2(n int) int {
	if n <= capacityFloor {
		return capacityFloor
	}

	p := capacityFloor
	for p < n {
		next := p << 1
		// Stop before overflow; the caller clamps to batchSize anyway.
		if next <= p {
			return p
		}

		p = next
	}

	return p
}

// clampCapacity keeps capacity within [capacityFloor, batchSize].
//
// The upper clamp is a contract, not an optimisation: it bounds what a processor
// that retains its batch slice can hold.
func clampCapacity(n, batchSize int) int {
	if n > batchSize {
		return batchSize
	}

	if n < capacityFloor {
		if batchSize < capacityFloor {
			return batchSize
		}

		return capacityFloor
	}

	return n
}
