package batcher

import "sync/atomic"

type AtomicCounter struct {
	number int64
}

func NewAtomicCounter() *AtomicCounter {
	return &AtomicCounter{0}
}

func (c *AtomicCounter) Add(num int64) {
	atomic.AddInt64(&c.number, num)
}

func (c *AtomicCounter) Read() int64 {
	return atomic.LoadInt64(&c.number)
}

func (c *AtomicCounter) Reset() {
	atomic.StoreInt64(&c.number, 0)
}
