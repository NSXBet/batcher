# Decision record: default batch interval

**Decision:** `DefaultBatchInterval` **stays at 1s**. 10ms is published as the
recommended value for latency-sensitive services, and callers opt in with
`WithBatchInterval`.

**Status:** accepted. Supersedes an earlier draft of this record that changed the
default to 10ms.

The plan required the default to follow measurement rather than intuition, and the
measurement below is not in dispute: 10ms is a large latency win at sparse rates and
still coalesces meaningfully. What the latency tables alone do not settle is who pays
for it. At 1k items/s the downstream call rate moves from ~10 calls/s to ~94 calls/s —
roughly 10x more downstream requests and the CPU that goes with them — for every
existing caller who never set an interval and never asked for lower latency.

A default is the setting imposed on people who have not thought about the trade-off.
Latency is visible to whoever is measuring it and can be fixed with one option; a
silent 10x increase in downstream load is not visible to that same caller until
something else saturates. So the evidence is published as tuning guidance, and the
default stays where existing deployments already are.

## The problem being solved

The batch interval is a maximum *partial-batch age*, not a periodic flush tick: the
timer arms when the first item enters an empty batch. A request that crosses several
batching services therefore pays up to one interval **per hop**, and the cost is
worst for sparse traffic, which is exactly the traffic that gains least from
batching.

With a 1s default, a five-hop sparse path could accumulate several seconds before any
downstream work happened. That is the cost this record quantifies, and the reason
10ms is *recommended*; it is not, on its own, a reason to move the default for
callers whose downstream cost this record cannot see.

## Environment

```text
go=go1.26.5 os=darwin arch=arm64 gomaxprocs=12 cpus=12
```

Absolute microseconds are host-specific. The **shape** of the relationship — and the
order of magnitude of every comparison below — is what the decision rests on. All
runs are open-loop with schedule-lateness validation; every row reported here passed
that validity check, so none of these numbers describe a run where the load generator
itself fell behind.

Reproduce with:

```sh
SCENARIO_MATRIX=1 go test -run TestDefaultWindow -timeout 30m -v ./test/scenario/
```

## Evidence 1: latency and coalescing versus window

`BatchSize=1000`, no-op processor, so this isolates batching cost from downstream
cost.

### 1,000 items/s

| Window | p50 | p99 | p99.9 | mean batch | calls/s | allocs/item |
| --- | --- | --- | --- | --- | --- | --- |
| 1ms | 1.21ms | 1.53ms | 2.25ms | 2 | 562 | 0.668 |
| 5ms | 3.21ms | 7.59ms | 13.1ms | 6 | 174 | 0.211 |
| **10ms** | **5.29ms** | **12.1ms** | **21.7ms** | **11** | **94** | **0.134** |
| 100ms | 51.2ms | 100ms | 100ms | 100 | 10 | 0.039 |
| 1s (old default) | 450ms | **981ms** | 997ms | 1000 | 1 | 0.033 |

### 10,000 items/s

| Window | p50 | p99 | p99.9 | mean batch | calls/s | allocs/item |
| --- | --- | --- | --- | --- | --- | --- |
| 1ms | 533µs | 1.05ms | 1.15ms | 11 | 929 | 0.097 |
| 5ms | 2.54ms | 5.03ms | 5.08ms | 51 | 197 | 0.023 |
| **10ms** | **5.03ms** | **10.0ms** | **10.1ms** | **101** | **99** | **0.013** |
| 100ms | 50.0ms | 99.0ms | 99.9ms | 1000 | 10 | 0.004 |
| 1s (old default) | 50.0ms | 99.0ms | 99.9ms | 1000 | 10 | 0.004 |

### 50,000 items/s

| Window | p50 | p99 | mean batch | calls/s |
| --- | --- | --- | --- | --- |
| 1ms | 516µs | 1.02ms | 51 | 981 |
| 5ms | 2.52ms | 4.99ms | 251 | 199 |
| **10ms** | **5.04ms** | **9.95ms** | **500** | **100** |
| 100ms | 10.0ms | 19.8ms | 1000 | 50 |
| 1s (old default) | 10.0ms | 19.8ms | 1000 | 50 |

Two things stand out.

