# Batcher Performance, Reliability, and Benchmarking Plan

## Introduction

Batcher combines individual items into processor calls, flushing by batch size
or by elapsed batch window. It defaults to a 1-second window, while users
commonly configure 100ms. That coalesces well, but a request crossing several
batching services accumulates roughly one batch window per hop. A five-hop
sparse path can add about 500ms before any downstream work happens.

The goal is to make **5-10ms windows** practical wherever arrival rate justifies
batching. Measured on an idle process, the current timer path already flushes
sparse batches below 10ms. At 10,000 items/s a 10ms window still yields about
100 items per batch, so most coalescing survives while per-hop latency drops by
roughly 90ms.

The blocker is not the timer. It is that the processor runs **inline** in the
aggregation loop (`pkg/batcher/batcher.go:136`), so nothing is consumed while a
batch is being processed. The effective window becomes
`max(window, processor_duration)`. Measured with a 50ms processor at 10,000
items/s:

| Configured window | Effective flush interval | p50 latency |
| ----------------- | ------------------------ | ----------- |
| 5ms               | 50ms                     | 120ms       |
| 100ms             | 100ms                    | 50ms        |

Reducing the window made latency **2.4x worse**. Any guidance to lower the
window is invalid until processing is decoupled from intake, which requires real
concurrency.

Two further measurements shape this plan:

- **Bounded admission is a safety feature, not a performance feature.** The
  `feat/max-queue-size` implementation costs about 435ns per `Add` versus 335ns
  on `main` (~25-30% slower) and adds roughly 500µs at a 10ms window. It is
  worth landing for safety, but it must not be justified as a speed improvement.
- **A smaller window provides no overload protection.** With a 2ms processor and
  a saturating producer, 100ms, 10ms, and 1ms windows all accepted 1.8-2.4M items
  and grew the heap 2.2-4.3GB within two seconds. Overload requires bounded
  admission or upstream flow control.

  Those figures come from an unbounded exploratory probe that ran until it was
  stopped. The pinned regression test
  (`TestSmallWindowGivesNoOverloadProtection`) deliberately uses a much shorter
  window and far fewer items, so it reports tens of megabytes rather than
  gigabytes. Both measure the same property — accepted work accumulates without
  bound regardless of interval — at different scales; the smaller numbers in test
  output are not a contradiction.

Additionally, three concurrency hazards were confirmed by targeted experiment
and constrain the design rather than merely the tests:

1. A `select` containing both a closed "sealed" channel and a ready send admits
   work after sealing in about half of attempts. Sealing cannot be expressed as
   a `select` arm alongside the send.
2. Incrementing the pending counter *after* publishing an item lets a consumer
   decrement first, producing transient **negative** pending counts. This
   currently makes `Len`, `Join`, and shutdown completion unreliable.
3. Any "check the seal, then send" sequence has a send-on-closed-channel panic
   window. A blocked publisher panicked in **200 of 200** trials when the owner
   closed the channel underneath it.

Finding 3 has an architectural consequence adopted throughout this plan: **the
input queue is never closed.** Shutdown is communicated by a seal flag, a
publisher gate, and an explicit "no further input possible" signal — never by
closing the channel under publishers.

That rule is incompatible with `golang.design/x/chann`, and this forces a second
dependency decision. A `chann` unbounded queue owns a relay goroutine whose only
exit path closes its own ingress channel
(`chann@v0.1.2/chann.go:143`, `:167`) — the very channel publishers send to. So
with `chann` one must choose between leaking a goroutine per batcher forever and
re-arming the 200/200 send-on-closed panic. **Batcher therefore owns its queue**:

- **bounded mode:** a plain buffered channel of capacity `N`. Exact bound, no
  goroutine, never closed.
- **unbounded mode:** a mutex-guarded slice plus a capacity-1 signal channel. No
  goroutine, never closed.

This also removes the relay-invisibility problem: a successful publish is
immediately visible to the aggregator, so no accepted item can be hidden inside a
relay during the final drain. Measured consequence: **1 goroutine per batcher**
(from 6 today, and better than the 3 that removing `rill` alone would give), with
zero leaked after `closed`.

A crucial corollary makes this design robust: because the queue is never closed,
a publisher that races past the seal check is **benign**. Its item still lands in
the queue and is still drained. Select bias between a ready seal signal and a
ready send therefore cannot lose, corrupt, or double-count anything. Sealing is
an admission *policy* boundary; memory safety and the drain guarantee do not
depend on winning that race.

## Delivery model

Each **phase** is delivered as one pull request. Its milestones are ordered,
independently reviewable **commits** within that pull request; they are not
separate pull requests. A phase PR may be merged only after every mandatory
milestone commit in that phase meets its acceptance criteria. Conditional
milestones, such as 4.2, remain separate commits and may be omitted entirely
when their evidence gate is not met.

This keeps the phase-level architecture reviewable as one coherent change while
retaining bisectable, incremental commits and explicit acceptance points.

## Design commitments

1. **Accepted work is never silently discarded.** `Shutdown(ctx)` seals
   admission and drains accepted work. A caller timeout reports what remains and
   does **not** cancel the drain; a later `Shutdown` with a fresh context waits
   on the same in-progress drain.
2. **`Close()` is a convenience, never a data-loss operation.** Flat,
   configurable 30-second grace; returns as soon as the drain completes; reports
   incompleteness while the drain continues.
3. **Admission is memory-safe by construction.** The input queue is owned by
   Batcher, holds no goroutine, and is never closed. Publishers reserve before
   publishing. No publisher can panic; pending can never go negative.
4. **The enqueue path stays cheap and is measured, not asserted.** See the
   measured cost model in the admission protocol below. No CAS retry loop and no
   allocation in steady state. Bounded mode takes no lock; unbounded mode takes
   one uncontended slice mutex, which the prototype measures as cheaper than
   today's channel-relay hop.
5. **Ordering is explicit.** `n = 1` guarantees no concurrent processor
   invocation and FIFO by *publication order*. `n > 1` requires both
   `WithConcurrency(n)` and `WithoutOrderedProcessing()`, or construction panics.
6. **No hot-path observability hooks.** `Stats()` is an allocation-free O(1)
   snapshot of typed `sync/atomic` counters; eventually consistent and
   explicitly non-transactional.
7. **No pooled batch slices.** Processors may retain their batch slice after
   returning, exactly as today. No allocation optimisation may impose an
   ownership rule on user code.
8. **Benchmark before tuning.** Timing and allocation changes require scenario
   evidence with predeclared numeric thresholds. Shared CI gates correctness and
   allocation counts, never latency percentiles.

## The admission and drain protocol

This section is normative. It is the synchronization design, not an aspiration.
It has been validated by three standalone prototypes under `-race`:

- the direct-channel protocol: 200 trials each for unbounded `Add`, bounded
  `Add`, bounded `Enqueue` with cancellation, and producers-finish-first, plus
  300 trials of the blocked-publisher shutdown case;
- the relay topology: 400 simultaneous-publisher/seal races and 300
  last-leaver-before-seal races, which is how the `chann` goroutine-exit
  contradiction was found; and
- the owned-queue protocol **including the worker pool**: 300 trials of inline
  `n=0` with shutdown racing 8 producers, 300 of an `n=4` worker pool under the
  same race, and 300 of the specific case where a full batch is in flight on a
  busy worker with an empty queue when shutdown fires.

All passed with no panic, no negative counters, no lost wakeup, no deadlock, and
exact `Accepted == Completed` accounting after drain, with 1 goroutine per
batcher and zero leaked after `closed`.

