# Decision record: default batch interval

**Decision:** `DefaultBatchInterval` changes from **1s to 10ms** in v0.3.0.

**Status:** accepted, with the caveat in [Where 10ms is the wrong
choice](#where-10ms-is-the-wrong-choice).

This record exists because the plan required the default to follow measurement
rather than intuition. If the evidence had been ambiguous, the correct outcome was to
keep 1s and publish a configuration recommendation instead. It was not ambiguous.

## The problem being solved

The batch interval is a maximum *partial-batch age*, not a periodic flush tick: the
timer arms when the first item enters an empty batch. A request that crosses several
batching services therefore pays up to one interval **per hop**, and the cost is
worst for sparse traffic, which is exactly the traffic that gains least from
batching.

With a 1s default, a five-hop sparse path could accumulate several seconds before any
downstream work happened.

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
**predicted 100, measured 100** (100 batches, 99 calls/s). Asserted in
`TestDefaultWindowCoalescingHoldsAtRate`, because the tuning guidance is only
trustworthy if this relationship is real.

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
because it flushes before batches fill. That is one reason the default is 10ms rather
than the lowest measured value.

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

## Why 10ms and not 5ms or 1ms

| Candidate | Rejected because |
| --- | --- |
| **1ms** | Increases downstream calls under saturation (495/s versus 199/s) and coalesces only 2 items at 1k/s. It optimises latency by nearly removing batching. |
| **2ms** | At 1k/s, 3 items per batch and 373 calls/s. Too little coalescing for a default. |
| **5ms** | Defensible, and genuinely lower latency: at 1k/s its p99.9 measured *better* than 10ms (13.1ms versus 21.7ms), though both are within noise at that rate. The reason to prefer 10ms is coalescing, not latency — 6 items per batch versus 11 — which halves the batching benefit for a latency gain the measurement cannot even distinguish. |
| **20-100ms** | Better coalescing at low rates, but reintroduces the per-hop latency this work exists to remove, and provides no additional protection above 10ms under saturation. |

10ms is the point where latency is bounded at roughly the window while coalescing
remains meaningful at every rate tested (11x at 1k/s, 101x at 10k/s, 500x at 50k/s).

## Where 10ms is the wrong choice

**A service at ~1k items/s with an expensive downstream.** 10ms gives ~11x
coalescing and ~94 downstream calls/s. If a downstream call is costly enough that 94
calls/s is unacceptable, configure a larger interval deliberately:

```go
b := batcher.New(
    batcher.WithProcessor(fn),
    batcher.WithBatchInterval[Item](100 * time.Millisecond), // ~100x coalescing at 1k/s
)
```

The default cannot know your downstream cost. What it can avoid is silently adding
up to a second of latency per hop, which the old default did.

**A service that needs overload protection.** No interval provides it. Under
saturation every window from 10ms to 1s accepted ~200k/s while completing ~199k/s,
with the queue absorbing the difference. Protection requires `WithMaxQueueSize` and
`Enqueue`, or upstream flow control.

**A service with a slow processor and `Concurrency=1`.** The effective interval is
`max(BatchInterval, processor duration)`. With a 50ms processor, a 10ms window
behaves like a 50ms one — and measured p50 *worse* than a 100ms window (120ms versus
100ms), because queueing dominates. Raise `WithConcurrency` (with
`WithoutOrderedProcessing`) before lowering the interval.

## Migration

This is a **behaviour change for every caller who did not set `WithBatchInterval`**.

| Situation | Effect | Action |
| --- | --- | --- |
| Sets `WithBatchInterval` to a **positive** value | None | None |
| Passes `WithBatchInterval(0)` or a negative duration | Falls back to `DefaultBatchInterval`, so it moves 1s → 10ms with everyone else | Pass an explicit positive interval if you relied on the old 1s fallback |
| Relies on the 1s default, traffic ≥ 10k/s | None measurable — `BatchSize` was already the binding constraint | None |
| Relies on the 1s default, sparse traffic | Latency drops sharply; downstream call rate rises | Verify the new call rate is acceptable; set a larger interval if not |
| Depends on ~1000-item batches at low traffic | Batches become much smaller | Set `WithBatchInterval` explicitly, or raise `BatchSize` |

`Stats().BatchesFlushed` with the terminal counters gives mean batch size after a
drain, which is the fastest way to confirm what the change did to your own
coalescing:

```go
s := b.Stats()

// Include every terminal outcome: a failed or panicked batch was still flushed, so
// dividing by Completed alone undercounts whenever the processor errors.
if s.BatchesFlushed > 0 {
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
