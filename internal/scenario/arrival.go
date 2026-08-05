package scenario

import (
	"math"
	"math/rand"
	"time"
)

// Arrival describes how offered load is shaped over time. Schedules are
// precomputed in full before a run starts, so the generator never allocates
// during measurement and the offered rate is independent of system health.
type Arrival struct {
	// Name identifies the shape in reports.
	Name string

	// Schedule returns offer times relative to run start, ascending.
	Schedule func(seed int64) []time.Duration
}

// Steady offers items at a fixed rate for the given duration.
func Steady(ratePerSecond int, duration time.Duration) Arrival {
	return Arrival{
		Name: "steady",
		Schedule: func(int64) []time.Duration {
			return fixedRate(ratePerSecond, duration)
		},
	}
}

// Sparse offers a single item every gap, which forces every batch to close on
// the timer rather than on size. This is the shape that exposes per-hop latency.
func Sparse(count int, gap time.Duration) Arrival {
	return Arrival{
		Name: "sparse",
		Schedule: func(int64) []time.Duration {
			out := make([]time.Duration, count)
			for i := range out {
				out[i] = time.Duration(i+1) * gap
			}

			return out
		},
	}
}

// Poisson offers items at an average rate with exponentially distributed gaps,
// which is a more realistic model of independent callers than a fixed rate.
// The seed is fixed by the caller so runs are reproducible.
func Poisson(ratePerSecond int, duration time.Duration) Arrival {
	return Arrival{
		Name: "poisson",
		Schedule: func(seed int64) []time.Duration {
			rng := rand.New(rand.NewSource(seed))
			mean := float64(time.Second) / float64(ratePerSecond)

			var (
				out []time.Duration
				at  float64
			)

			for {
				at += rng.ExpFloat64() * mean
				if time.Duration(at) > duration {
					break
				}

				out = append(out, time.Duration(at))
			}

			return out
		},
	}
}

// Burst alternates high-rate bursts with idle gaps. It measures absorption and
// recovery, which a steady rate cannot show.
func Burst(burstRate int, burstFor, idleFor time.Duration, cycles int) Arrival {
	return Arrival{
		Name: "burst",
		Schedule: func(int64) []time.Duration {
			var (
				out    []time.Duration
				offset time.Duration
			)

			for range cycles {
				for _, at := range fixedRate(burstRate, burstFor) {
					out = append(out, offset+at)
				}

				offset += burstFor + idleFor
			}

			return out
		},
	}
}

// AtCapacity offers load at a multiple of the processor's theoretical service
// rate, so overload is expressed as a ratio rather than a magic number.
//
// serviceRate is the number of items per second the configured processor and
// batch size can retire. A multiplier above 1.0 is deliberate overload.
func AtCapacity(serviceRate float64, multiplier float64, duration time.Duration) Arrival {
	rate := int(math.Max(1, serviceRate*multiplier))

	return Arrival{
		Name: "capacity",
		Schedule: func(int64) []time.Duration {
			return fixedRate(rate, duration)
		},
	}
}

func fixedRate(ratePerSecond int, duration time.Duration) []time.Duration {
	if ratePerSecond <= 0 || duration <= 0 {
		return nil
	}

	gap := time.Second / time.Duration(ratePerSecond)
	count := int(duration / gap)

	out := make([]time.Duration, count)
	for i := range out {
		out[i] = time.Duration(i+1) * gap
	}

	return out
}