What is **not** yet proven: the prototypes model the protocol, not the finished
library. They do not exercise the batch timer, bounded-mode blocking `Add`
combined with worker backpressure, panic recovery interacting with the drain, or
the diagnostics channel. Those paths are specified and test-mandated below, and
must be validated during implementation rather than assumed.

### Publisher gate

```text
enter():  if sealed -> reject
          gate++
          if sealed -> leave(); reject      // ALL decrements route through leave()
          proceed
leave():  if --gate == 0 && sealed -> signal gateEmpty (once)
```

The gate counts publishers that are between `enter()` and `leave()`. The
re-check after incrementing is what makes sealing observable without a mutex.

**Every gate decrement must route through `leave()`**, including the rejection
path in `enter()`. A bare `gate--` there reintroduces the lost wakeup: an entrant
could increment the gate, the coordinator's step-3 check could observe a non-zero
gate and proceed to wait, and the entrant's re-check could then decrement to zero
and return without signalling — leaving `Shutdown` blocked forever. This is a
distinct interleaving from the last-leaver-before-seal case and has its own
mandatory test.

### Reservation, acceptance, and accounting

A pre-publication reservation is needed to ensure the drain cannot finish before
a publisher that has entered the gate either publishes or aborts. The library
therefore uses **two distinct work counters**, both typed atomics:

- `Pending` counts every drain obligation: a publisher that has entered the
  publication window plus every successfully published item until its terminal
  processor outcome. It is incremented before publication; an abort or a terminal
  outcome decrements it exactly once.
- `IntakePending` counts obligations that the **aggregator has not yet received**.
  It too is incremented before publication, decremented when the aggregator
  receives the item, and rolled back on abort. It is the only counter used to
  decide how many post-`noInput` receives remain.
- `Accepted` is incremented on successful publication. Cancellation or seal
  rollback never increments it, consumes no FIFO position, and is therefore
  never accepted work.

There is intentionally no `Reserved` statistic. The gate is the authoritative
proof that no publisher remains in the reservation/publication window once
`gateEmpty` is observed. Exposing a partial `Reserved` count would still not make
an atomic multi-field `Stats()` snapshot transactional.

The equations have explicit scopes:

```text
// Exact after publisher quiescence (gateEmpty), before terminal completion:
Accepted == Completed + Failed + Panicked + Pending

// Intake-only drain obligation. It excludes worker in-flight work:
IntakePending == 0  and noInput closed  =>  intake is exhausted forever
```

`Pending` is deliberately conservative during `sealing`: it may include a
publisher that will shortly abort on `sealCh`. This protects correctness. A
`ShutdownIncompleteError` must therefore report both `Pending` and the current
`PublishersInGate`, and document that `Pending` is a **conservative drain
obligation**, not an exact count of already accepted items until `gateEmpty`.

No accepted item is ever discarded by cancellation: cancellation happens before
`Accepted` increments. `Pending` and `IntakePending` are incremented before
publication specifically so the drain cannot terminate while a publisher is in
the publication window.

### Publication and hot-path atomic budget

```text
enter():                       // gate: 1 seal load, gate++, 1 seal re-check load
Add / Enqueue entry:
  Pending++ ; IntakePending++  // reserve before publication

successfully publish:
  Accepted++                   // semantic acceptance

abort (seal, context cancellation):
  Pending-- ; IntakePending-- ; Rejected++
  // MUST complete inside the gate window, before leave()

leave():                       // gate--, 1 seal load

aggregator receives:
  IntakePending--

terminal processor outcome:
  Pending-- ; increment exactly one terminal category
```

The honest successful-path budget is therefore **two seal loads plus one in
`leave()`, five typed atomic RMWs (`gate++`, `Pending++`, `IntakePending++`,
`Accepted++`, `gate--`), and one publish**. Earlier drafts claimed one RMW and
then three; both undercounted by ignoring the mandatory gate. The count is stated
here in full precisely because Phase 4's own measurement shows contended atomics
are the dominant enqueue cost, so an understated budget would invalidate the
design rationale.

There is no CAS retry loop and no allocation. The owned-queue prototype measures
**136-168 ns/op with zero allocations** at 1, 4, and 12 producers — faster than
today's 335 ns/op `Add`, because removing the `chann` relay removes a channel
hop and a goroutine handoff. Phase 1 owns the authoritative evidence and must
confirm this before the change is accepted; the figure is a prototype baseline,
not a portable promise.

**Rollback ordering is a correctness requirement, not a detail.** An abort must
roll `Pending` and `IntakePending` back *before* `leave()`. If a rejected
publisher left the gate first, the coordinator could observe `gateEmpty` while a
phantom intake obligation remained, and the final drain would wait for an item
that will never arrive.

`Add` remains the compatibility fast path: normally unbounded mode completes as
a direct send; in bounded mode it may block when full, as it does today. It does
not expose an error. `Enqueue` is the API for callers who require a cancellation
deadline. Both select on `sealCh`, which releases an already-blocked publisher
when shutdown starts.

### Drain and aggregator termination

The aggregator cannot learn "input is exhausted" from a never-closed channel, so
the coordinator supplies that knowledge explicitly via a `noInput` channel. The
aggregator selects on input and on `noInput`; there is **no polling and no
busy-wait** in steady state.

```text
Shutdown():
  1. sealed = true
  2. close(sealCh)                  // release publishers blocked on a full queue
  3. if gate == 0 { signal gateEmpty }   // coordinator self-check, see below
  4. wait for gateEmpty             // aggregator KEEPS DRAINING during this wait
  5. close(noInput)                 // no publisher can ever send again
  6. aggregator: drain intake by accounting; flush partial batch exactly once
  7. terminate worker dispatch; wait for workers (Pending -> 0)
  8. close diagnostics last
```

`gateEmpty` must be a **durable, non-blocking latch** — a `sync.Once` closing a
channel, or a capacity-one token. An unbuffered send at step 3 would deadlock
before step 4.

**Step 3 is mandatory, not an optimisation.** `leave()` signals `gateEmpty` only
when it observes `sealed`. If the last publisher leaves *before* `sealed` is
stored, nobody would ever signal and `Shutdown` would block forever. The
coordinator must therefore perform its own post-seal `gate == 0` check. This is
the lost-wakeup hazard; a prototype without step 3 deadlocks, and with it 300 of
300 last-leaver-before-seal trials complete.

**The gate barrier.** `enter()` increments the gate and then *re-checks* the
seal, aborting if set. Combined with step 3, this is sufficient under
sequentially consistent `sync/atomic` accesses. A successful `enter()` performed
its second unsealed read before the coordinator's seal store in the total order,
so its gate increment is visible to any later coordinator read; an entrant that
increments *after* the coordinator observed zero must observe `sealed` in its
re-check and abort without publishing. Validated across 400 trials of 16
publishers released simultaneously with a concurrent `Shutdown`, under `-race`,
with zero lost items and zero deadlocks. That run used the relay topology; the
owned-queue prototype re-validates the same gate at `n=0` and `n=4`.

**Intake termination is by intake accounting.** After `noInput` closes, the
aggregator drains on `IntakePending` rather than on an empty-queue observation:

```text
<-noInput
for IntakePending > 0 { drainReady() }   // owned queue: a publish is immediately visible
flush()                                  // final partial batch, exactly once
close(dispatch); workers.Wait()          // n>1 only
```

Draining on the counter rather than on "the queue looks empty" is what makes this
correct for any queue implementation. It was originally required because a
`chann` relay could hide an accepted item mid-transfer; with an owned queue a
successful publish is immediately visible, so the counter and the queue agree, and
the rule now also guards against a publisher that has reserved but not yet
pushed.

