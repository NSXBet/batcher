package batcher

import (
	"fmt"
	"math"
	"sync"
	"time"

	"golang.design/x/chann"
)

var ErrTimeout = fmt.Errorf("timeout waiting for batches to complete")

type Processor[T any] func([]T) error

func NoOpProcessor[T any]([]T) error {
	return nil
}

type Config[T any] struct {
	SkipAutoStart bool
	BatchSize     int
	BatchInterval time.Duration
	Concurrency   int
	ProcessorFunc Processor[T]
}

type Batcher[T any] struct {
	config *Config[T]

	isClosed       bool
	closeOnce      sync.Once
	itemCount      *AtomicCounter
	doneChan       chan struct{}
	errorsChan     *chann.Chann[error]
	batchInputChan *chann.Chann[T]
	batchesChan    <-chan []T
}

// New creates a new Batcher with the given options.
func New[T any](options ...Option[T]) *Batcher[T] {
	b := &Batcher[T]{
		config: &Config[T]{
			BatchSize:     DefaultBatchSize,
			BatchInterval: DefaultBatchInterval,
			Concurrency:   DefaultConcurrency,
			ProcessorFunc: NoOpProcessor[T],
		},

		itemCount:      NewAtomicCounter(),
		doneChan:       make(chan struct{}),
		batchInputChan: chann.New[T](chann.Cap(-1)),
	}

	for _, option := range options {
		option(b)
	}

	b.errorsChan = chann.New[error](chann.Cap(-1))
	b.batchesChan = aggregate(
		forward(b.batchInputChan.Out()),
		b.config.BatchSize,
		b.config.BatchInterval,
	)

	if !b.config.SkipAutoStart {
		b.Start()
	}

	return b
}

// forward relays items from the unbounded input queue onto an unbuffered channel,
// closing it when the source is closed.
//
// This stage exists for a measured reason, not for symmetry. The unbounded queue
// has a small bounded ingress buffer, so its output has to be consumed promptly
// or producers start paying for the backlog. When the aggregator read that output
// directly, sequential Add regressed 39-50% against the stored baseline, because
// the aggregator interleaves reads with timer and slice bookkeeping. Splitting the
// drain out restored parity. It is the same separation rill's pipeline provided.
//
// Phase 2.2 removes this stage along with the unbounded queue itself: an owned
// queue can be read directly without a relay.
func forward[T any](in <-chan T) <-chan T {
	out := make(chan T)

	go func() {
		defer close(out)

		for item := range in {
			out <- item
		}
	}()

	return out
}

// aggregate groups items from in into batches, emitting a batch when it reaches
// size, when interval elapses, or when in is closed.
//
// This replaces rill.Batch and deliberately preserves its semantics, which the
// characterization tests pin:
//
//   - The interval timer is armed when the first item enters an empty batch, not
//     on a periodic tick. A sparse producer therefore waits one full interval per
//     item, and an idle batcher performs no work at all.
//   - A batch reaching n is emitted immediately, without waiting for the timer.
//   - Empty batches are never emitted, so a processor always receives at least
//     one item.
//   - Closing in flushes any pending partial batch, then closes out.
//
// Aggregation runs in its own goroutine and hands batches over an unbuffered
// channel. That separation is load-bearing: it lets the next batch accumulate
// while the current one is being processed. Merging the two stages would make the
// effective flush interval max(interval, processor duration) and change latency
// for every caller, which is a later phase's decision, not this one's.
func aggregate[T any](in <-chan T, n int, interval time.Duration) <-chan []T {
	out := make(chan []T)

	go func() {
		defer close(out)

		var (
			batch []T
			timer *time.Timer
			// timerC stays nil while the batch is empty, disabling that select arm
			// rather than relying on a stopped timer never firing.
			timerC <-chan time.Time
		)

		stopTimer := func() {
			timerC = nil

			if timer == nil {
				return
			}

			if !timer.Stop() {
				// Drain a tick that fired while we were emitting, so the next batch
				// cannot inherit a stale expiry.
				select {
				case <-timer.C:
				default:
				}
			}
		}

		flush := func() {
			if len(batch) == 0 {
				return
			}

			items := batch
			batch = nil

			stopTimer()

			out <- items
		}

		for {
			select {
			case item, ok := <-in:
				if !ok {
					flush()

					return
				}

				if len(batch) == 0 {
					batch = make([]T, 0, n)

					if timer == nil {
						timer = time.NewTimer(interval)
					} else {
						timer.Reset(interval)
					}

					timerC = timer.C
				}

				batch = append(batch, item)

				if len(batch) >= n {
					flush()
				}
			case <-timerC:
				flush()
			}
		}
	}()

	return out
}

func (b *Batcher[T]) Start() {
	go b.startProcessing()
}

func (b *Batcher[T]) Config() *Config[T] {
	return b.config
}

func (b *Batcher[T]) Add(item T) {
	b.batchInputChan.In() <- item
	b.itemCount.Add(1)
}

func (b *Batcher[T]) Len() int {
	return int(b.itemCount.Read())
}

func (b *Batcher[T]) Join(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)

	for {
		if b.Len() == 0 {
			return nil
		}

		if time.Now().After(deadline) {
			return ErrTimeout
		}

		time.Sleep(1 * time.Millisecond)
	}
}

// startProcessing consumes aggregated batches and invokes the processor for each
// one.
//
// Teardown order matters and is preserved from the previous implementation:
//
//  1. close the input queue, so aggregation sees end-of-input and flushes its
//     pending partial batch;
//  2. drain the batch channel, so the aggregation goroutine can exit rather than
//     blocking forever on an unbuffered send;
//  3. close the errors channel last, once no further error can be published.
//
// Batches drained during step 2 after a shutdown signal are discarded without
// being processed. That is existing behaviour, and it is the data-loss path a
// later milestone replaces; this commit does not change it.
func (b *Batcher[T]) startProcessing() {
	defer b.errorsChan.Close()
	defer func() {
		for range b.batchesChan {
			// Drain without processing so aggregation can finish and exit.
		}
	}()
	defer b.batchInputChan.Close()

	for {
		select {
		case <-b.doneChan:
			return
		case items, ok := <-b.batchesChan:
			if !ok {
				return
			}

			if err := b.config.ProcessorFunc(items); err != nil {
				b.errorsChan.In() <- err
			}

			// Errored batches still count as completed work, so Join cannot hang on
			// a processor that always fails. Retry policy belongs to the caller.
			b.itemCount.Add(int64(-len(items)))
		}
	}
}

func (b *Batcher[T]) Errors() <-chan error {
	return b.errorsChan.Out()
}

func (b *Batcher[T]) Close() error {
	var clErr error

	b.closeOnce.Do(func() {
		// Calculate timeout based on pending items, with a maximum of 10 seconds
		timeout := time.Duration(2*2*math.Ceil(float64(b.Len())/float64(b.config.BatchSize))) *
			b.config.BatchInterval

		// Cap the timeout at 10 seconds to prevent indefinite blocking
		maxTimeout := 10 * time.Second
		if timeout > maxTimeout {
			timeout = maxTimeout
		}

		// Ensure a minimum timeout of 100ms
		if timeout < 100*time.Millisecond {
			timeout = 100 * time.Millisecond
		}

		clErr = b.Join(timeout)

		// Signal shutdown to the processing goroutine
		close(b.doneChan)

		// Give the goroutine a moment to exit cleanly
		// This allows the deferred cleanup to run
		time.Sleep(50 * time.Millisecond)

		b.isClosed = true
	})

	return clErr
}

func (b *Batcher[T]) IsClosed() bool {
	return b.isClosed
}
