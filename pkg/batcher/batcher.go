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
	MaxQueueSize  int
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
	queueSemaphore chan struct{} // nil when unbounded
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

		itemCount: NewAtomicCounter(),
		doneChan:  make(chan struct{}),
	}

	for _, option := range options {
		option(b)
	}

	b.batchInputChan = chann.New[rill.Try[T]](chann.Cap(-1))

	if b.config.MaxQueueSize > 0 {
		b.queueSemaphore = make(chan struct{}, b.config.MaxQueueSize)
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
	if b.queueSemaphore != nil {
		b.queueSemaphore <- struct{}{} // blocks when queue is full
	}

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

			if b.queueSemaphore != nil {
				for range len(batch.Value) {
					<-b.queueSemaphore
				}
			}
		}
	}
}

func (b *Batcher[T]) Errors() <-chan error {
	return b.errorsChan.Out()
}

func (b *Batcher[T]) Close() error {
	var clErr error

	b.closeOnce.Do(func() {
		timeout := time.Duration(2*2*math.Ceil(float64(b.Len())/float64(b.config.BatchSize))) *
			b.config.BatchInterval
		clErr = b.Join(timeout)

		close(b.doneChan)

		b.isClosed = true
	})

	return clErr
}

func (b *Batcher[T]) IsClosed() bool {
	return b.isClosed
}