`IntakePending`, **not** `Pending`, is the loop condition. `Pending` includes
worker in-flight items, so using it would deadlock at `n > 1`: with a full batch
already dispatched to a busy worker and no items left in the queue, `Pending > 0`
while nothing further can ever arrive, and the aggregator would block forever on
a receive. `IntakePending` counts only obligations the aggregator has not yet
received, so it reaches zero exactly when intake is exhausted. Because the gate
guarantees no publisher remains, every awaited receive is guaranteed to arrive and
the loop cannot hang or over-receive.

Termination is thus driven by the gate, `noInput`, intake accounting, and finally
worker quiescence — never by observing an empty channel.

### Lifecycle states

Exactly one of: `new` (constructed, not started), `running`, `sealing`
(admission closed, publishers still leaving the gate), `draining` (gate empty,
accepted work still processing), `closed` (terminal, all owned goroutines
exited).

- `IsClosing()` is true in `sealing`, `draining`, `closed`.
- `IsClosed()` is true only in `closed`.
- A processor that never returns holds the batcher in `draining` forever. This
  library cannot cancel user code; that is documented, not worked around.

### Terminal outcomes

Terminal counters are **mutually exclusive**: a batch whose processor returns an
error increments `Failed` only; a batch whose processor panics increments
`Panicked` only. `Failed` means "processor returned a non-nil error", never
"panicked". `Pending` is decremented exactly once per item, on abort or terminal
outcome. The scope rules for the accounting equations are given in
"Reservation, acceptance, and accounting" above: the `Accepted` equation is exact
only at publisher quiescence, and the `Pending` decomposition is a diagnostic
conservation check, never an instantaneous API guarantee.

### Capacity contract

`WithMaxQueueSize(N)` bounds queued items. Total accepted-but-not-terminal work
is bounded by:

```text
N + (1 + concurrency) × BatchSize + P
```

where `P` is the number of publishers currently inside the gate, exposed as
`Stats().PublishersInGate`. The `(1 + concurrency)` term is deliberate: one batch
is held by the aggregator after leaving the queue, and `concurrency` batches can
be inside processors. Writing it as `BatchSize + concurrency × BatchSize`
understates the peak by one batch, which at `n = 1` is a factor-of-two error on
that term. At `n = 1` there is no dispatch stage, so the
`concurrency × BatchSize` term collapses to the single inline batch already
counted by `BatchSize`; the formula is therefore conservative rather than wrong
at `n = 1`. `P` is bounded by the caller's own concurrency, not
by Batcher, because a blocking `Add` on a full queue is by definition a parked
caller goroutine. This must be documented plainly: **Batcher bounds its own
buffers; it cannot bound how many goroutines a caller parks in `Add`.** Callers
who need a hard cap on that term must use `Enqueue` with a context deadline,
which converts parked publishers into bounded, rejected admissions.

## Phase 1: Establish a truthful measurement baseline

### Reason

No default window, allocation change, or regression claim is defensible without
end-to-end measurement. Current benchmarks time only serial `Add`, use a
one-second interval, include `fmt.Sprintf` inside the timed loop, stop timing
before `Join`, and mislabel two batch sizes. Measured spread across all five
existing benchmarks (240-446 ns/op) exceeds the difference between the
configurations they claim to compare.

Phase 1 changes no library behaviour, so it can land while Phase 2 design
review proceeds.

### Milestone 1.1 — Fix and scope the enqueue microbenchmarks

**Commit scope**

- Fix `BenchmarkBatcherBatchSize10_000` and `BenchmarkBatcherBatchSize100_000`,
  which both call `runBench(b, 100)` (`pkg/batcher/batcher_bench_test.go:24-30`).
- Hoist payload construction out of the timed loop
  (`batcher_bench_test.go:48`); the `fmt.Sprintf` alone measured ~51 ns/op.
- Add `ReportAllocs`, sequential and `RunParallel` variants at `-cpu=1,2,GOMAXPROCS`.
- Document the exact invocation, including `-count`, `-benchtime`, `-benchmem`.
- Label this suite explicitly as enqueue overhead, not throughput.

**Acceptance criteria**

- Each benchmark's configured batch size equals the size in its name.
- No formatting or payload allocation inside any timed region.
- Allocations reported for every case.
- `benchstat` over `-count=10` distinguishes the previously mislabeled 100k case
  from the 100 case. Note the separation appears in **bytes/op**, not ns/op:
  measured 238.5 B/op ± 5% versus 176.0 B/op ± 7%, while sec/op overlaps within
  noise. That is expected — `Add` does the same work per item regardless of batch
  size, and only the per-flush batch slice capacity differs — so ns/op must not
  be used as the discriminator here.
- No non-test file is modified.

**Dependencies**: none.

### Milestone 1.2 — Open-loop scenario harness

**Commit scope**

Build a deterministic, in-repo harness in `test/scenario` (test infrastructure,
not `internal/`, which is reserved for library code) with mock processors and no
network dependency. It must be **open-loop**: arrivals follow a precomputed schedule and
are never gated on completion or on capacity, so overload cannot suppress
arrivals (coordinated omission).

Required per-item timestamps: `scheduled_at`, `admission_start`,
`admission_end`, `processor_start`, `processor_end`.

Required reported metrics:

- schedule lateness (`admission_start - scheduled_at`) as a distribution;
- admission blocking time and rejection counts, reported separately from
  processing latency;
- end-to-end latency p50/p95/p99/p99.9/max from raw samples, never from `ns/op`;
- offered, accepted, rejected, and completed rates;
- batch-size histogram, partial-batch fraction, downstream calls/s;
- allocations/item, heap high-water, GC count and pause totals, goroutine count;
- post-burst recovery time to a stated queue-depth threshold.

Harness hygiene requirements:

- sample storage preallocated before measurement; no growth during a run;
- recording via preallocated per-producer buffers, merged after the run, so the
  recorder neither allocates nor locks on the hot path;
- fixed PRNG seeds for jitter; documented warmup and measurement windows;
- memory/GC runs separated from latency runs in distinct processes;
- `GOMAXPROCS`, Go version, OS/arch, and CPU model recorded per report;
- sub-millisecond windows flagged as environment-sensitive and excluded from
  portable pass/fail claims.

Scenario matrix: windows `500µs, 1, 2, 5, 10, 20, 50, 100ms` × arrivals
`sparse, steady 40/70/90/100%, overload 150/200/500%, bursty` × producers
`1, 4, GOMAXPROCS, 4×GOMAXPROCS` × processors
`no-op, fixed 0.5/2/50ms, jittered, occasional-slow-outlier, error-returning`.

**Acceptance criteria**

- Arrival generation is open-loop and lateness is reported; a scenario whose
  lateness p99 exceeds a declared bound is marked invalid rather than reported
  as a latency result.
- Completion is observed via explicit processor signals, never `Join` polling.
- A self-check scenario confirms harness overhead: recorder allocations per item
  are zero during measurement.
- Reproduces the two known baseline results (inline slow-processor inversion;
  overload heap growth at every window).

**Dependencies**: Milestone 1.1.

### Milestone 1.3 — CI lanes and predeclared thresholds

**Commit scope**

- Blocking PR lane: `go test -race ./...` plus allocation-count assertions via
  `testing.AllocsPerRun` for `Add`, `Enqueue`, and the recovery wrapper.
- Scheduled/manual lane: full scenario matrix, artifacts published, trend
  comparison via `benchstat`. Collection and comparison are separate steps —
  `-count` is a `go test` flag, not a `benchstat` one:

  ```sh
  go test -run='^$' -bench=. -benchmem -count=10 ./... > new.txt
  benchstat old.txt new.txt
  ```
