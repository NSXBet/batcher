package batcher

import (
	"fmt"
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

type BatcherConfig[T any] struct {
	BatchSize     int
	BatchInterval time.Duration
	Concurrency   int
	ProcessorFunc Processor[T]
}

type Batcher[T any] struct {
	config *BatcherConfig[T]

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
		config: &BatcherConfig[T]{
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

	go b.startProcessing()

	return b
}

func (b *Batcher[T]) Config() *BatcherConfig[T] {
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
	for {
		select {
		case <-time.After(timeout):
			return ErrTimeout
		default:
			if b.Len() == 0 {
				return nil
			}
			time.Sleep(1 * time.Millisecond)
		}
	}
}

func (b *Batcher[T]) startProcessing() {
	defer b.errorsChan.Close()
	defer b.batchInputChan.Close()

	for {
		select {
		case <-b.doneChan:
			return
		case batch := <-b.batchesChan:
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
	b.closeOnce.Do(func() {
		close(b.doneChan)
	})

	b.isClosed = true

	return nil
}

func (b *Batcher[T]) IsClosed() bool {
	return b.isClosed
}
