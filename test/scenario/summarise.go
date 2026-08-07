package scenario

import (
	"runtime"
	"time"
)

// summarise converts raw per-item samples into the reported metrics.
//
// Samples offered during the warmup window are excluded from latency
// distributions but still counted in rates, since they were real load.
func summarise(
	cfg Config,
	slots []slot,
	batches []int,
	offeredFor time.Duration,
	runDuration time.Duration,
	memBefore, memAfter runtime.MemStats,
	goroutinesPeak int,
	heapPeak uint64,
	pendingPeak int64,
	heapSamples int64,
) Result {
	var (
		lateness   []time.Duration
		endToEnd   []time.Duration
		queueDelay []time.Duration
		admission  []time.Duration

		offered   int
		accepted  int
		completed int
		rejected  int
	)

	for i := range slots {
		s := Sample{
			ScheduledAt:    time.Duration(slots[i].scheduledAt.Load()),
			AdmissionStart: time.Duration(slots[i].admissionStart.Load()),
			AdmissionEnd:   time.Duration(slots[i].admissionEnd.Load()),
			ProcessorStart: time.Duration(slots[i].processorStart.Load()),
			ProcessorEnd:   time.Duration(slots[i].processorEnd.Load()),
			Accepted:       slots[i].accepted.Load(),
		}

		offered++

		if !s.Accepted {
			rejected++

			continue
		}

		accepted++

		// A zero ProcessorEnd means the item never reached the processor, which
		// is a shortfall worth surfacing rather than averaging away.
		if s.ProcessorEnd > 0 {
			completed++
		}

		if s.ScheduledAt < cfg.Warmup {
			continue
		}

		lateness = append(lateness, s.Lateness())
		admission = append(admission, s.AdmissionBlocking())

		if s.ProcessorEnd > 0 {
			endToEnd = append(endToEnd, s.EndToEnd())
			queueDelay = append(queueDelay, s.QueueDelay())
		}
	}

	latenessDist := NewDistribution(lateness)

	batchSizes := make([]int, 0, len(batches))
	partial := 0
	totalBatched := 0

	for _, size := range batches {
		batchSizes = append(batchSizes, size)
		totalBatched += size

		if size < cfg.BatchSize {
			partial++
		}
	}

	meanBatch := 0.0
	if len(batches) > 0 {
		meanBatch = float64(totalBatched) / float64(len(batches))
	}

	seconds := offeredFor.Seconds()
	if seconds <= 0 {
		seconds = runDuration.Seconds()
	}

	// Prefer the in-flight peak. Fall back to the post-drain reading only if the
	// sampler never fired, and report that via HeapSampled so a caller can tell a
	// real peak from a recovered-process number.
	heapSampled := heapSamples > 0 && heapPeak > 0

	heapHighWater := heapPeak
	if !heapSampled {
		heapHighWater = memAfter.HeapAlloc
	}

	allocsPerItem := 0.0
	if accepted > 0 {
		allocsPerItem = float64(memAfter.Mallocs-memBefore.Mallocs) / float64(accepted)
	}

	return Result{
		Config:            cfg,
		Lateness:          latenessDist,
		LatenessValid:     latenessDist.Count == 0 || latenessDist.P99 <= cfg.LatenessBudget,
		EndToEnd:          NewDistribution(endToEnd),
		QueueDelay:        NewDistribution(queueDelay),
		AdmissionBlocking: NewDistribution(admission),
		OfferedRate:       float64(offered) / seconds,
		AcceptedRate:      float64(accepted) / seconds,
		CompletedRate:     float64(completed) / runDuration.Seconds(),
		RejectedCount:     rejected,
		Batches:           len(batches),
		MeanBatchSize:     meanBatch,
		PartialBatches:    partial,
		BatchSizeDist:     NewIntDistribution(batchSizes),
		DownstreamPerSec:  float64(len(batches)) / runDuration.Seconds(),
		AllocsPerItem:     allocsPerItem,
		HeapHighWater:     heapHighWater,
		HeapSampled:       heapSampled,
		PendingWorkPeak:   pendingPeak,
		GCCount:           memAfter.NumGC - memBefore.NumGC,
		GCPauseTotal:      time.Duration(memAfter.PauseTotalNs - memBefore.PauseTotalNs),
		GCDuringRun:       memAfter.NumGC > memBefore.NumGC,
		GoroutinesPeak:    goroutinesPeak,
		Duration:          runDuration,
	}
}
