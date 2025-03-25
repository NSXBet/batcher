# batcher

![CI](https://github.com/NSXBet/batcher/actions/workflows/go.yml/badge.svg)
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

## Tests

Just run `make unit` to run all tests.

We strive to have 100% test coverage, but for now we're close to 95%. It will do for now.

## Benchmarks

Just run `make bench` to run all benchmarks.

For the most up-to-date benchmarks in this repository, you can access [this
page](https://nsxbet.github.io/batcher/dev/bench/). These results are run every time someone merges a PR into the main
branch.

Our benchmarks are divided by batch size and should look like this (actual results depend on your machine):

```bash
Running benchmarks...
2024-06-07T00:21:10.619-0300    INFO    test/helpers.go:30        processing items {"count": 1000}
goos: linux
goarch: amd64
pkg: github.com/NSXBet/batcher/pkg/batcher
cpu: Intel(R) Core(TM) i9-14900KF
BenchmarkBatcherBatchSize10-24           4588717         255.4 ns/op
BenchmarkBatcherBatchSize100-24          5017683         254.8 ns/op
BenchmarkBatcherBatchSize1_000-24        4721426         235.4 ns/op
BenchmarkBatcherBatchSize10_000-24       4603827         245.5 ns/op
BenchmarkBatcherBatchSize100_000-24      4848703         244.8 ns/op
PASS
ok      github.com/NSXBet/batcher/pkg/batcher     26.988s
```

These benchmarks take into account the time it takes to add items to the batcher, not the time to process the batches as
that will vary depending on the processor function you pass to the batcher.

## License

MIT.

## Contributing

Feel free to open issues and send PRs.