- Record predeclared numeric thresholds in a checked-in file, keyed by Go version
  and architecture. Initial values, to be re-baselined on the chosen reference
  runner in this milestone:

  | Gate                                             | Threshold                          |
  | ------------------------------------------------ | ---------------------------------- |
  | `Add` allocations, unbounded path                | 0 in steady state (see note)       |
  | `Stats()` allocations                            | exactly 0                          |
  | Recovery wrapper allocations, non-panic          | exactly 0                          |
  | `Add` ns/op regression (enqueue microbench)      | ≤ +10% vs stored baseline          |
  | Sparse-window allocated bytes/flush, 4.2 trigger | > 2 KB/flush to justify work       |
  | 4.2 allocation regression ceiling, any scenario  | ≤ +2% allocated bytes              |
  | Goroutines per idle batcher after 2.2            | exactly 0                          |
  | Goroutines per running n=1 batcher after 2.2     | exactly 2 (aggregator + processor) |
  | Goroutines after `closed`                        | equal to pre-construction baseline |

  Latency percentiles are deliberately absent: they are reported, never gated.

  Note on the allocation gate: the unbounded queue is slice-backed, so `append`
  must allocate when it grows its backing array. The gate is therefore "zero
  allocations per `Add` in steady state", measured with `AllocsPerRun` after a
  stated warmup and with the queue retaining capacity across drains. Growth-path
  allocations are exempt and must be named in the test.
- Demote the current post-merge 200% threshold
  (`.github/workflows/bench.yml` `alert-threshold` / `fail-on-alert` settings) to informational; it cannot catch
  meaningful regressions and runs on mutable `ubuntu-latest` with Go `stable`.

**Acceptance criteria**

- The PR lane contains no latency-percentile pass/fail gate.
- Allocation thresholds are exact integers, not prose.
- Every scheduled report embeds full environment metadata.
- A deliberately introduced extra allocation in `Add` fails the PR lane.

**Dependencies**: Milestone 1.2.

## Phase 2: Make admission and shutdown safe

### Reason

Performance work is not credible while `Add` can panic during shutdown
(reproduced), `IsClosed` races (reproduced under `-race` at
`batcher.go:177/184`), and shutdown can discard accepted work (reproduced: 50
accepted, 0 processed, `Len()` reporting 50 phantom items). Each milestone here
is an independently reviewable commit that leaves the library semantically
consistent, so the phase PR can be bisected safely.

### Milestone 2.1 — Replace `rill` with an owned aggregation loop

**Commit scope**

This is separated from admission work deliberately: it is a pipeline rewrite,
and bundling it would make regressions unattributable. It ports the equivalent
commits from `feat/max-queue-size` (`f896def`, `b452bb0`, `05dca9b`) as one
reviewable behaviour-preserving change.

- Land characterization tests against current `rill` behaviour **first**, in the
  same PR, and keep them passing after the swap.
- Replace `rill.Batch` (`pkg/batcher/batcher.go:62`) with an in-repo aggregation
  loop preserving today's semantics: timer armed on first item of an empty
  batch; flush on size, on timer, and at shutdown; no empty batches.
- Remove the `github.com/destel/rill` dependency.
- **Retain today's close-to-terminate shutdown model in this commit.** The
  never-closed-input design arrives in 2.2. Adopting it partially here would
  leave an intermediate release with neither channel closure nor a viable
  drain-exit signal.

**Acceptance criteria**

- Characterization tests written pre-swap pass unchanged post-swap: batch
  boundaries, timer-armed-on-first-item behaviour, ordering, error propagation,
  no-empty-batch guarantee.
- Goroutines per *constructed, auto-started* batcher drop from 6 to **5**,
  verified by test and by direct measurement (50 batchers: +250 goroutines, 0
  leaked after `Close`).

  The plan first predicted 3, then 4. Both were wrong, and the reasons are worth
  recording because they constrain the design:

  1. **Merging aggregation into the processing loop changes latency.** Measured
     with a 50ms processor at 10k items/s, a merged loop inverted the baseline
     finding (5ms window p50 28ms versus 100ms window p50 75ms), because the next
     batch only began accumulating after the processor returned. Aggregation and
     processing must stay separate goroutines here; decoupling them differently
     is a Phase 3 decision, not one this milestone may smuggle in.
  2. **Input draining must be isolated from batch bookkeeping.** With a single
     goroutine doing both, sequential `Add` regressed 39-50% against the stored
     baseline, breaching the ≤+10% gate: the `chann` relay has a small bounded
     ingress, and its output was not being read promptly while the aggregator was
     busy with timer and slice work. Adding a dedicated forwarding stage restored
     parity (no regression; one case 6.7% faster). This is what `rill`'s
     `ToChans`/`Batch`/`FromChans` pipeline was buying.

  The five are: `chann` input relay, input forwarder, aggregator, processor loop,
  and `chann` error relay. Phase 2.2 removes both `chann` relays and the
  forwarder along with them, because an owned queue needs no relay and can be
  read directly. The single-goroutine target in the threshold table applies only
  once Phase 3 owns the worker model.
- Enqueue benchmarks from 1.1 show no regression beyond predeclared thresholds.
- `go.mod`/`go.sum` no longer reference `rill`; no other dependency added.
- No public API change in this commit.
- Shutdown semantics are byte-for-byte equivalent to pre-PR behaviour, including
  its existing defects; this commit fixes no lifecycle bug.

**Dependencies**: Milestones 1.1 and 1.3 (regression attribution needs the
threshold file).

### Milestone 2.2 — Never-closed input and the admission gate

**Commit scope**

This milestone is the **complete internal lifecycle migration** required by a
never-closed queue. It is self-contained because it includes the
internal coordinator that owns `sealed`, `sealCh`, `gateEmpty`, `noInput`, final
intake drain, and legacy `Close()` termination. It also replaces `chann`, whose
relay cannot exit without closing publisher ingress. It does **not** yet expose
the new resumable `Shutdown(ctx)` API; externally, `Close()` retains its existing
grace-period result shape while using the new safe internal coordinator.

Implement the publisher gate, two-counter accounting, and owned queues exactly
as specified in "The admission and drain protocol":

- Replace both `chann` instances and remove the dependency from `go.mod`:
  - **input queue:** plain buffered channel of capacity `N` in bounded mode; a
    mutex-guarded slice plus a capacity-1 signal channel in unbounded mode. Never
    closed; owns no goroutine.
  - **diagnostics queue:** a plain buffered channel (capacity 1,024 by default),
    closed once by the drain coordinator after all senders have exited. Owns no
    goroutine. Milestone 2.4 adds the drop-newest policy and counter on top of
    this channel; it does not re-replace the queue.
- Publisher gate with the post-increment seal re-check, plus the coordinator's
  mandatory post-seal `gate == 0` self-signal to prevent a lost wakeup.
- Reserve `Pending + IntakePending` before publish; successful publication
  increments `Accepted`; abort rolls both obligations back **before `leave()`**.
- Add `sealCh`, closed at shutdown, to release publishers blocked on a full
  bounded queue.
- Add `Enqueue(ctx, item) error` returning `ErrClosing`, `ctx.Err()`, or `nil`;
  drop the branch's redundant pre-flight selects.
- Add `WithMaxQueueSize(N)`; default remains unbounded.
- Keep `Add` as the compatibility fast path: it may block on a full bounded
  queue, as today's `Add` does, and `sealCh` releases it at shutdown. Callers
  that need cancellation use `Enqueue`.
- **`Close()` on timeout must already report-and-keep-draining** in this
  milestone. It may keep its existing signature and grace behaviour, but it must
  not retain the legacy path that signalled `doneChan` and abandoned the drain
  (`pkg/batcher/batcher.go:171`), because that is the data-loss defect. The
  resumable public API and typed error arrive in 2.3; the non-destructive
  behaviour arrives here.
