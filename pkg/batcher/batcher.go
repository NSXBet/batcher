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
	b.itemCount.Add(1)
	b.batchInputChan.In() <- item
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
	defer b.errorsChan.Close()
	defer b.batchInputChan.Close()

	var (
		batch  []T
		timer  *time.Timer
		timerC <-chan time.Time
	)

	stopTimer := func() {
		if timer == nil {
			return
		}

		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}

		timerC = nil
	}

	flush := func() {
		if len(batch) == 0 {
			return
		}

		items := batch
		batch = nil
		stopTimer()

		if err := b.config.ProcessorFunc(items); err != nil {
			b.errorsChan.In() <- err
		}

		b.itemCount.Add(int64(-len(items)))
	}

	for {
		select {
		case <-b.doneChan:
			flush()
			return
		case item, ok := <-b.batchInputChan.Out():
			if !ok {
				flush()
				return
			}

			if len(batch) == 0 {
				batch = make([]T, 0, b.config.BatchSize)
				if timer == nil {
					timer = time.NewTimer(b.config.BatchInterval)
				} else {
					timer.Reset(b.config.BatchInterval)
				}
				timerC = timer.C
			}

			batch = append(batch, item)

			if len(batch) == b.config.BatchSize {
				flush()
			}
		case <-timerC:
			flush()
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