**The 1s default is already ineffective above ~10k/s.** At 10k/s and 50k/s, 1s and
100ms produce *identical* results, because `BatchSize=1000` fills long before the
timer fires. The old default's only real effect was on sparse traffic — where it
did the most latency damage and bought the least.

**Latency tracks the window almost exactly.** p99 ≈ the window, plus scheduling
overhead. That is the expected consequence of a partial-batch-age timer and confirms
the sizing rule below.

## Evidence 2: the sizing rule holds

Expected batch size ≈ `arrival rate × window`. Measured at 10k/s with a 10ms window:
**predicted 100, measured ~101** (99 calls/s), matching the ~101 in the 10k/s table
above. Asserted in `TestDefaultWindowCoalescingHoldsAtRate`, because the tuning
guidance is only trustworthy if this relationship is real. The test asserts the
measurement lands within a factor of two of the prediction rather than pinning 101,
since the exact figure moves with host scheduling.

## Evidence 3: a smaller window does not worsen overload

Offered 200,000/s against a deliberately saturated 2ms processor:

| Window | accepted/s | completed/s | peak queued | peak heap | calls/s | mean batch |
| --- | --- | --- | --- | --- | --- | --- |
| 1ms | 199,999 | 197,972 | 1,018 | 12MB | 495 | 400 |
| 5ms | 199,997 | 197,130 | 1,415 | 12MB | 199 | 990 |
| **10ms** | **199,998** | **199,004** | **1,405** | **12MB** | **199** | **1000** |
| 100ms | 199,997 | 198,987 | 1,408 | 12MB | 199 | 1000 |
| 1s | 199,998 | 198,997 | 1,406 | 11MB | 199 | 1000 |

At 10ms and above the behaviour is indistinguishable: under saturation `BatchSize`
is reached before the timer matters, so queue depth, heap, and downstream call rate
are the same. This is the check that prevented the decision resting on latency alone
— a smaller default would have been wrong if it traded latency for stability.

Only 1ms differs, and it is *worse* on downstream load (495 calls/s versus 199)
because it flushes before batches fill. That is one reason the recommendation is 10ms
rather than the lowest measured value.

## Evidence 4: bursts

50,000/s for 100ms, idle 100ms, five cycles — the shape where a small window looks
worst on paper, since each burst coalesces from a standing start:

| Window | p50 | p99 | mean batch | calls/s |
| --- | --- | --- | --- | --- |
| 1ms | 518µs | 1.02ms | 51 | 545 |
| 5ms | 2.57ms | 5.02ms | 250 | 111 |
| **10ms** | **5.09ms** | **9.99ms** | **500** | **56** |
| 100ms | 10.0ms | 19.8ms | 1000 | 28 |
| 1s | 10.0ms | 19.8ms | 1000 | 28 |

10ms halves p99 against 100ms while only doubling downstream calls — not the 10x a
timer-only model would predict, because batches still fill during the burst.

## Why the recommendation is 10ms, and not 5ms or 1ms

| Candidate | Rejected because |
| --- | --- |
| **1ms** | Increases downstream calls under saturation (495/s versus 199/s) and coalesces only 2 items at 1k/s. It optimises latency by nearly removing batching. |
| **2ms** | At 1k/s, 3 items per batch and 373 calls/s. Too little coalescing for a default. |
| **5ms** | Defensible, and genuinely lower latency: at 1k/s its p99.9 measured *better* than 10ms (13.1ms versus 21.7ms), though both are within noise at that rate. The reason to prefer 10ms is coalescing, not latency — 6 items per batch versus 11 — which halves the batching benefit for a latency gain the measurement cannot even distinguish. |
| **20-100ms** | Better coalescing at low rates, but reintroduces the per-hop latency this work exists to remove, and provides no additional protection above 10ms under saturation. |

10ms is the point where latency is bounded at roughly the window while coalescing
remains meaningful at every rate tested (11x at 1k/s, 101x at 10k/s, 500x at 50k/s).
That makes it the right *recommendation*; the call-rate cost above is why it is not
the default.

## Where 10ms is the wrong choice (and the default is right)

**A service at ~1k items/s with an expensive downstream.** 10ms gives ~11x
coalescing and ~94 downstream calls/s. If a downstream call is costly enough that 94
calls/s is unacceptable, configure a larger interval deliberately:

