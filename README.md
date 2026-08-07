# batcher

[![CI](https://github.com/NSXBet/batcher/actions/workflows/go.yml/badge.svg)](https://github.com/nsxbet/batcher)
![CI](https://github.com/NSXBet/batcher/actions/workflows/codeql.yaml/badge.svg)
[![Go Report Card](https://goreportcard.com/badge/github.com/NSXBet/batcher)](https://goreportcard.com/report/github.com/NSXBet/batcher)
[![Maintainability](https://api.codeclimate.com/v1/badges/868870a2b4f7f29512ad/maintainability)](https://codeclimate.com/github/NSXBet/batcher/maintainability)
[![Test Coverage](https://api.codeclimate.com/v1/badges/868870a2b4f7f29512ad/test_coverage)](https://codeclimate.com/github/NSXBet/batcher/test_coverage)

The dead-simple batching solution for golang applications.

With batcher you can easily batch operations and run them asynchronously in batches:

```go
package main

import (
    "fmt"
    "time"

    "github.com/NSXBet/batcher/pkg/batcher"
)

type BatchItem struct {
    ID   int
    Name string
}

func main() {
    // create a batcher
    batcher := batcher.New[*BatchItem](
        batcher.WithBatchSize[*BatchItem](100),                       // flush at 100 items,
        batcher.WithBatchInterval[*BatchItem](10*time.Millisecond),   // or when the oldest item is 10ms old.
        // then run this processor with each batch
        batcher.WithProcessor(func(items []*BatchItem) error {
            fmt.Printf("processing batch with %d items...\n", len(items))

            // do your thing :)

            return nil
        }),
    )
    // stop the batcher, and report if the drain did not finish in time
    defer func() {
        if err := batcher.Close(); err != nil {
            fmt.Printf("shutdown incomplete: %v\n", err)
        }
    }()

    // add operations to the batcher
    for i := 0; i < 1000; i++ {
        batcher.Add(&BatchItem{
            ID:   i,
            Name: fmt.Sprintf("item-%d", i),
        })
    }

    // wait for all batches to process...
    timeout := 10 * time.Second
    if err := batcher.Join(timeout); err != nil {
        fmt.Printf("timeout error: %v\n", err)

        return
    }

    // You should see something like (10 times):
    // processing batch with 100 items...
    // processing batch with 100 items...
    // processing batch with 100 items...
    // processing batch with 100 items...
    // ...
}
```

## Installing

Just `go get -u github.com/NSXBet/batcher` and you're ready to go!

## Usage

### Creating a batcher

To create a batcher you can use the `New` function:

```go
batcher := batcher.New[*BatchItem](
    batcher.WithBatchSize[*BatchItem](100),                       // flush at 100 items,
    batcher.WithBatchInterval[*BatchItem](10*time.Millisecond),   // or when the oldest item is 10ms old.
    // then run this processor with each batch
    batcher.WithProcessor(func(items []*BatchItem) error {
        fmt.Printf("processing batch with %d items...\n", len(items))

        // do your thing :)

        return nil
    }),
)
```

### Using a processor

You can pass a processor in the form of a function of signature `func(items []*BatchItem) error` to the batcher:

```go
batcher := batcher.New[*BatchItem](
    batcher.WithProcessor(func(items []*BatchItem) error {
        return nil
    }),
)
```

This function will be called with the batch of items to process.

You can also use a struct in order to have access to any dependencies you require:

```go
// 1. Create a Processor struct with all the dependencies you need.
type Processor struct {
    logger *zap.Logger
}

func NewProcessor() (*Processor, error) {
    logger, err := zap.NewDevelopment() // or whatever dependency you need
    if err != nil {
        return nil, err
    }

    return &Processor{
        logger: logger,
    }, nil
}

// 2. Implement the Processor interface function.
// Here you get to use any dependencies you injected into the processor.
func (p *Processor) Process(items []BatchItem) error {
    p.logger.Info("processing items", zap.Int("count", len(items)))

    return nil
}

// 3. Later when you are creating the batcher, pass the processor.Process function
// to the WithProcessor option to wire batcher with your processor struct.
processor, err := NewProcessor()
if err != nil {
    log.Fatalf("error creating processor: %v", err)
}

batcher := batcher.New[*BatchItem](
    batcher.WithProcessor(processor.Process),
)
```

### Adding items to the batcher

To add items to the batcher you can use the `Add` function:

```go
for i := 0; i < 1000; i++ {
    batcher.Add(&BatchItem{
        ID:   i,
        Name: fmt.Sprintf("item-%d", i),
    })
}
```

### Waiting for all batches to process

To wait for all batches to process you can use the `Join` function:

```go
timeout := 10 * time.Second
if err := batcher.Join(timeout); err != nil {
    fmt.Printf("timeout error: %v\n", err)
}
```

### Stopping the batcher

To stop the batcher, use `Close`:

```go
defer func() {
    if err := batcher.Close(); err != nil {
        // The drain did not finish within the grace period. Work is still being
        // processed in the background, so decide deliberately: wait longer with
        // Shutdown, or accept that the process is about to exit with work pending.
        log.Printf("batcher shutdown incomplete: %v", err)
    }
}()

// batcher.IsClosed() == true once the drain has completed
```

`Close` seals admission and drains work that has already been accepted, waiting up
to 30 seconds by default (configurable with `WithCloseGrace`). It is safe to call
multiple times and from multiple goroutines.

If the grace period expires, `Close` reports that the drain is incomplete — it does
**not** discard the remaining work, which keeps being processed in the background.
Do not write a bare `defer batcher.Close()`: it discards that report, and if the
process exits immediately afterwards, accepted work is lost without any signal.

When you need to control the wait, or to keep waiting after a timeout, use
`Shutdown`:

```go
ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
defer cancel()

if err := b.Shutdown(ctx); err != nil {
    var incomplete *batcher.ShutdownIncompleteError
    if errors.As(err, &incomplete) {
        // Still draining. Nothing was lost, and we can wait longer.
        log.Printf("%d items still pending", incomplete.Pending)

        err = b.Shutdown(context.Background())
    }
}
```

`Shutdown` is resumable: a later call waits on the same drain rather than starting
a new one, so an expired deadline never costs you accepted work.

### Concurrent processing and ordering

By default, Batcher processes **one batch at a time**. This guarantees that a
processor is never invoked concurrently and that a single producer's batches are
processed in publication order. Use this default when the processor holds
unsynchronised state or when cross-batch ordering matters.

A slow processor can make a small batch window ineffective at this setting: a 5ms
window behind a 50ms processor is effectively bounded by the processor.

The per-item worst case is additive, not a maximum. The interval timer starts when a
batch takes its first item, so while the aggregator is blocked handing the previous
batch to the busy worker no timer is running. An item arriving in that window waits
the remaining processor time *plus* a full interval.

When the processor is goroutine-safe and cross-batch ordering does not matter,
explicitly opt into worker concurrency:

```go
b := batcher.New(
    batcher.WithProcessor(processor),
    batcher.WithConcurrency[Item](4),
    batcher.WithoutOrderedProcessing[Item](), // required acknowledgement
)
```

`WithoutOrderedProcessing` is deliberately required. `WithConcurrency(n > 1)`
without it panics at construction, because concurrent processing gives up two
properties callers may rely on:

- batches may start, interleave, and complete in any order;
- `processor` may be invoked concurrently, so it must be goroutine-safe.

Items retain publication order **within** each batch at every concurrency. The
worker dispatch is unbuffered, so `WithMaxQueueSize` continues to bound queued
work; concurrency trades larger batches for more downstream calls, not an
unbounded hidden dispatch queue.

### Back-pressure and rejection

By default the queue is unbounded, which absorbs bursts well but turns a sustained
overload into unbounded memory growth. Note that shrinking the batch interval does
**not** bound queued work — only bounding the queue does.

To get back-pressure instead, bound the queue and use `Enqueue`, which reports why
an item was refused:

```go
b := batcher.New(
    batcher.WithProcessor(processor.Process),
    batcher.WithMaxQueueSize[Item](10_000),
)

if err := b.Enqueue(ctx, item); err != nil {
    // batcher.ErrClosing            -> shutting down
    // context.DeadlineExceeded      -> queue stayed full until the deadline passed
    // context.Canceled              -> queue was full and the caller cancelled
    return err
}
```

`Add` remains available as the fast path for best-effort use: it returns no error,
and after shutdown it is a counted no-op rather than a panic.

### Observing a batcher

`Stats` returns an allocation-free snapshot suitable for frequent scraping:

```go
s := b.Stats()

// Where work currently is. These three are disjoint, so together they show
// whether a backlog is waiting on the queue, on batching, or on the processor:
//   s.Queued    -> published, not yet taken by the aggregator (alert on this)
//   s.BatchHeld -> held by the aggregator: filling, or waiting for a free worker
//   s.InFlight  -> inside a processor call

// Totals.
//   s.Pending        -> conservative drain obligation: accepted-or-reserved work,
//                       including in-flight batches. It counts publishers that have
//                       reserved but not yet published, so it equals
//                       accepted-but-unfinished work only once PublishersInGate == 0
//   s.Accepted       -> successful enqueues
//   s.Rejected       -> refused enqueues
//   s.Completed / s.Failed / s.Panicked -> mutually exclusive terminal outcomes
//   s.BatchesFlushed -> batches emitted. After a terminal drain,
//                       (Completed+Failed+Panicked)/BatchesFlushed is mean batch size
//   s.DroppedErrors  -> diagnostics lost because Errors() was not drained
```

`BatchesFlushed` is the coalescing signal when tuning `WithBatchInterval`: a mean
batch size well below `BatchSize` means windows are closing on the timer rather than
filling, so the interval is costing latency without buying batching. Include every
terminal outcome in that mean — a failed or panicked batch was still flushed, so
dividing by `Completed` alone undercounts whenever the processor errors.

A rising `BatchHeld` with `InFlight` at its ceiling means batches are ready but every
worker is busy — that is the signal to raise `WithConcurrency`, not to shrink the
window.

The snapshot is eventually consistent, not transactional: each field is read
independently, so use it to observe where work sits, not as an accounting identity
while load is in flight.

### Handling Errors

Whenever the processor function returns an error, the batcher will send the error in the `Errors()` channel:

```go
for err := range batcher.Errors() {
    fmt.Printf("error processing batch: %v\n", err)
}
```

### Getting how many items are in the batcher

You can get how many items are in the batcher by using the `Len` function:

```go
fmt.Printf("batcher has %d items\n", batcher.Len())
```

## Available Options to configure batcher

| Option | Default | Purpose |
| --- | --- | --- |
| `WithProcessor(fn)` | no-op | The function called with each batch. |
| `WithBatchSize[T](n)` | 1000 | Flush as soon as a batch holds `n` items. |
| `WithBatchInterval[T](d)` | **10ms** | Maximum age of a partial batch. See [Choosing a batch interval](#choosing-a-batch-interval). |
| `WithMaxQueueSize[T](n)` | unbounded | Bound queued items to get back-pressure instead of unbounded growth. |
| `WithConcurrency[T](n)` | 1 | Process `n` batches at once. Requires `WithoutOrderedProcessing`. |
| `WithoutOrderedProcessing[T]()` | off | Acknowledges that `n > 1` gives up cross-batch ordering and processor mutual exclusion. |
| `WithCloseGrace[T](d)` | 30s | How long `Close` waits before reporting an incomplete drain. |
| `WithErrorBufferSize[T](n)` | 1024 | How many diagnostics `Errors()` buffers before dropping newer ones. |
| `WithSkipAutoStart[T]()` | off | Do not start processing in `New`; call `Start` yourself. |

## Choosing a batch interval

The interval is the **maximum age of a partial batch**, not a periodic flush tick:
the timer starts when the first item enters an empty batch. Sparse traffic therefore
waits the full interval per item, and a request crossing several batching services
pays up to one interval per hop.

Expected batch size is approximately **min(arrival rate x interval, BatchSize)**.
The arrival-rate term is measured, not estimated: at 10,000 items/s with a 10ms
interval it predicts 100 items per batch, and the measured mean is 100.

`BatchSize` is the binding constraint whenever it fills first, and the interval then
never fires. With the default `BatchSize` of 1,000, a service at 50,000 items/s
reaches 1,000 items after about 20ms, so configuring a 100ms interval changes nothing
— which is exactly what the 50,000/s row below shows.

Use the formula to pick an interval from the coalescing you actually need:

| Arrival rate | 10ms (default) | 100ms |
| --- | --- | --- |
| 1,000/s | ~11 items/batch, ~94 calls/s, p99 ~12ms | ~100 items/batch, ~10 calls/s, p99 ~100ms |
| 10,000/s | ~101 items/batch, ~99 calls/s, p99 ~10ms | ~1000 items/batch, ~10 calls/s, p99 ~99ms |
| 50,000/s | ~500 items/batch, ~100 calls/s, p99 ~10ms | ~1000 items/batch (capped by `BatchSize`), ~50 calls/s, p99 ~20ms |

Raise the interval when your downstream is expensive enough that the call rate at
10ms is unacceptable — most relevant below a few thousand items/s. The full matrix
and the reasoning behind the default are in
[`docs/improvements/default-window.md`](./docs/improvements/default-window.md).

**Shrinking the interval is not overload protection.** Measured with a saturated
downstream, every interval from 10ms to 1s accepted the same offered load while
completing the same amount, with the queue absorbing the difference. Use
`WithMaxQueueSize` and `Enqueue` for that, or upstream flow control.

**A slow processor bounds the effective interval.** At the default `Concurrency` of 1,
one batch must finish before the next starts, so the effective interval is
`max(BatchInterval, processor duration)`. Measured with a 50ms processor at 10k
items/s:

| Configured interval | Effective behaviour | p50 latency |
| --- | --- | --- |
| 5ms | bounded by the 50ms processor | **120ms** |
| 100ms | bounded by the interval | **100ms** |

The smaller interval is *worse*, because queueing dominates. If your processor is
slow, raise `WithConcurrency` (with `WithoutOrderedProcessing`) rather than lowering
the interval. With 8 workers, the same scenario measured a p50 latency of 4ms.

## FX Integration

The batcher can be easily integrated with [uber-go/fx](https://github.com/uber-go/fx) for dependency injection and lifecycle management. Here's how to use it:

```go
package main

import (
    "fmt"
    "time"

    "go.uber.org/fx"
    "go.uber.org/zap"
    "github.com/NSXBet/batcher/pkg/batcher"
)

type BatchItem struct {
    ID   int
    Name string
}

// RequestHandler handles incoming requests and enqueues them to the batcher
type RequestHandler struct {
    batcher *batcher.Batcher[*BatchItem]
}

// NewRequestHandler creates a new request handler with batcher dependency
func NewRequestHandler(b *batcher.Batcher[*BatchItem]) *RequestHandler {
    return &RequestHandler{
        batcher: b,
    }
}

// HandleRequest processes a single request by enqueueing it to the batcher
func (h *RequestHandler) HandleRequest(id int, name string) error {
    h.batcher.Add(&BatchItem{
        ID:   id,
        Name: name,
    })
    return nil
}

// Processor handles the actual batch processing
type Processor struct {
    logger *zap.Logger
}

// NewProcessor creates a new processor with its dependencies
func NewProcessor(logger *zap.Logger) *Processor {
    return &Processor{
        logger: logger,
    }
}

// Process implements the batch processing logic
func (p *Processor) Process(items []*BatchItem) error {
    p.logger.Info("processing items", zap.Int("count", len(items)))
    return nil
}

func main() {
    app := fx.New(
        // Provide the processor
        fx.Provide(NewProcessor),
        // Add the batcher module with a processor function that resolves dependencies
        batcher.ProvideBatcherInFX[*BatchItem](
            // This function is an FX resolver that will be called with the processor dependency
            func(processor *Processor) batcher.Processor[*BatchItem] {
                // Return the processor's Process method as the batch processor
                return processor.Process
            },
            2,                    // batch size
            time.Millisecond*100, // batch interval
        ),
        // Provide the request handler
        fx.Provide(NewRequestHandler),
    )

    // Start the app
    app.Run()
}
```

### Configuring the Fx batcher beyond size and interval

`ProvideBatcherInFX` takes batch size and interval positionally, which covers most
applications. For anything else — bounded queues, worker concurrency, close grace,
diagnostics buffer — use `ProvideBatcherInFXWithOptions`, which accepts the same
options as `New`:

```go
batcher.ProvideBatcherInFXWithOptions[*BatchItem](
    func(processor *Processor) batcher.Processor[*BatchItem] {
        return processor.Process
    },
    batcher.WithBatchSize[*BatchItem](500),
    batcher.WithBatchInterval[*BatchItem](10*time.Millisecond),
    batcher.WithMaxQueueSize[*BatchItem](10_000),
    batcher.WithConcurrency[*BatchItem](4),
    batcher.WithoutOrderedProcessing[*BatchItem](),
)
```

Do not pass `WithProcessor` here: the processor comes from the injected factory, and
supplying it as an option would bypass dependency injection.

Both variants forward the Fx stop-hook context to `Shutdown`, so your application's
shutdown deadline governs the drain:

| Case | Result |
| --- | --- |
| Normal drain | `nil` |
| Stop context expires | `*ShutdownIncompleteError` as an Fx lifecycle error; the drain continues in the background |
| Batcher holds queued work but never started | Drains anyway |
| Repeated stop | Idempotent |

The key part of the FX integration is the processor function passed to `ProvideBatcherInFX`. This function is an FX resolver that:

1. Takes any dependencies you need (like the `Processor` struct) as parameters
2. Returns a `batcher.Processor[T]` function that will be used to process batches
3. Can use any of the injected dependencies to implement the processing logic

This allows you to:

- Have access to all your dependencies in the processor function
- Keep your processor logic in a separate struct with its own dependencies
- Let FX handle the dependency injection and lifecycle management

The batcher will be automatically started when the FX app starts and stopped when the app stops.

## Developing batcher

### Tests

```sh
make unit      # the test suite
make guards    # what CI blocks on: race detector + allocation gates
```

`make guards` runs `go test -race ./...` plus the allocation gates recorded in
[`docs/improvements/thresholds.md`](./docs/improvements/thresholds.md) — the two
signals stable enough to gate on shared CI runners. See
[Contributing](#if-your-change-touches-performance) for when to run what.

Coverage for `pkg/batcher` is enforced at 80% by CI. The scenario harness under
`test/scenario` is excluded from that number: it is measurement infrastructure, not
shipped code, and counting it would invite tests written to satisfy a percentage.

### Measuring performance

Performance work here is expected to be evidence-led, so the repository carries a
measurement baseline rather than ad-hoc timing runs. There are two distinct tools,
and using the wrong one produces confidently wrong numbers.

#### Enqueue microbenchmarks — producer-side cost only

```sh
make bench-enqueue   # -benchmem -benchtime=3s -count=10, ready for benchstat
```

```text
BenchmarkBatcherEnqueue/batch_size=10-12          ...  ns/op   ...  B/op  0 allocs/op
BenchmarkBatcherEnqueue/batch_size=100-12         ...
BenchmarkBatcherEnqueueParallel/batch_size=100-12 ...
```

These measure **only** the cost of `Add` returning. They deliberately exclude batch
formation, the timer, and the processor, so they must never be reported as throughput
or end-to-end latency. Compare runs with `benchstat`; a single run is not evidence:

```sh
make bench-enqueue > new.txt
benchstat docs/improvements/baselines/enqueue-darwin-arm64.txt new.txt
```

Stored baselines live in `docs/improvements/baselines/` and record the toolchain and
CPU they were captured on, because comparing across machines is meaningless.

#### Scenario harness — end-to-end behaviour

```sh
go test ./test/scenario/            # correctness of the harness itself
make bench-matrix                   # full reporting sweep (minutes, informational)
```

The harness (`test/scenario`) answers the questions Go benchmarks cannot. It is
**open-loop**: arrival times are precomputed from a seeded schedule and are never
gated on completion, so a slow system cannot suppress offered load. That property is
what makes overload results trustworthy — a closed loop quietly stops offering work
exactly when the system starts struggling.

It reports distributions from raw samples rather than means:

- end-to-end p50/p95/p99/p99.9/max, with admission blocking separated from queueing;
- batch-size distribution, partial-batch fraction, downstream calls/s;
- allocations per item, heap high-water, GC activity, peak queue depth;
- **schedule lateness** — if the load generator itself fell behind, the run is marked
  invalid instead of being reported as batcher latency.

Two known defects are pinned as tests here, so a change that fixes one makes it fail
on purpose; see [Contributing](#if-your-change-touches-performance).

Guidelines that keep the numbers honest:

- Never use `Join` as a completion signal in a measurement. Its 1ms polling cannot
  resolve sub-10ms windows.
- Build payloads outside the timed region.
- Treat sub-millisecond windows as environment-sensitive; report them, do not gate on
  them.

#### CI lanes

| Lane | Trigger | Blocking? |
| --- | --- | --- |
| Race detector + allocation gates | every PR | **yes** |
| Scenario matrix and benchmark artifacts | scheduled / manual | no, informational |

Latency percentiles are reported but never gated: measured spread on shared runners
has exceeded the difference between the configurations under comparison, so a p99
threshold there would fire on noise. Historical microbenchmark trends are published
to [this page](https://nsxbet.github.io/batcher/dev/bench/).

## License

MIT.

## Contributing

Feel free to open issues and send PRs.

### Before you open a PR

```sh
make guards
```

That is what CI blocks on: the race detector plus the allocation gates in
[`docs/improvements/thresholds.md`](./docs/improvements/thresholds.md). Everything
else CI reports is informational.

### If your change touches performance

This repository keeps a **measurement baseline** so performance claims are backed by
evidence rather than intuition. Please use it — a plausible-sounding optimisation has
already been wrong here more than once.

**1. Measure before you change anything.** Capture the current numbers so you have
something to compare against:

```sh
make bench-enqueue > /tmp/before.txt
```

**2. Compare with `benchstat`, not by eye.** A single run is noise:

```sh
make bench-enqueue > /tmp/after.txt
benchstat /tmp/before.txt /tmp/after.txt
```

Only compare runs from the same machine. Stored baselines in
`docs/improvements/baselines/` record the toolchain and CPU they came from, precisely
because cross-machine comparison is meaningless.

**3. Use the right tool for the claim you are making.**

| Claim | Use |
| --- | --- |
| "`Add` is cheaper" | `make bench-enqueue` — producer-side cost only |
| "latency improved" | `test/scenario` harness — it measures end to end |
| "batching is more efficient" | `test/scenario` — reports batch-size distribution and downstream calls/s |
| "it survives overload" | `test/scenario` — open-loop, so load is not suppressed when the system slows |

The enqueue benchmarks deliberately exclude batch formation and the processor, so they
cannot support a latency or throughput claim. Reporting them as such is the most
common way to be confidently wrong here.

**4. Expect two tests to fail if you fix what they pin.** These encode known
defects, not desired behaviour:

| Test | Pins |
| --- | --- |
| `TestReproducesInlineSlowProcessorInversion` | A smaller batch window measures *worse* latency behind a slow processor at `Concurrency=1` |
| `TestSmallWindowGivesNoOverloadProtection` | A smaller window does not bound queued work |

If your change makes one fail, that is likely the change working. Say so in the PR
and update the test to pin the new behaviour, rather than deleting it.

**5. State what you measured, and on what.** Include the numbers, the command, and
the environment (`go version`, OS/arch, `GOMAXPROCS`, CPU). A result without its
environment cannot be reproduced or trusted.

**6. If the evidence does not support the change, say so.** Recording that an
optimisation did not help is a useful result and cheaper than discovering it later.
Adaptive batch capacity in this repository was gated on exactly that kind of
measurement, and an earlier estimator design was rejected because the numbers were
worse than doing nothing.
