package batcher

import (
	"fmt"
	"math"
	"reflect"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/destel/rill"
	"golang.design/x/chann"
)

var ErrTimeout = fmt.Errorf("timeout waiting for batches to complete")

type Processor[T any] func([]T) error

func NoOpProcessor[T any]([]T) error {
	return nil
}

type Config[T any] struct {
	SkipAutoStart  bool
	BatchSize      int
	BatchInterval  time.Duration
	Concurrency    int
	ProcessorFunc  Processor[T]
	BatchSizeBytes int64
}

type Batcher[T any] struct {
	config *Config[T]

	isClosed    uint32
	closeOnce   sync.Once
	itemCount   *AtomicCounter
	doneChan    chan struct{}
	errorsChan  *chann.Chann[error]
	batchesChan chan rill.Try[[]T]

	currentBatch     []T
	currentBatchSize int64
	batchMutex       sync.Mutex
	batchTimer       *time.Timer
}

// New creates a new Batcher with the given options.
func New[T any](options ...Option[T]) *Batcher[T] {
	b := &Batcher[T]{
		config: &Config[T]{
			BatchSize:      DefaultBatchSize,
			BatchInterval:  DefaultBatchInterval,
			Concurrency:    DefaultConcurrency,
			ProcessorFunc:  NoOpProcessor[T],
			BatchSizeBytes: DefaultBatchSizeBytes,
		},

		itemCount:    NewAtomicCounter(),
		doneChan:     make(chan struct{}),
		batchesChan:  make(chan rill.Try[[]T], 10),
		currentBatch: make([]T, 0, DefaultBatchSize),
	}

	for _, option := range options {
		option(b)
	}

	b.errorsChan = chann.New[error](chann.Cap(-1))

	if !b.config.SkipAutoStart {
		go b.startProcessing()
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
	if atomic.LoadUint32(&b.isClosed) != 0 {
		return
	}

	itemSize := b.CalculateItemSize(item)

	b.batchMutex.Lock()
	defer b.batchMutex.Unlock()

	if atomic.LoadUint32(&b.isClosed) != 0 {
		return
	}

	shouldFlush := len(b.currentBatch) >= b.config.BatchSize ||
		b.currentBatchSize+itemSize >= b.config.BatchSizeBytes

	if shouldFlush && len(b.currentBatch) > 0 {
		b.flushCurrentBatch()
	}

	b.currentBatch = append(b.currentBatch, item)
	b.currentBatchSize += itemSize
	b.itemCount.Add(1)

	if b.batchTimer == nil && len(b.currentBatch) == 1 {
		var timer *time.Timer
		timer = time.AfterFunc(b.config.BatchInterval, func() {
			b.flushBatchOnTimer(timer)
		})

		b.batchTimer = timer
	}
}

func (b *Batcher[T]) CalculateItemSize(item T) int64 {
	v := reflect.ValueOf(item)
	visited := make(map[uintptr]bool)
	return int64(b.sizeOfWithVisited(v, visited))
}

func (b *Batcher[T]) sizeOfWithVisited(v reflect.Value, visited map[uintptr]bool) uintptr {
	switch v.Kind() {
	case reflect.String:
		return unsafe.Sizeof("") + uintptr(v.Len())

	case reflect.Slice:
		if v.IsNil() {
			return unsafe.Sizeof([]int{})
		}

		addr := v.Pointer()
		if visited[addr] {
			return unsafe.Sizeof([]int{})
		}
		visited[addr] = true

		elemSize := uintptr(0)
		if v.Len() > 0 {
			elemSize = b.sizeOfWithVisited(v.Index(0), visited)
		}

		return unsafe.Sizeof([]int{}) + uintptr(v.Len())*elemSize

	case reflect.Map:
		if v.IsNil() {
			return unsafe.Sizeof(map[int]int{})
		}

		addr := v.Pointer()
		if visited[addr] {
			return unsafe.Sizeof(map[int]int{})
		}
		visited[addr] = true

		size := unsafe.Sizeof(map[int]int{})
		for _, key := range v.MapKeys() {
			size += b.sizeOfWithVisited(key, visited) + b.sizeOfWithVisited(v.MapIndex(key), visited)
		}

		return size

	case reflect.Struct:
		size := uintptr(0)
		for i := 0; i < v.NumField(); i++ {
			size += b.sizeOfWithVisited(v.Field(i), visited)
		}

		return size

	case reflect.Ptr:
		if v.IsNil() {
			return unsafe.Sizeof(uintptr(0))
		}

		addr := v.Pointer()
		if visited[addr] {
			return unsafe.Sizeof(uintptr(0))
		}
		visited[addr] = true

		return unsafe.Sizeof(uintptr(0)) + b.sizeOfWithVisited(v.Elem(), visited)

	default:
		return v.Type().Size()
	}
}

func (b *Batcher[T]) flushBatchOnTimer(timer *time.Timer) {
	b.batchMutex.Lock()
	defer b.batchMutex.Unlock()

	if atomic.LoadUint32(&b.isClosed) != 0 {
		return
	}

	if b.batchTimer == timer && len(b.currentBatch) > 0 {
		b.flushCurrentBatch()
	}
}

func (b *Batcher[T]) flushCurrentBatch() {
	if atomic.LoadUint32(&b.isClosed) != 0 {
		return
	}

	if len(b.currentBatch) == 0 {
		return
	}

	batch := make([]T, len(b.currentBatch))
	copy(batch, b.currentBatch)

	select {
	case <-b.doneChan:
		return
	default:
	}

	select {
	case b.batchesChan <- rill.Try[[]T]{Value: batch}:
	case <-b.doneChan:
		return
	}

	b.currentBatch = b.currentBatch[:0]
	b.currentBatchSize = 0

	if b.batchTimer != nil {
		b.batchTimer.Stop()
		b.batchTimer = nil
	}
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
	defer close(b.batchesChan)

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
	var clErr error

	b.closeOnce.Do(func() {
		b.batchMutex.Lock()
		if len(b.currentBatch) > 0 {
			b.flushCurrentBatch()
		}
		b.batchMutex.Unlock()

		timeout := time.Duration(2*2*math.Ceil(float64(b.Len())/float64(b.config.BatchSize))) *
			b.config.BatchInterval
		clErr = b.Join(timeout)

		atomic.StoreUint32(&b.isClosed, 1)
		close(b.doneChan)
	})

	return clErr
}

func (b *Batcher[T]) IsClosed() bool {
	return atomic.LoadUint32(&b.isClosed) != 0
}
