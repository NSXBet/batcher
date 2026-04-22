package batcher

import (
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/destel/rill"
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
	batchInputChan *chann.Chann[rill.Try[T]]
	batchesChan    <-chan rill.Try[[]T]
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
		batchInputChan: chann.New[rill.Try[T]](chann.Cap(-1)),
	}

	for _, option := range options {
		option(b)
	}

	b.errorsChan = chann.New[error](chann.Cap(-1))

	batchOutput := rill.Batch(b.batchInputChan.Out(), b.config.BatchSize, b.config.BatchInterval)
	b.batchesChan = batchOutput

	if !b.config.SkipAutoStart {
		b.Start()
	}

	return b
}

func (b *Batcher[T]) Start() {
	go b.startProcessing()
}

func (b *Batcher[T]) Config() *Config[T] {
	return b.config
}

func (b *Batcher[T]) Add(item T) {
	b.batchInputChan.In() <- rill.Try[T]{Value: item}
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

func (b *Batcher[T]) startProcessing() {
	// Close channels in the correct order to prevent goroutine leaks:
	// 1. First close the input channel to stop the rill pipeline from producing more batches
	// 2. Then drain the output channel to allow rill goroutines to exit cleanly
	// 3. Finally close the errors channel
	defer b.errorsChan.Close()
	defer func() {
		// Drain any remaining batches from the rill pipeline to ensure
		// all rill goroutines can exit cleanly. This prevents goroutine leaks.
		for range b.batchesChan {
			// Just drain, don't process during shutdown
		}
	}()
	defer b.batchInputChan.Close()

	for {
		select {
		case <-b.doneChan:
			return
		case batch, ok := <-b.batchesChan:
			if !ok {
				// Channel closed, exit gracefully
				return
			}

			if batch.Error != nil {
				b.errorsChan.In() <- batch.Error

				continue
			}

			if err := b.config.ProcessorFunc(batch.Value); err != nil {
				b.errorsChan.In() <- err
			}

			b.itemCount.Add(int64(-len(batch.Value)))
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
