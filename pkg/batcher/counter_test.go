package batcher_test

import (
	"sync"
	"testing"

	"github.com/NSXBet/batcher/pkg/batcher"
	"github.com/stretchr/testify/require"
)

func TestCanAddToCounter(t *testing.T) {
	// ARRANGE
	n := 1000000
	counter := batcher.NewAtomicCounter()

	// ACT
	var wg sync.WaitGroup

	wg.Add(n)

	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()

			counter.Add(1)
		}()
	}

	wg.Wait()

	// ASSERT
	require.Equal(t, int64(n), counter.Read())
}

func TestCanResetCounter(t *testing.T) {
	// ARRANGE
	n := 1000000
	counter := batcher.NewAtomicCounter()
	counter.Add(int64(n))

	// ACT
	var wg sync.WaitGroup

	wg.Add(n)

	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()

			counter.Reset()

			if counter.Read() != 0 {
				panic("counter.Read() != 0")
			}
		}()
	}

	wg.Wait()

	// ASSERT
	require.Equal(t, int64(0), counter.Read())
}

func TestCanReadCounter(t *testing.T) {
	// ARRANGE
	n := 1000000
	counter := batcher.NewAtomicCounter()
	counter.Add(int64(n))

	// ACT
	var wg sync.WaitGroup

	wg.Add(n)

	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()

			if int(counter.Read()) != n {
				panic("counter.Read() != n")
			}
		}()
	}

	wg.Wait()

	// ASSERT
	require.Equal(t, int64(n), counter.Read())
}
