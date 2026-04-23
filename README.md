# batcher

[![CI](https://github.com/NSXBet/batcher/actions/workflows/go.yml/badge.svg)](https://github.com/nsxbet/batcher)
![CI](https://github.com/NSXBet/batcher/actions/workflows/codeql.yaml/badge.svg)
[![Go Report Card](https://goreportcard.com/badge/github.com/NSXBet/batcher)](https://goreportcard.com/report/github.com/NSXBet/batcher)
[![Maintainability](https://api.codeclimate.com/v1/badges/868870a2b4f7f29512ad/maintainability)](https://codeclimate.com/github/NSXBet/batcher/maintainability)
[![Test Coverage](https://api.codeclimate.com/v1/badges/868870a2b4f7f29512ad/test_coverage)](https://codeclimate.com/github/NSXBet/batcher/test_coverage)

The dead-simple batching solution for golang applications.

With batcher you can easily batch operations and run them asynchronously in batches.
By default, batcher uses an unbounded internal queue so fire-and-forget producers
can enqueue quickly. If you configure `WithMaxQueueSize`, batcher switches to a
bounded queue that adds natural backpressure by blocking producers when the queue
is full.

This makes the package useful for two different producer styles:

- fire-and-forget request paths that prefer the default unbounded queue and `Add`
- pull-based consumers such as Kafka loops that benefit from bounded queues and
  `Enqueue(ctx, item)`

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

Use `Add` when you want the simplest producer API:

```go
for i := 0; i < 1000; i++ {
    batcher.Add(&BatchItem{
        ID:   i,
        Name: fmt.Sprintf("item-%d", i),
    })
}
```

`Add` is the convenience API:

- in unbounded mode, it remains effectively immediate unless shutdown has started
- in bounded mode, it blocks while the queue is full
- once shutdown has started, it silently drops the item

Use `Enqueue(ctx, item)` when the caller needs precise control over admission:

```go
ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
defer cancel()

err := batcher.Enqueue(ctx, &BatchItem{
    ID:   42,
    Name: "important-item",
})
if err != nil {
    // err can be context.Canceled, context.DeadlineExceeded, or batcher.ErrClosing
}
```

`Enqueue` is the explicit API:

- in unbounded mode, it usually returns immediately unless shutdown has started
- in bounded mode, it waits until there is queue capacity
- if the caller's context is canceled while waiting, it returns the context error
- if shutdown begins while waiting, it returns `batcher.ErrClosing`

Calls that race exactly with `Close()` may still be accepted if the send wins
before shutdown becomes visible. Accepted items are still drained by `Join()` or
`Close()`.

### Waiting for all batches to process

To wait for all batches to process you can use the `Join` function:

```go
timeout := 10 * time.Second
if err := batcher.Join(timeout); err != nil {
    fmt.Printf("timeout error: %v\n", err)
}
```

### Stopping the batcher

To stop the batcher you can use the `Close` function:

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

### Choosing unbounded or bounded mode

Batcher defaults to an unbounded internal queue:

```go
batcher := batcher.New[*BatchItem](
    batcher.WithBatchSize[*BatchItem](100),
    batcher.WithBatchInterval[*BatchItem](time.Second),
)
```

This mode is a good default for request-scoped code that wants a cheap enqueue
path and is willing to absorb bursts in memory.

If you want natural backpressure, configure a bounded queue:

```go
batcher := batcher.New[*BatchItem](
    batcher.WithBatchSize[*BatchItem](100),
    batcher.WithBatchInterval[*BatchItem](time.Second),
    batcher.WithMaxQueueSize[*BatchItem](1_000),
)
```

In bounded mode:

- producers block when the queue is full
- waiting producers unblock as soon as the consumer drains capacity
- `Add` silently drops once shutdown has started
- `Enqueue` returns `batcher.ErrClosing` once shutdown has started

The queue bound is about accepted-but-not-yet-drained items. It is most useful
for consumer loops where slowing the producer is better than allowing memory
growth during downstream slowness.

### Kafka-style bounded backpressure

Bounded queues pair well with pull-based consumers because backpressure on
enqueue naturally slows how fast records move from the consumer loop into the
batcher:

```go
for {
    fetches := client.PollFetches(ctx)
    if ctx.Err() != nil {
        break
    }

    fetches.EachRecord(func(record *kgo.Record) {
        item := &BatchItem{
            ID:   int(record.Offset),
            Name: string(record.Key),
        }

        if err := batcher.Enqueue(ctx, item); err != nil {
            switch {
            case errors.Is(err, context.Canceled):
                return
            case errors.Is(err, batcher.ErrClosing):
                return
            default:
                log.Printf("enqueue failed: %v", err)
            }
        }
    })
}
```

This pattern is safer than `Add` for shutdown-aware consumers because the caller
can observe cancellation, deadlines, and close-time rejection explicitly.

## Available Options to configure batcher

- `WithBatchSize[*BatchItem](size int)`: sets the batch size.
- `WithBatchInterval[*BatchItem](interval time.Duration)`: sets the batch interval.
- `WithMaxQueueSize[*BatchItem](size int)`: sets the maximum queue size. Values less than or equal to zero keep the queue unbounded.
- `WithProcessor(func(items []*BatchItem) error)`: sets the processor function.

## FX Integration

The batcher can be easily integrated with [uber-go/fx](https://github.com/uber-go/fx) for dependency injection and lifecycle management. Here's how to use it:

```go
package main

import (
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
            0,                    // max queue size; use > 0 to enable bounded mode
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
Pass a positive `maxQueueSize` to `ProvideBatcherInFX` when the Fx-managed
application should run in bounded mode.

## Tests

Just run `make unit` to run all tests.

We strive to have 100% test coverage, but for now we're close to 95%. It will do for now.

## Benchmarks

Just run `make bench` to run all benchmarks.

For the most up-to-date benchmarks in this repository, you can access [this
page](https://nsxbet.github.io/batcher/dev/bench/). These results are run every time someone merges a PR into the main
branch.

The benchmark suite is scenario-driven and compares unbounded and bounded queue
modes side by side. It includes enqueue-only throughput, end-to-end batch
throughput, concurrent producer contention, single-item latency, and
shutdown/drain scenarios.

The output should look roughly like this (actual results depend on your machine):

```bash
goos: linux
goarch: amd64
pkg: github.com/NSXBet/batcher/pkg/batcher
cpu: Intel(R) Core(TM) i9-14900KF
BenchmarkBatcherEnqueueOnly/unbounded/steady_high_volume_flushes_by_size_batch_100-24
BenchmarkBatcherEnqueueOnly/bounded_cap_100/steady_high_volume_flushes_by_size_batch_100-24
BenchmarkBatcherConcurrentProducers/unbounded/many_producers_flush_by_size_batch_100-24
BenchmarkBatcherConcurrentProducers/bounded_cap_100/many_producers_flush_by_size_batch_100-24
PASS
ok      github.com/NSXBet/batcher/pkg/batcher     26.988s
```

The point of these benchmarks is to compare bounded and unbounded tradeoffs in
the same workload families rather than to report one global "fastest" number.

## License

MIT.

## Contributing

Feel free to open issues and send PRs.
