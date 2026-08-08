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
        batcher.WithBatchSize[*BatchItem](100),                 // will batch each 100 items.
        batcher.WithBatchInterval[*BatchItem](1*time.Second),   // or each second.
        // then run this processor with each batch
        batcher.WithProcessor(func(items []*BatchItem) error {
            fmt.Printf("processing batch with %d items...\n", len(items))

            // do your thing :)

            return nil
        }),
    )
    // stop the batcher
    defer batcher.Close()

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
    batcher.WithBatchSize[*BatchItem](100),                 // will batch each 100 items.
    batcher.WithBatchInterval[*BatchItem](1*time.Second),   // or each second.
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

To stop the batcher you can use the `StopProcessing` function:

```go
defer batcher.Close()

// batcher.IsClosed() == true after this point
```

This function is safe to be called multiple times as it will only stop the processor once.

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

- `WithBatchSize[*BatchItem](size int)`: sets the batch size.
- `WithBatchInterval[*BatchItem](interval time.Duration)`: sets the batch interval.
- `WithProcessor(func(items []*BatchItem) error)`: sets the processor function.

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
