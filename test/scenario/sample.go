// Package scenario provides an open-loop load harness for measuring Batcher's
// end-to-end behaviour.
//
// The harness exists because Go benchmarks alone cannot answer the questions
// this project needs answered. `ns/op` is a mean over a closed loop, and a
// closed loop stops offering load exactly when the system slows down. That
// hides the failure mode we care about most: a slow processor under sustained
// arrivals. This package therefore drives load from a precomputed schedule that
// never waits for completion, and reports distributions rather than means.
//
// Terminology used throughout:
//
//   - scheduled_at: when an item was due to be offered, fixed before the run.
//   - admission_start/admission_end: the window spent inside Add or Enqueue.
//   - processor_start/processor_end: the window spent inside the user processor.
//   - lateness: admission_start - scheduled_at. If the generator itself cannot
//     keep up, lateness rises and the run's latency numbers become meaningless;
//     the harness marks such runs invalid instead of reporting them.
package scenario

import (
	"sort"
	"time"
)

// Sample is one item's journey through the batcher. Times are monotonic
// readings captured on the producing and processing goroutines.
type Sample struct {
	ScheduledAt    time.Duration // relative to run start
	AdmissionStart time.Duration
	AdmissionEnd   time.Duration
	ProcessorStart time.Duration
	ProcessorEnd   time.Duration

	// Accepted is false when admission rejected or dropped the item. Rejected
	// items carry no processor timings and are excluded from latency stats.
	Accepted bool
}

// Lateness reports how far behind schedule the generator was when it offered
// this item. A healthy run keeps this far below the batch window.
func (s Sample) Lateness() time.Duration {
	return s.AdmissionStart - s.ScheduledAt
}

// AdmissionBlocking reports time spent parked inside Add or Enqueue, which is
// backpressure rather than batching delay.
func (s Sample) AdmissionBlocking() time.Duration {
	return s.AdmissionEnd - s.AdmissionStart
}

// EndToEnd reports the latency a caller ultimately experiences: from the moment
// the item was due to be sent until its batch finished processing.
func (s Sample) EndToEnd() time.Duration {
	return s.ProcessorEnd - s.ScheduledAt
}

// QueueDelay reports time between successful admission and the processor
// starting on the item's batch. This is the portion attributable to batching
// and queueing rather than to the processor itself.
func (s Sample) QueueDelay() time.Duration {
	return s.ProcessorStart - s.AdmissionEnd
}

// Distribution summarises a set of durations. Percentiles are computed from raw
// samples; nothing here is derived from a mean.
type Distribution struct {
	Count int
	Min   time.Duration
	P50   time.Duration
	P95   time.Duration
	P99   time.Duration
	P999  time.Duration
	Max   time.Duration
	Mean  time.Duration
}

// NewDistribution sorts values in place and summarises them.
func NewDistribution(values []time.Duration) Distribution {
	if len(values) == 0 {
		return Distribution{}
	}

	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })

	var total time.Duration
	for _, v := range values {
		total += v
	}

	return Distribution{
		Count: len(values),
		Min:   values[0],
		P50:   quantile(values, 0.50),
		P95:   quantile(values, 0.95),
		P99:   quantile(values, 0.99),
		P999:  quantile(values, 0.999),
		Max:   values[len(values)-1],
		Mean:  total / time.Duration(len(values)),
	}
}

// quantile expects values to be sorted ascending.
func quantile(values []time.Duration, q float64) time.Duration {
	if len(values) == 0 {
		return 0
	}

	idx := int(float64(len(values)-1) * q)
	if idx < 0 {
		idx = 0
	}

	if idx >= len(values) {
		idx = len(values) - 1
	}

	return values[idx]
}

// IntDistribution summarises a set of counts, such as batch sizes.
//
// This exists rather than reusing Distribution because batch sizes are items, not
// durations. Storing them as time.Duration to reuse one helper made a mean batch of
// 455 print as "455ns" in any formatted output, which is the kind of unit confusion
// that ends up quoted in a decision record.
type IntDistribution struct {
	Count int
	Min   int
	P50   int
	P95   int
	P99   int
	Max   int
	Mean  float64
}

// NewIntDistribution sorts values in place and summarises them.
func NewIntDistribution(values []int) IntDistribution {
	if len(values) == 0 {
		return IntDistribution{}
	}

	sort.Ints(values)

	total := 0
	for _, v := range values {
		total += v
	}

	return IntDistribution{
		Count: len(values),
		Min:   values[0],
		P50:   intQuantile(values, 0.50),
		P95:   intQuantile(values, 0.95),
		P99:   intQuantile(values, 0.99),
		Max:   values[len(values)-1],
		Mean:  float64(total) / float64(len(values)),
	}
}

// intQuantile expects values to be sorted ascending.
func intQuantile(values []int, q float64) int {
	if len(values) == 0 {
		return 0
	}

	idx := int(float64(len(values)-1) * q)
	if idx < 0 {
		idx = 0
	}

	if idx >= len(values) {
		idx = len(values) - 1
	}

	return values[idx]
}