```go
b := batcher.New(
    batcher.WithProcessor(fn),
    batcher.WithBatchInterval[Item](100 * time.Millisecond), // ~100x coalescing at 1k/s
)
```

The default cannot know your downstream cost, which is why it is left alone. Setting
10ms is what avoids the up-to-one-second per-hop latency; the unchanged 1s default
still carries it, and that is the trade a caller accepts by not configuring an
interval.

**A service that needs overload protection.** No interval provides it. Under
saturation every window from 10ms to 1s accepted ~200k/s while completing ~199k/s,
with the queue absorbing the difference. Protection requires `WithMaxQueueSize` and
`Enqueue`, or upstream flow control.

**A service with a slow processor and `Concurrency=1`.** The effective interval is
`max(BatchInterval, processor duration)`, so a window below the processor duration
buys nothing and can measure *worse*. With a 50ms processor at 10k items/s, a **5ms**
window measured p50 120ms against 100ms for a 100ms window, because queueing
dominates once the processor is the bottleneck.

`TestReproducesInlineSlowProcessorInversion` pins the ordering (5ms slower than
100ms), not those millisecond values; the figures are from the environment recorded
above (darwin/arm64, Apple M4 Pro, Go 1.26.5, `GOMAXPROCS=12`) and will differ
elsewhere. Raise `WithConcurrency` (with `WithoutOrderedProcessing`) before lowering
the interval.

## Adopting 10ms

There is no migration: the default is unchanged, so every existing caller keeps the
behaviour it has today. Adopting the recommendation is opt-in.

```go
b := batcher.New(
    batcher.WithProcessor(processor),
    batcher.WithBatchInterval[Item](10*time.Millisecond),
)
```

Before adopting it, check the effect on your own traffic. The interval only binds
when it closes a batch before `BatchSize` does — expected batch size is
approximately `min(arrival rate × interval, BatchSize)` — so the change is largest at
sparse rates and vanishes once `BatchSize` is reached first.

| Your arrival rate | 1s default | 10ms | What to check |
| --- | --- | --- | --- |
| ~1k/s | ~1000 items/batch, ~1 call/s | ~11 items/batch, ~94 calls/s | Downstream cost per call. This is the ~90x call-rate increase; it is the whole reason 10ms is not the default |
| ~10k/s | ~1000 items/batch, ~10 calls/s | ~101 items/batch, ~99 calls/s | Still ~10x more calls. `BatchSize` binds at 1s, the timer binds at 10ms |
| ~50k/s | ~1000 items/batch, ~50 calls/s | ~500 items/batch, ~100 calls/s | ~2x more calls. `BatchSize` caps both, so the gap narrows |
| Saturated | `BatchSize` binds | `BatchSize` binds | Nothing. Identical under saturation |

`Stats().BatchesFlushed` with the terminal counters gives mean batch size after a
drain, which is the fastest way to confirm what an interval change did to your own
coalescing:

```go
// Take the snapshot after the drain completes. BatchesFlushed increments when a batch
// is dispatched, while the terminal counters increment when the processor returns, so
// a snapshot taken under load divides finished items by dispatched batches and
// undercounts the mean.
if err := b.Shutdown(ctx); err != nil {
    return err
}

s := b.Stats()

// Include every terminal outcome: a failed or panicked batch was still flushed, so
// dividing by Completed alone undercounts whenever the processor errors.
if s.Pending == 0 && s.BatchesFlushed > 0 {
    meanBatch := float64(s.Completed+s.Failed+s.Panicked) / float64(s.BatchesFlushed)
    log.Printf("mean batch size: %.1f", meanBatch)
}
```

## What would reverse this decision

- Measurement on the CI reference runner (`ubuntu-latest`/amd64) showing a materially
  different latency/coalescing relationship. The stored baseline is currently
  darwin/arm64 only.
- A reported workload where 10ms coalescing is insufficient *and* the sizing rule
  fails to predict it, which would mean the guidance is wrong rather than the default.
- Evidence that the downstream call-rate increase is acceptable for the general case —
  for example a major version where callers must revisit configuration anyway. That is
  what would move 10ms from recommendation to default.
