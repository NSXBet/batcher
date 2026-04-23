package batcher

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"golang.design/x/chann"
)

var ErrTimeout = fmt.Errorf("timeout waiting for batches to complete")
var ErrClosing = errors.New("batcher is closing")

type Processor[T any] func([]T) error

func NoOpProcessor[T any]([]T) error {
	return nil
}

type Config[T any] struct {
	SkipAutoStart bool
	BatchSize     int
	BatchInterval time.Duration
	MaxQueueSize  int
	Concurrency   int
	ProcessorFunc Processor[T]
}

type Batcher[T any] struct {
	config *Config[T]

	isClosed       bool
	started        atomic.Bool
	startOnce      sync.Once
	closeOnce      sync.Once
	itemCount      *AtomicCounter
	stopAccepting  chan struct{}
	doneChan       chan struct{}
	processingDone chan struct{}
	errorsChan     *chann.Chann[error]
	boundedInputCh chan T
	batchInputChan *chann.Chann[T]
}

// New creates a new Batcher with the given options.
func New[T any](options ...Option[T]) *Batcher[T] {
	b := &Batcher[T]{
		config: &Config[T]{
			BatchSize:     DefaultBatchSize,
			BatchInterval: DefaultBatchInterval,
			MaxQueueSize:  0,
			Concurrency:   DefaultConcurrency,
			ProcessorFunc: NoOpProcessor[T],
		},

		itemCount:      NewAtomicCounter(),
		stopAccepting:  make(chan struct{}),
		doneChan:       make(chan struct{}),
		processingDone: make(chan struct{}),
	}

	for _, option := range options {
		option(b)
	}

	if b.config.MaxQueueSize > 0 {
		b.boundedInputCh = make(chan T, b.config.MaxQueueSize)
	} else {
		b.batchInputChan = chann.New[T](chann.Cap(-1))
	}

	b.errorsChan = chann.New[error](chann.Cap(-1))

	if !b.config.SkipAutoStart {
		b.Start()
	}

	return b
}

func (b *Batcher[T]) Start() {
	select {
	case <-b.stopAccepting:
		return
	default:
	}

	b.startOnce.Do(func() {
		b.started.Store(true)
		go b.startProcessing()
	})
}

func (b *Batcher[T]) Config() *Config[T] {
	return b.config
}

func (b *Batcher[T]) Add(item T) {
	_ = b.Enqueue(context.Background(), item)
}

func (b *Batcher[T]) Enqueue(ctx context.Context, item T) error {
	inputCh := b.inputSendCh()

	if ctx == nil {
		ctx = context.Background()
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	select {
	case <-b.stopAccepting:
		return ErrClosing
	default:
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-b.stopAccepting:
		return ErrClosing
	case inputCh <- item:
		b.itemCount.Add(1)
		return nil
	}
}

func (b *Batcher[T]) inputSendCh() chan<- T {
	if b.boundedInputCh != nil {
		return b.boundedInputCh
	}

	return b.batchInputChan.In()
}

func (b *Batcher[T]) inputRecvCh() <-chan T {
	if b.boundedInputCh != nil {
		return b.boundedInputCh
	}

	return b.batchInputChan.Out()
}

func (b *Batcher[T]) closeInput() {
	if b.batchInputChan != nil {
		b.batchInputChan.Close()
	}
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
	defer close(b.processingDone)
	defer b.errorsChan.Close()

	inputCh := b.inputRecvCh()

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
			return
		case item, ok := <-inputCh:
			if !ok {
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
		close(b.stopAccepting)

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

		if b.started.Load() {
			b.closeInput()

			// Don't block forever if the processor is stuck in user code.
			select {
			case <-b.processingDone:
			case <-time.After(50 * time.Millisecond):
			}
		} else {
			b.closeInput()
			b.errorsChan.Close()
			close(b.processingDone)
		}

		b.isClosed = true
	})

	return clErr
}

func (b *Batcher[T]) IsClosed() bool {
	return b.isClosed
}
