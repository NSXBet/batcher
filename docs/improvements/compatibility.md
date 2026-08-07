# API compatibility and migration — v0.3.0

This release completes the performance and reliability work described in
[`plan-perf.md`](./plan-perf.md). It changes public behaviour, so every change is
listed below with what it was, what it is now, and what a caller has to do.

## Version

**v0.2.1 → v0.3.0.**

The module is `v0.x`, so a minor bump is the correct signal for breaking changes
under Go module semantics: no import path change is required, and callers opt in by
updating. A `v1.0.0` would be the wrong claim to make while the default batch
interval is still under review (see the [decision record](./default-window.md), which
keeps 1s for now and recommends 10ms as an explicit setting).

## Requires Go 1.25.0

The module's `go` directive moved **1.22.4 → 1.25.0**.

This was not a deliberate language-feature upgrade. Modernising dependencies pulled
in `golang.org/x/sys` v0.46.0, which requires 1.25.0. Every CI job now resolves its
toolchain with `go-version-file: go.mod` rather than a hardcoded version, so the
declared floor and the tested toolchain cannot drift apart.

**Migration:** build with Go 1.25.0 or later. There is no way to keep the older
floor while taking this release.

## Breaking changes

### `Config()` returns a value, not a pointer

| | |
| --- | --- |
| **Before** | `func (b *Batcher[T]) Config() *Config[T]` |
| **After** | `func (b *Batcher[T]) Config() Config[T]` |

The old signature handed out the live configuration struct. A caller could mutate
`BatchSize`, `BatchInterval` or `ProcessorFunc` while the aggregation goroutine was
reading them — an exported data race, and one that could change batching semantics
mid-batch. It was detected under `-race` once the aggregator began reading
`BatchSize` at start-up.

Reconfiguring a running batcher was never coherent, so the capability is removed
rather than synchronised.

**Migration:**

```go
// Before: read (or, unsafely, mutate) live config.
cfg := b.Config()
size := cfg.BatchSize

// After: Config() returns a value; read its fields directly, no pointer needed.
size := b.Config().BatchSize

// Before: mutate a running batcher (a data race, now impossible).
b.Config().BatchSize = 500

// After: construct a batcher with the configuration you want.
b = batcher.New(batcher.WithBatchSize[Item](500), batcher.WithProcessor(fn))
```

Code that only *reads* `Config()` compiles unchanged. Code that assigned through it
will now fail to compile, which is the intended outcome: it was racing.

### `DefaultBatchInterval` is unchanged at 1s

| | |
| --- | --- |
| **Before** | `DefaultBatchInterval = 1 * time.Second` |
| **After** | `DefaultBatchInterval = 1 * time.Second` |

An earlier draft of this release changed the default to 10ms on latency evidence. It
was not shipped. The measurement holds — at 1,000 items/s the 1s default measures p99
981ms against 12ms at 10ms — but the same tables show the downstream call rate moving
from ~1 call/s to ~94 calls/s at that rate. That is a CPU and downstream-load
increase for every caller who never configured an interval, so the low-latency value
is published as a recommendation instead of imposed as a default.

**Nothing to migrate.** Callers keep the behaviour they have today.

To adopt the recommendation, set it explicitly:

```go
b := batcher.New(
    batcher.WithProcessor(fn),
    batcher.WithBatchInterval[Item](10*time.Millisecond),
)
```

The decision, its full matrix, and the call-rate cost per arrival rate are in
[`default-window.md`](./default-window.md).

Note that shrinking the interval is **not** overload protection, and neither is
keeping it large: under a saturated downstream every interval from 10ms to 1s
accepted the same load. If the concern is memory rather than latency, bound the queue
instead:

```go
b := batcher.New(
    batcher.WithProcessor(fn),
    batcher.WithMaxQueueSize[Item](10_000), // back-pressure, reported via Enqueue
)
```

### `DefaultConcurrency` changes 3 → 1, and `Concurrency` now works

| | |
| --- | --- |
| **Before** | `DefaultConcurrency = 3`, read nowhere; observed parallelism always 1 |
| **After** | `DefaultConcurrency = 1`, honoured by a real worker pool |

The old constant was advertised but never used, so every batcher processed one batch
at a time. Making it functional at its old value would have silently parallelised
every existing caller's processor — including processors holding unsynchronised
state. The default therefore matches the behaviour callers already had.

Concurrency above 1 requires an explicit acknowledgement, because it gives up two
guarantees:

```go
b := batcher.New(
    batcher.WithProcessor(fn),
    batcher.WithConcurrency[Item](4),
    batcher.WithoutOrderedProcessing[Item](), // required; construction panics without it
)
```

**Migration:** no action to keep current behaviour. Callers who *want* parallelism
must add both options and ensure the processor is goroutine-safe.

### `Close()` no longer abandons the drain

| | |
| --- | --- |
| **Before** | On timeout, signalled done and discarded remaining batches without processing them |
| **After** | Reports an incomplete drain; accepted work keeps being processed |

The old behaviour was silent data loss. A partial batch with an interval beyond the
internal cap was dropped: 50 items accepted, 0 processed, and `Len()` still
reporting 50.

The timeout is now a flat, configurable 30s (`WithCloseGrace`) instead of a formula
derived from queue size and interval.

**Migration:** handle the error rather than discarding it.

```go
// Before: silently lost work on timeout.
defer batcher.Close()

// After: report it, and decide deliberately.
defer func() {
    if err := batcher.Close(); err != nil {
        log.Printf("batcher shutdown incomplete: %v", err)
    }
}()
```

If you need to keep waiting, use `Shutdown`, which is resumable:

