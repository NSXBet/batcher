package scenario

import (
	"math/rand"
	"time"
)

// Processor models downstream service time. A no-op processor measures Batcher
// itself; the others exist because Batcher's most important behaviour only
// appears when the downstream is slower than the arrival rate.
type Processor struct {
	// Name identifies the model in reports.
	Name string

	// ServiceTime returns how long a batch of the given size takes. It is called
	// on the processing goroutine.
	ServiceTime func(rng *rand.Rand, batchSize int) time.Duration

	// Err, when non-nil, is returned for every batch, exercising the error path.
	Err error
}

// NoOpProcessor returns immediately. Use it to isolate Batcher's own overhead.
func NoOpProcessor() Processor {
	return Processor{
		Name:        "noop",
		ServiceTime: func(*rand.Rand, int) time.Duration { return 0 },
	}
}

// FixedProcessor takes the same time for every batch, regardless of size. This
// models a downstream dominated by round-trip latency rather than payload size.
func FixedProcessor(d time.Duration) Processor {
	return Processor{
		Name:        "fixed=" + d.String(),
		ServiceTime: func(*rand.Rand, int) time.Duration { return d },
	}
}

// JitteredProcessor varies service time uniformly within +/- jitter, modelling
// ordinary downstream variance.
func JitteredProcessor(base, jitter time.Duration) Processor {
	return Processor{
		Name: "jittered=" + base.String(),
		ServiceTime: func(rng *rand.Rand, _ int) time.Duration {
			if jitter <= 0 {
				return base
			}

			delta := time.Duration(rng.Int63n(int64(2*jitter))) - jitter

			d := base + delta
			if d < 0 {
				return 0
			}

			return d
		},
	}
}

// SlowOutlierProcessor is fast most of the time and occasionally very slow.
// Tail behaviour, not the mean, is what breaks latency budgets, and a
// head-of-line blocking design only reveals itself under outliers.
func SlowOutlierProcessor(base, slow time.Duration, oneIn int) Processor {
	return Processor{
		Name: "outlier=" + base.String() + "/" + slow.String(),
		ServiceTime: func(rng *rand.Rand, _ int) time.Duration {
			if oneIn > 0 && rng.Intn(oneIn) == 0 {
				return slow
			}

			return base
		},
	}
}

// ErroringProcessor always fails, exercising the diagnostics path.
func ErroringProcessor(err error) Processor {
	p := NoOpProcessor()
	p.Name = "erroring"
	p.Err = err

	return p
}

// ServiceRate estimates how many items per second a processor can retire at the
// given batch size. It is used to express arrival rates as a fraction of
// capacity rather than as arbitrary numbers.
//
// A zero service time has no finite capacity, so callers must supply their own
// arrival rate in that case; this returns 0 to signal that.
func ServiceRate(p Processor, batchSize int) float64 {
	d := p.ServiceTime(rand.New(rand.NewSource(1)), batchSize)
	if d <= 0 {
		return 0
	}

	batchesPerSecond := float64(time.Second) / float64(d)

	return batchesPerSecond * float64(batchSize)
}