- **A minimal `Stats()` lands here**, exposing every field this milestone's own
  acceptance criteria assert: `Pending`, `IntakePending`, `PublishersInGate`,
  `Queued`, `Accepted`, `Completed`, `Failed`, `Panicked`, and `Rejected`.
  Later milestones each add the field they need as they add the mechanism: 2.4
  adds `DroppedErrors`, 3.2 adds `InFlight`. Milestone 4.1 completes the snapshot
  (`BatchHeld`, `BatchesFlushed`), documents the whole contract, and asserts the
  accounting invariants — it does not introduce `Stats()`.

Because this milestone introduces the never-closed input, it must also land the
`noInput`-based aggregator termination from the protocol section; 2.1's
close-to-terminate model is replaced here, atomically, in one PR.

**Acceptance criteria**

- A test that parks a publisher mid-send and seals concurrently produces zero
  panics across ≥10,000 iterations. (Worker concurrency does not exist until
  Phase 3; 3.2 re-runs this and every other admission/drain test at `n>1`.)
- A stress test asserts `Pending` is **never observed negative** and converges
  to zero, with `Accepted == Completed + Failed + Panicked` at quiescence, across
  ≥400 trials of: unbounded `Add`; bounded `Add`; bounded `Enqueue` with
  cancellation; and shutdown racing simultaneously-released publishers.
- **Lost-wakeup test A (last leaver before seal):** the last publisher leaves the
  gate *before* sealing begins; `Shutdown` still completes, across ≥300
  iterations. Removing the coordinator's post-seal `gate == 0` self-check must
  make this test hang, proving the check is load-bearing.
- **Lost-wakeup test B (entrant re-check rejection):** using barriers, force the
  interleaving where an entrant does `gate++`, the coordinator seals and observes
  a non-zero gate and commits to waiting, and only then does the entrant's
  re-check reject and decrement to zero. `Shutdown` must complete across ≥500
  iterations. Replacing `enter()`'s `leave()` call with a bare `gate--` must make
  this test hang; prototype confirms the bare decrement deadlocks while routing
  through `leave()` completes 500/500.
- **Entrant-race test:** N publishers released simultaneously with a concurrent
  `Shutdown`; every publisher either observes rejection or has its item
  processed. `Accepted == Completed` exactly; no item is lost.
- **Final-intake completeness test:** shutdown immediately after a successful
  publish, repeated, asserts the item is always processed. A deliberately broken
  implementation that terminates from an empty-queue observation instead of
  `IntakePending` must fail this test, proving accounting-based draining is
  required.
- `PublishersInGate` and `IntakePending` are exposed and asserted: a test parks
  publishers in the publication window and asserts `PublishersInGate > 0`, that
  it reaches zero exactly when `gateEmpty` is observed, and that
  `IntakePending == 0` once the final intake drain completes.
- A canceled or seal-rejected reservation increments `Rejected`, leaves
  `Accepted` unchanged, rolls `Pending` back, and consumes no FIFO position.
- `Enqueue` blocked on a full bounded queue returns `ErrClosing` promptly when
  `sealCh` closes, with its reservation rolled back.
- `Start` called twice, concurrently, or after sealing yields exactly one
  processing lifecycle and no double-close.
- No code path closes the input channel; enforced by a review checklist item and
  by a test that seals while a publisher is parked in the send.
- Steady-state operation performs no polling: a test asserts the aggregator
  blocks in `select` rather than spinning when idle.
- `AllocsPerRun` shows zero allocations for `Add` in the unbounded path **in
  steady state**, measured after the warmup and with queue capacity retained
  across drains as defined in 1.3; slice growth is the one named exemption. The
  enqueue benchmark stays within the predeclared threshold of the recorded
  baseline. Removing the `chann` relay is expected to make `Add` *faster* than
  today's 335 ns/op (prototype: 136-168 ns/op); a regression here indicates the
  owned queue is mis-implemented.
- **Goroutine assertions:** 0 per unstarted batcher, 2 per running `n=1` batcher
  (aggregator + serial processor), `1 + n` per running `n>1` batcher (aggregator
  + workers), and exact return to the pre-construction baseline after `closed`.
  Measured: n=1=2, n=2=3, n=4=5, n=8=9, all with zero leaked. No queue owns a
  goroutine.

**Dependencies**: Milestones 2.1 and 1.3.

### Milestone 2.3 — Drain protocol and resumable shutdown

**Commit scope**

- Add `Shutdown(ctx) error` with a single drain coordinator and a write-once
  terminal result. Additional and later callers wait on the same drain; none
  restarts it. The internal coordinator itself already exists from 2.2; this
  milestone adds the public resumable API, per-caller waiting, and the terminal
  result, and must not re-implement the seal/gate/`noInput` sequence.
- The aggregator **keeps draining while the coordinator waits for the gate to
  empty**; the gate wait must never block consumption, or a publisher parked on a
  full bounded queue can never leave the gate.
- **`Shutdown` on an unstarted batcher with pending items starts the drain
  lifecycle** to satisfy commitment 1. This is the one permitted exception to
  "no start after sealing", and it is stated as such. For an unstarted bounded
  batcher, the drain consumer must start before the gate wait, for the same
  deadlock reason.
- Return `*ShutdownIncompleteError{Pending int, PublishersInGate int, Cause error}`
  on caller timeout, while the drain continues. Its godoc must state that
  `Pending` is a **conservative drain obligation**: it can include publishers
  still inside the gate which may yet abort on `sealCh`, and it counts worker
  in-flight batches, so it is not a queue depth. It is an exact count of
  accepted-but-unfinished work only once `PublishersInGate == 0`. `Cause` is the
  caller's `ctx.Err()`.
- **Terminal result semantics**, defined explicitly:
  - a per-caller deadline error is returned to *that caller only* and is never
    stored as the batcher's terminal result, so a short deadline cannot poison a
    later caller;
  - the stored terminal result is written once, when the drain completes, and is
    `nil` on a completed drain;
  - processor errors and recovered panics are **not** returned by `Shutdown` or
    `Close`; they remain on `Errors()`, preserving today's separation
    (`pkg/batcher/batcher.go:130-140`). Error-channel overflow is reported via
    `Stats().DroppedErrors`, not via the shutdown result.
- `Close()` = `Shutdown` with a flat, configurable 30-second grace
  (`WithCloseGrace`). Delete the queue-size/interval timeout formula
  (`batcher.go:154-161`), the fixed 50ms sleep (`batcher.go:175`), and the
  drain-without-processing path (`batcher.go:114-116`).
- `fx.go` forwards the `StopHook` context (currently discarded at
  `fx.go:41-47`).

**Acceptance criteria**

Each of these is a required deterministic test, using injected barriers rather
than sleeps:

- `Shutdown` before `Start` with queued items drains them.
- `Shutdown` on an empty, never-started batcher reaches `closed` promptly.
- **`Shutdown` on an unstarted bounded batcher whose queue is full and which has
  a publisher parked in `Add` completes without deadlock**, across ≥300
  iterations. This is the specific ordering hazard the protocol addresses.
- `Start` racing `Shutdown` yields exactly one lifecycle; no goroutine leak.
- Two callers with different deadlines: the short one gets
  `ShutdownIncompleteError`, the long one observes drain completion and receives
  `nil`; the short caller's deadline error is not stored as terminal.
- `Shutdown` after terminal completion returns the stored terminal result.
- A processor that returns errors, and one that panics, both still yield a `nil`
  terminal shutdown result; the diagnostics appear on `Errors()`.
- Bounded `Enqueue` blocked on a full queue unblocks with `ErrClosing` when
  sealing begins; its reservation is rolled back.