```go
if err := b.Shutdown(ctx); err != nil {
    var incomplete *batcher.ShutdownIncompleteError
    if errors.As(err, &incomplete) {
        // Nothing was lost. Wait longer on the same drain.
        err = b.Shutdown(context.Background())
    }
}
```

### `Add` after shutdown no longer panics

| | |
| --- | --- |
| **Before** | Panicked with `send on closed channel`, crashing the caller's goroutine |
| **After** | A counted no-op; `Stats().Rejected` increments |

**Migration:** none required — this only removes a crash. Callers that need to
observe rejection should use `Enqueue`, which returns `ErrClosing`.

### `Len()` counts more than the queue

| | |
| --- | --- |
| **Before** | Roughly "items waiting", and could transiently go negative |
| **After** | Accepted-but-unfinished work: queued **plus** aggregator-held **plus** in-flight **plus** gate reservations |

`Len()` can therefore be non-zero while the queue is empty — for example while a
batch is inside the processor — and can briefly exceed accepted work while a
publisher is mid-publish. It can no longer be negative.

**Migration:** if you were using `Len()` as queue depth, use `Stats().Queued`, which
is exactly that. `Len()` remains the right value for "is there outstanding work".

### `ProvideBatcherInFX` is unchanged; new variant added

The existing signature compiles unchanged, deliberately: no positional parameter was
added for options most callers do not use.

```go
// Unchanged.
batcher.ProvideBatcherInFX[Item](factory, batchSize, batchInterval)

// New: full option set.
batcher.ProvideBatcherInFXWithOptions[Item](factory,
    batcher.WithBatchSize[Item](500),
    batcher.WithMaxQueueSize[Item](10_000),
    batcher.WithConcurrency[Item](4),
    batcher.WithoutOrderedProcessing[Item](),
)
```

Both variants forward the Fx stop-hook context to `Shutdown`, so the application's
shutdown deadline governs the drain. Previously the hook discarded that context and
used the batcher's own timeout, so an app with a longer grace period could not use it
and one with a shorter deadline could not bound it.

Fx stop-hook behaviour:

| Case | Result |
| --- | --- |
| Normal drain | `nil` |
| Stop context expires | `*ShutdownIncompleteError` as an Fx lifecycle error; drain continues |
| Batcher never started but holds queued work | Drains; `Shutdown` starts a consumer when one is needed |
| Repeated stop | Idempotent: later calls wait on the same drain and return their own wait result, so a caller that waits long enough gets `nil` even if an earlier caller timed out |

## Clarified contracts (no code change required)

### `Join` is a quiescence snapshot, not a barrier

`Join` returns when no work is pending. A concurrent `Add` can make work pending
again the instant it returns, so it is only authoritative **after admission is
sealed**. The 1ms polling implementation is unchanged: it is off the operational hot
path. Benchmarks must not use it as a completion signal, because 1ms polling cannot
resolve sub-10ms windows.

### `Stats()` is eventually consistent

Each field is an independent atomic load, so a live snapshot can show a state
transition partially applied. Use it to observe *where* work sits. The accounting
identity `Accepted == Completed + Failed + Panicked + Pending` holds only at
quiescence, after a terminal drain.

`Queued`, `BatchHeld` and `InFlight` are disjoint, so which one is growing is the
diagnosis. `Stats()` takes the queue mutex for a single length check to read
`Queued`; scraping frequently is fine, scraping in a tight loop is not.

## Removed dependencies

`github.com/destel/rill` and `golang.design/x/chann` are both gone. Batching and
queueing are now owned in-repo.

`chann` had to go for a correctness reason, not a preference: its unbounded queue
owns a relay goroutine whose only exit path closes its own ingress channel — the
channel publishers send to. That made "never close the input" and "leak no
goroutines" mutually exclusive, and the first is what removes the shutdown panic.

Measured effect: goroutines per running batcher went **6 → 2** (aggregator plus
serial processor; `1 + n` with `n` workers), with none leaked after shutdown.

## Performance notes

Two changes pull in opposite directions, and both are worth stating plainly.

**Enqueue got faster.** Removing the relay layers removed a channel hop and a
goroutine handoff per item: **-54% geomean sec/op** on the enqueue microbenchmarks
(`Add` 229.8 ns → 63.4 ns at small batch sizes), with 49-90% fewer bytes per
operation.

**Bounded admission costs something.** The safety machinery — the publisher gate,
reservation-before-publish, and rollback — adds work to every `Add`. Measured in
isolation during planning, that admission path was **~25-30% slower** per call than a
bare channel send. It is paid for by removing a class of shutdown panic and by making
back-pressure possible at all.

The net result is still faster than v0.2.1, because the relay removal outweighs the
gate. But a caller comparing a micro-benchmark of admission alone should expect the
gate cost, not be surprised by it.

**Allocation.** Batch slices are now sized from observed demand rather than always
reserving `BatchSize`. Sparse timer-driven workloads improved ~98% in allocated
bytes; full-batch workloads are unchanged; the worst case across the tested matrix is
+1.12%.

## What has not changed

- `Add`, `Join`, `Errors`, `Start`, `Close`, `IsClosed` all still exist with the same
  names.
- Batching rules are identical: the interval timer arms on the first item of an empty
  batch, a full batch flushes immediately, empty batches are never emitted, and
  shutdown flushes the partial batch. These are pinned by characterization tests
  written against v0.2.x behaviour before the engine was replaced.
- The queue is still **unbounded by default**. Bounding it is opt-in via
  `WithMaxQueueSize`.
- A processor may still retain the batch slice it receives. No pooling was
  introduced, precisely so this stays true.