- A timer tick delivered concurrently with the final drain flushes the partial
  batch exactly once, never twice, never zero times.
- Partial batch with interval far exceeding the grace period: processed, or
  reported pending; never discarded. (Today: silently dropped.)
- A non-returning processor keeps `IsClosed()` false and is documented, with a
  test asserting `IsClosing()` true and `IsClosed()` false.
- Fx: stop-hook context is used; a timed-out hook leaves no leaked goroutine and
  the drain continues.

**Dependencies**: Milestone 2.2.

### Milestone 2.4 — Bounded diagnostics and per-batch panic recovery

**Commit scope**

- Bounded error policy on the plain buffered diagnostics channel introduced in
  2.2 (default capacity 1,024, configurable), adding `Stats().DroppedErrors`;
  non-blocking drop-newest
  with a `DroppedErrors` counter. It must never block, since a blocking error
  send with no reader would deadlock a failing pipeline.
- **Single closer rule:** only the drain coordinator closes the error channel,
  and only after a `WaitGroup` confirms every potential sender (aggregator and
  all workers) has exited.
- Per-batch recovery with fixed `defer` ordering: mark in-flight → invoke →
  recover → publish diagnostic non-blockingly → decrement in-flight and pending
  exactly once → increment exactly one terminal category (`Completed` on a nil
  error, `Failed` on a non-nil error, `Panicked` on a recovered panic) → continue
  the worker loop.
  Recovery is scoped to the batch, never the worker, so a panic cannot silently
  reduce effective concurrency.

**Acceptance criteria**

- Error storm with no consumer: retained memory bounded; processing never
  blocks; `DroppedErrors` exact.
- Panic in the first, a middle, and the final batch during shutdown, at `n=1`
  and `n>1`: worker survives, accounting invariant holds, drain still completes.
- A panic never leaves `Pending` inflated (which would hang shutdown forever).
- Error or panic emitted concurrently with final worker exit and channel close
  produces no send-on-closed panic across ≥10,000 iterations.
- Measured recovery cost: ~3ns and **zero** added allocations on the non-panic
  path; asserted via `AllocsPerRun`.

**Dependencies**: Milestone 2.3 (the drain coordinator owns the single-closer
rule for diagnostics).

## Phase 3: Explicit concurrency

### Reason

`Config.Concurrency` defaults to 3 (`pkg/batcher/constants.go:12-13`) but is
read nowhere; observed parallelism is 1. This is the phase that actually
unlocks small windows, because it removes the
`effective window = max(window, processor_duration)` coupling.

Prototype evidence, 5ms window and 50ms processor at 10,000 items/s:

| Design                           | mean batch | p50       |
| -------------------------------- | ---------- | --------- |
| inline (today)                   | 536        | 27.7ms    |
| decoupled, n=1, blocking handoff | 484        | **70ms**  |
| decoupled, n=1, adaptive         | 492        | 25.0ms    |
| decoupled, n=4                   | 124        | **9.6ms** |

Naive decoupling at `n=1` is **2.5x worse**, because it adds a stage without
adding capacity. Phase 2 already has one unbuffered aggregation→processing handoff
for behaviour preservation; the relevant constraint is that `n=1` adds **no
additional worker-pool dispatch** and invokes the processor serially. Only `n>1`
introduces worker-pool dispatch and real concurrent processing.

### Milestone 3.1 — Concurrency configuration and ordering contract

**Commit scope**

- Add `WithConcurrency(n)`; set `DefaultConcurrency = 1`.
- Add `WithoutOrderedProcessing()` as an acknowledgement-only gate.
- Panic at construction when `n > 1` without the acknowledgement.
- Keep the `n = 1` path without an **additional worker-pool dispatch**. Phase 2
  already has one unbuffered aggregation→processing handoff; it is retained because
  removing it changes observable latency. At `n=1` the processor remains serial
  and no worker pool or extra dispatch queue is introduced.
- Document FIFO-by-publication-order and processor mutual-exclusion guarantees
  for `n = 1`.

**Acceptance criteria**

- `n = 1` ordering test across full, timer, and shutdown flushes, from a single
  producer, asserts exact FIFO.
- A concurrent-producer test asserts per-producer subsequence order is
  preserved, and explicitly does **not** assert cross-producer order.
- `n = 1` never invokes the processor concurrently, verified with a concurrency
  detector in the processor.
- `WithConcurrency(3)` without the gate panics; `WithConcurrency(1)` does not.
- Enqueue and end-to-end benchmarks at `n=1` show no regression versus 2.x
  baselines beyond predeclared thresholds.

**Dependencies**: Milestones 2.3 and 1.3.

### Milestone 3.2 — Worker pool, dispatch bound, and shutdown accounting

**Commit scope**

- Worker pool for acknowledged unordered processing; item order preserved
  *within* each batch.
- **Bounded dispatch:** the aggregator-to-worker handoff is unbuffered, so
  backpressure remains at admission and total accepted work stays bounded by
  `N + (1 + n) × BatchSize`. An unbounded dispatch queue would silently
  void the `MaxQueueSize` contract.
- Add `Stats().InFlight`, tracked per worker, so the snapshot separates queued,
  batch-held, and in-flight work, and so the drain waits for active workers.
- Reuse 2.4's recovery and single-closer rules in every worker.

**Acceptance criteria**

- With a slow processor, `n>1` prevents serial head-of-line blocking; scenario
  data shows the p50 improvement over `n=1` at the same window.
- **Semantic-difference matrix:** scenarios compare `n=1` and `n>1` timer
  cadence, batch-size distribution, admission blocking, pending depth, and
  flush timing under the same slow processor. Documentation labels the observed
  differences contractual: n=1 uses the existing serial aggregation→processor
  handoff; n>1 aggregates independently until the **unbuffered** worker dispatch
  is unavailable; cross-batch ordering is not guaranteed at n>1.

  The implementation was measured at 10k items/s with a 50ms processor and a 5ms
  window on **darwin/arm64, Apple M4 Pro, GOMAXPROCS=12, Go 1.26.5**: n=1 p50 70ms
  / mean batch 476; n=2 p50 25ms / 244; n=4 p50 15ms / 123; n=8 p50 4ms / 62.

  Treat the ratio between rows as the finding and the absolute values as this
  host's. A re-run on the same machine under different load measured n=1 p50 120ms
  and n=8 p50 54ms — the same direction, roughly 2x rather than 17x. Anything
  quoting a single multiplier out of this table will be wrong somewhere else, which
  is why the tests assert only the relative ordering (`n>1` p50 below `n=1` p50)
  and never a millisecond threshold.

  With a 100ms window and a 20ms processor (timer-bound rather than
  processor-bound), n=1 and n=4 both measured ~70ms p50, which is the control
  showing concurrency does not alter the batching rule.
- Total accepted-but-not-terminal items never exceed
  `N + (1 + n) × BatchSize + PublishersInGate` under a blocked processor;
  the test records `PublishersInGate` separately and demonstrates that its maximum
  equals the number of intentionally parked publisher goroutines.
- **Worker-mode drain test:** shutdown while a full batch is in flight on a busy
  worker and no items remain in the queue. The aggregator must terminate intake
  and wait for the worker, not block on a receive. A version of the loop
  conditioned on `Pending` instead of `IntakePending` must deadlock this test,
  proving the distinction is load-bearing.
- Shutdown with batches queued and workers active completes with the accounting
  invariant intact and nothing discarded.
- Goroutine count returns to the pre-construction baseline after `closed`, for
  `n=1` and `n>1`, using this leak-check protocol: sample after `Shutdown`
  returns, retry up to 50 times at 20ms intervals to allow scheduler settle,
  then require the count to be at most the pre-construction baseline. It is `<=`
  rather than `==` because an unrelated parallel test can retire a goroutine during
  the settle window, which would make an equality assertion flaky without
  indicating a leak. A count *above* the baseline is the leak signal, and any
  runtime-owned goroutine must still be named in the test.
- **Per-batcher goroutine budget is asserted by a test table** covering `new`,
  `running` at `n=1`, `running` at `n>1`, `draining`, and `closed`. Because 2.2
  replaces both `chann` queues with goroutine-free owned queues, the achievable
  budget is: `new` = 0; `running` at `n=1` = 2 (aggregator + serial processor);
  `running` at `n>1` = 1 + n; `closed` = 0. (An earlier prediction of 1 for `n=1`
  was invalidated by Phase 2, which deliberately retains the separate serial
  processor goroutine to preserve latency semantics.) Any deviation must be explained in the test rather than
  absorbed into an allowance. The drain coordinator runs on the caller's
  `Shutdown` goroutine and must not add a persistent goroutine; if an
  implementation needs one, the table and this budget must be updated with it
  named explicitly.
- Race tests cover worker completion vs close vs error publication vs enqueue.

**Dependencies**: Milestones 2.3, 2.4, 3.1, and 1.2 (scenario evidence for the
semantic-difference matrix).

## Phase 4: Observability and allocation

### Reason

Unbounded queues remain the default for spike-and-recover workloads, so
operators need a cheap way to distinguish a recovering spike from a runaway one.
`Len()` alone cannot: it conflates queued with in-flight work.

Counter placement is performance-critical. Four contended atomics per `Add`
measured 97-174 ns/op versus 37-45 ns/op for one — up to **4x**, worst at 4-12
producers. All non-reservation counters therefore live on the flush/worker path,
amortized across a batch.

### Milestone 4.1 — Complete the stats snapshot

**Commit scope**

`Stats()` already exists: 2.2 introduced it, 2.4 added `DroppedErrors`, 3.2 added
`InFlight`. This milestone completes and ratifies it.

- Add the remaining fields (`BatchHeld`, `BatchesFlushed`) so the full set is
  `Pending`, `IntakePending`, `PublishersInGate`, `Queued`, `BatchHeld`,
  `InFlight`, `Accepted`, `Completed`, `Failed`, `Panicked`, `BatchesFlushed`,
  `DroppedErrors`, `Rejected`.
- All counters are typed atomics (`atomic.Int64`, `atomic.Uint64`,
  `atomic.Bool`); `Queued` is read from the queue as specified below.
- Document the complete contract and assert the accounting invariants.
- `Queued` has exactly **one owner: the queue itself**, and one definition: items
  successfully published and not yet received by the aggregator.
  - bounded mode: `len(ch)` on the buffered channel — no counter, no lock.
  - unbounded mode: `len(items)` read under the slice mutex the queue already
    holds for `push`/`pop`, so it adds no new synchronization and no
    producer-side atomic.

  `Stats()` therefore reads `Queued` from the queue rather than from an atomic
  counter, and its godoc states this single field may take an uncontended lock in
  unbounded mode. It is deliberately *not* derived as
  `IntakePending - PublishersInGate`: that expression subtracts publishers that
  have already published but not yet left the gate, so it undercounts and can go
  negative. `Queued` is the value operators should scrape and alert on.
- Hot path performs exactly the budget defined in the protocol section: three
  seal loads (two in `enter()`, one in `leave()`) and five typed atomic RMWs
  (`gate++`, `Pending++`, `IntakePending++`, `Accepted++`, `gate--`) on the
  success path. Failure-path counters (`Rejected`, rollbacks) occur only on
  failure. Terminal counters update on the flush/worker path, amortized across a
  batch.
- **No `QueueHighWater`**: a correct high-water needs a per-item CAS-max, which
  measurement showed is the dominant contended cost. Operators derive max from
  sampled queue depth.
- `Len()` retained as the `Pending` snapshot. Note this is a **semantic change**
  that 5.1 must inventory: `Len()` now counts accepted-but-unfinished work
  including in-flight batches and gate reservations, so it can be non-zero while
  the queue is empty and can briefly exceed accepted work.

**Acceptance criteria**

- `AllocsPerRun` shows `Stats()` allocates zero and `Add` allocation count is
  unchanged from Phase 2.
- A successful `Add` performs the protocol's stated budget exactly — three seal
  loads and five typed atomic RMWs — verified by review checklist against the
  protocol section, not by an independently invented number. Failure-path
  counters are measured separately and reported in the benchmark table.
- Terminal counters are mutually exclusive: a test asserts an erroring processor
  increments only `Failed` and a panicking processor increments only `Panicked`.
- `Accepted == Completed + Failed + Panicked + Pending` is asserted **after
  terminal drain**, when `PublishersInGate == 0` and `Pending == 0`; it is never
  sampled mid-flight.
- The intake condition `IntakePending == 0` is asserted after final intake drain.
  The detailed `BatchHeld`/`InFlight` fields are documented as
  non-transactional diagnostics, not as an equation testable from a live
  `Stats()` snapshot.
- A test demonstrates that sampling `Stats()` during active load can legitimately
  show transiently inconsistent detailed fields, so the documented contract and
  the tests agree.
- Godoc states the snapshot is per-field atomic and not transactional.

**Dependencies**: Milestone 3.2.

### Milestone 4.2 — Conditional adaptive initial batch capacity

**Commit scope**

Only opened if 1.2 shows sparse-window allocation pressure above the
predeclared threshold. Both `rill` and the ported loop allocate the batch at
full `BatchSize`, so a 1ms window with `BatchSize=1000` can reserve ~8KB per
flush to hold one item.

The estimator is **recent-max, rounded to a power of two, clamped to
`[16, BatchSize]`**, re-evaluated on a fixed flush window, updated once per
flush in the aggregator goroutine only — no locks, no atomics, no pooling, no
ownership change.

EWMA-of-mean was evaluated and **rejected**: on alternating sparse/full traffic
it allocated 2.80MB versus 1.64MB for the current full-size strategy, i.e. worse
than doing nothing. Recent-max avoids this:

| Workload                | full (today) | EWMA×1.25  | recent-max pow2 |
| ----------------------- | ------------ | ---------- | --------------- |
| steady sparse           | 1.64MB       | 25.6KB     | **25.6KB**      |
| alternating sparse/full | 1.64MB       | **2.80MB** | 1.65MB          |
| bimodal                 | 1.64MB       | 0.60MB     | 1.65MB          |

**Acceptance criteria**

- Sparse scenarios improve allocated bytes/flush by a predeclared factor.
- **No scenario regresses beyond +2% allocated bytes versus the current
  full-capacity strategy**, specifically including alternating sparse/full,
  burst-after-idle, and bimodal.
- A stated retention bound: capacity never exceeds `BatchSize`, so a
  user-retained batch slice never holds more than today's worst case.
- Processors may still retain batch slices; a test mutates a retained slice
  across subsequent flushes and asserts no corruption.

**Dependencies**: Milestones 1.2 and 1.3. Conditional, not mandatory.

## Phase 5: Compatibility, guidance, and the default decision

### Reason

The preceding phases change public semantics. Users need an explicit
compatibility story, and the window recommendation must follow measurement
rather than intuition.

### Milestone 5.1 — API compatibility and release strategy

**Commit scope**

- Publish an API inventory covering: `Config.Concurrency` becoming meaningful
  and its default changing 3 → 1; `Add` becoming a counted rejection after
  sealing instead of panicking; `Close()` no longer abandoning the drain at its
  deadline; **`Len()` now counting accepted-but-unfinished work including
  in-flight batches, so it can be non-zero with an empty queue**; `Join`'s
  clarified quiescence-snapshot contract; **`Config()` returning a value snapshot
  instead of a live mutable pointer**, which removes the ability to reconfigure a
  running batcher and the data race that came with it; the removal of the `rill` and
  `chann` dependencies; **the module's minimum Go version moving from 1.22.4 to 1.25.0**
  (required by `golang.org/x/sys` v0.46.0, pulled in when dependencies were
  modernised in 2.1); and any `ProvideBatcherInFX` signature change.
- **`Config()` must stop exposing live mutable state.** It currently returns the
  internal pointer (`batcher.go:76-78`), letting callers mutate `ProcessorFunc`
  or `BatchSize` during processing — an exported data race that can invalidate
  every invariant in this plan. Return a value snapshot and add a race test
  proving live configuration cannot be mutated.
- **`Join` contract decision.** Document it as a *quiescence snapshot*,
  authoritative only after admission is sealed, and state that a concurrent
  `Add` can invalidate it. Keep the existing 1ms polling implementation, since
  it is off the operational hot path; benchmarks must not use it.
- Choose and document the release version, with migration notes.
- **Fx integration shape is decided, not deferred:** keep the existing
  `ProvideBatcherInFX(processorFactory, batchSize, batchInterval)` signature
  compiling unchanged, and add `ProvideBatcherInFXWithOptions[T](factory, ...Option[T])`
  for new configuration. No positional parameter is added to the existing
  function, so no current call site breaks. The stop hook forwards its context to
  `Shutdown` in both variants.
- Document Fx stop-hook behaviour for each case: normal drain returns `nil`; a
  stop-context deadline returns `ShutdownIncompleteError` (an Fx lifecycle
  error) while the background drain continues; an unstarted batcher with pending
  items drains; repeated app stop is idempotent.

**Acceptance criteria**

- Every behaviour change is listed with before/after and a migration note.
- A compile-level compatibility test builds the previous README examples, or the
  break is explicitly declared in release notes.
- `Config()` mutation can no longer affect a running batcher; proven under
  `-race`.
- Release notes state the ~25-30% `Add` cost of bounded-admission safety work.

**Dependencies**: Milestones 2.4, 3.2, 4.1.

### Milestone 5.2 — Documentation

**Commit scope**

- Remove the stale `StopProcessing` reference (`README.md:174`).
- Migrate the request-handler example (`README.md:243`) to `Enqueue`, since a
  handler should observe rejection; retain an `Add` example for deliberate
  best-effort use.
- Document: the unbounded default and its measured risk; the full capacity
  formula; error-drop policy; `Stats()` scraping; resumable shutdown; the
  `n > 1` acknowledgement; and per-batcher goroutine counts by state.
- Sizing guidance: expected batch ≈ `arrival rate × window`; 5-10ms is
  reasonable where that preserves useful batch sizes.
- State plainly that at `n = 1` a slow processor bounds effective batch timing,
  with the measured 5ms/50ms inversion as the worked example.

**Acceptance criteria**

- Every public addition has a documented contract and a compiling example.
- No documentation claims a smaller window provides overload protection.
- The measured inversion table appears in the tuning guidance.

**Dependencies**: Milestone 5.1.

### Milestone 5.3 — Default-window decision record

**Commit scope**

- Run the full matrix against the completed implementation.
- Publish the decision and its evidence.
- Change `DefaultBatchInterval` only if p99 latency, batching efficiency,
  allocation, and overload behaviour all support it.

**Acceptance criteria**

- Decision cites measured p50/p99/p99.9, batch-size distribution,
  downstream-call rate, allocation/GC, sustained and burst behaviour, and
  schedule-lateness validity.
- Any default change is accompanied by migration notes and scenario validation.
- If evidence is insufficient, the default is retained and only a configuration
  recommendation is published.

**Dependencies**: all prior mandatory milestones.

## Sequencing summary

```text
Mandatory path:
  1.1 ──► 1.2 ──► 1.3
  2.1 ──► 2.2 ──► 2.3 ──► 2.4 ──► 3.1 ──► 3.2 ──► 4.1 ──► 5.1 ──► 5.2 ──► 5.3

Conditional, off the critical path (nothing depends on it):
  4.2   (requires 1.2 and 1.3 only; may land any time after them, or never)

All declared edges:
  1.2←1.1        1.3←1.2
  2.1←1.1,1.3    2.2←2.1,1.3    2.3←2.2      2.4←2.3
  3.1←2.3,1.3    3.2←2.3,2.4,3.1,1.2
  4.1←3.2        4.2←1.2,1.3
  5.1←2.4,3.2,4.1    5.2←5.1    5.3←all mandatory

Every cross-phase edge points to an earlier phase, so merging the phase PRs in
order (1 → 2 → 3 → 4 → 5) satisfies all dependencies with no back-edges.
```

Phases are pull requests; milestones are commits inside them. Every milestone
commit leaves the library in a consistent state, so any prefix of the plan is a
valid stopping point:

- Stopping after **1.x**: measurement only, no behaviour change.
- Stopping after **2.1**: same semantics, `rill` removed, goroutines 6 → 5,
  and ~47% less memory per enqueued item.
- Stopping after **2.2**: admission is panic-free, accounting is sound, `chann`
  is gone (**measured 2 goroutines per running batcher**, down from 6, and enqueue
  54% faster), and the internal seal/gate/`noInput` coordinator is complete.
  `Close()` keeps its existing signature, no longer abandons the drain, and a
  minimal `Stats()` is public; only the resumable API and typed error are absent.
- Stopping after **2.3**: no accepted work is ever silently discarded. Verified
  directly: a partial batch with a 30s interval and a shorter grace now reports 50
  accepted / 50 processed / 0 pending, where it previously reported 50 accepted /
  0 processed / 50 phantom pending.
- Stopping after **2.4**: diagnostics bounded, panics survivable. Phase 2 as a
  whole is verified against the original defects: `Add` after `Close` is a counted
  rejection instead of a process panic, bounded mode caps the queue and reports
  rejections instead of growing the heap without limit, and goroutines per batcher
  are 2 with none leaked.
- Stopping after **3.x**: small windows are honoured under slow processors.
- Stopping after **4.x**: operators can see overload developing.
- Stopping after **5.x**: the compatibility story is published, the tuning guidance
  is measured rather than asserted, and `DefaultBatchInterval` is 10ms on the
  evidence recorded in `default-window.md`.

## Conclusion

Batcher becomes usable as a low-latency batching primitive instead of a
one-second queue with hidden failure modes.

Admission is memory-safe by construction: the input channel is never closed, so
no producer can panic during shutdown, and reservation-before-publish makes
negative pending counts impossible. Shutdown seals admission and either drains
accepted work or reports precisely what remains while continuing to drain, and
any caller may wait longer with a fresh context. The one case the library cannot
solve — a processor that never returns — is documented as a visible,
non-terminal `draining` state instead of silent data loss.

Serial users keep non-concurrent processing and FIFO by publication order. Users
who explicitly acknowledge unordered processing get real worker concurrency,
which is what stops a slow processor from inflating a 5-10ms window into tens or
hundreds of milliseconds. Bounded admission is available for process protection,
with a documented total-memory formula covering queue, aggregator, and in-flight
work; the unbounded default is retained for spike-and-recover workloads and is
now observable through an O(1) `Stats()` snapshot whose counters are placed to
keep the enqueue path measurably faster than today's.

Finally, the repository gains an open-loop measurement harness that resists
coordinated omission, reports admission and processing latency separately, and
carries predeclared thresholds. The 5-10ms recommendation is then made — or
declined — on evidence, with the honest caveat that a timer setting alone has
never been overload protection.
