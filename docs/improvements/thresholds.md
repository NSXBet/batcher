# Performance thresholds

Predeclared gates for the blocking CI lane. Values are committed here so a
regression is caught by a number agreed in advance, not by opinion during review.

Latency percentiles are deliberately absent. Shared CI runners are far too noisy
for a p99 gate to be meaningful — measured spread within a single benchmark
configuration has exceeded the difference between configurations. Latency is
reported by the scenario matrix and compared as a trend; it never fails a PR.

## Reference environment

Thresholds are keyed to the environment. Re-baseline when any of these change.

The Go version is whatever `go.mod` declares: CI resolves it with
`go-version-file: go.mod` rather than a hardcoded string, so this row and the
toolchain cannot drift apart. Phase 2 raised the floor from 1.22.4 to 1.25.0 when
`golang.org/x/sys` was updated.

Stored baselines record the toolchain they were captured on in their own header,
which is what makes them comparable or not; see Baselines below.

| Field      | Value                                    |
| ---------- | ---------------------------------------- |
| Go version | 1.25.0 (from `go.mod`)                   |
| Runner     | `ubuntu-latest` (GitHub-hosted, mutable) |
| Arch       | `amd64`                                  |

Local development figures in this repository's history were taken on Apple M4
Pro / darwin / arm64 / GOMAXPROCS=12 / Go 1.26.5. Those are recorded for
orientation only and are not gates.

## Allocation gates (blocking)

Allocation counts are stable even on noisy shared runners, which is why they are
the blocking signal rather than timings.

| Gate                                    | Threshold                                    | Enforced by                                                   |
| --------------------------------------- | -------------------------------------------- | ------------------------------------------------------------- |
| `Add` allocations, unbounded path       | exactly 0                                    | `TestAddAllocatesNothingPerCall`                              |
| Recovery wrapper allocations, non-panic | exactly 0                                    | `TestRecoveredPanicAddsNoSteadyStateAllocations`              |
| Scenario recorder allocations per item  | ≤ 1 total, and must not grow with run length | `TestHarnessRecorderDoesNotAllocatePerItem` (`test/scenario`) |
| Goroutines per running batcher, `n=1`   | exactly 2                                    | `TestGoroutineBudgetPerRunningBatcher`                        |
| `Stats()` allocations                   | exactly 0                                    | `TestStatsIsAllocationFree`                                   |

`Stats()` returns a value type built from atomic loads plus one queue length check
under the queue's existing mutex. It now carries its own allocation gate, because
metrics scraping runs continuously in production and generating garbage per scrape
would be a real cost.

The recorder threshold is not "exactly 0" because `AllocsPerItem` measures the
whole pipeline, including Batcher's own per-batch allocations, not just the
recorder. Measured values are 0.03-0.04 allocations per item and do not grow with
run length, which is the property that matters: a recorder that allocated per item
would make every allocation figure it reports a measurement of itself.

`Add` allocates 0 per call in the timed region because the item is constructed by
the caller. Phase 2 replaced `chann` with a slice-backed unbounded queue, so the
gate is now "zero allocations per `Add` in steady state": `append` must allocate
when it grows its backing array, so the gate is measured after a stated warmup
with queue capacity retained across drains, and growth-path allocations are the
one named exemption.

## Throughput gate (advisory until CI baseline exists)

| Gate                                        | Threshold                 |
| ------------------------------------------- | ------------------------- |
| `Add` ns/op regression (enqueue microbench) | ≤ +10% vs stored baseline |

Compare with `benchstat` over `-count=10`. A single run is not evidence. This is advisory until a scheduled run stores an ubuntu-latest/amd64 baseline; the current workflow uploads raw input but intentionally does not fail on this threshold.

## Goroutine gates (blocking)

These are the gates in force today. The n=1 row agrees with the allocation table
above; `n>1` is now a real, acknowledged configuration and has its own worker-pool
gate. Every count is enforced by a test, not merely documented.

| Gate                               | Threshold                                 | Enforced by |
| ---------------------------------- | ----------------------------------------- | ----------- |
| Goroutines per unstarted batcher   | exactly 0                                 | `TestWorkerGoroutineBudget` |
| Goroutines per running `n=1`       | exactly 2 (aggregator + serial processor) | `TestGoroutineBudgetPerRunningBatcher`, `TestWorkerGoroutineBudget` |
| Goroutines per running `n>1`       | exactly 1 + n (aggregator + workers)      | `TestWorkerGoroutineBudget` |
| Goroutines after terminal `closed` | at most the pre-construction baseline     | `TestGoroutineBudgetPerRunningBatcher`, `TestWorkerGoroutineBudget` |

Current `main` owns 6 goroutines per batcher. Phase 2.1 removed `rill`
(**measured 6 → 5**) and Phase 2.2 removed both `chann` relays and the input
forwarder they required (**measured 5 → 2**: aggregator plus processor), enforced
by `TestGoroutineBudgetPerRunningBatcher`.

Removing the relays also removed a channel hop and a goroutine handoff per item.
Measured against the stored baseline: **-54% geomean sec/op** on the enqueue
microbenchmarks (`Add` 229.8ns → 63.4ns at small batch sizes) and -49% to -90%
bytes/op. This is the one place in the plan where a safety change also made the
hot path materially faster.

The 6 → 5 figure supersedes earlier 6 → 3 and 6 → 4 estimates. Fewer goroutines
were unreachable without changing observable behaviour: merging aggregation into
the processing loop inverted the documented latency baseline, and merging input
draining into aggregation regressed sequential `Add` by 39-50% because the
`chann` relay's bounded ingress was not drained promptly.

Both earlier estimates assumed aggregation and processing could share one
goroutine. They cannot without changing observable latency, which is why the
enforced count is 2 at `n=1`. Phase 3 owns the worker model on top of that base:
`n=1` is 2 goroutines and `n>1` is `1 + n`, measured as n=2→3, n=4→5, n=8→9 with
zero leaked.

## Conditional gates (Milestone 4.2 only)

| Gate                                        | Threshold                    | Measured        |
| ------------------------------------------- | ---------------------------- | --------------- |
| Sparse-window allocated bytes/flush         | > 2 KB/flush to justify work | 55,944 B/flush  |
| Allocation regression ceiling, any scenario | ≤ +2% allocated bytes        | +1.12% (worst)  |

Milestone 4.2 was **triggered and implemented**. Sparse-window waste measured far
above the justification threshold: at a 1ms window with `BatchSize=1000` the
aggregator reserved 55,944 B/flush to hold a single item, and 559,944 B/flush at
`BatchSize=10000`.

Results per workload, adaptive versus the previous full-capacity strategy:

| Workload                | Change  |
| ----------------------- | ------- |
| steady sparse           | -97.9%  |
| small batches           | -93.3%  |
| full batches (control)  | -0.00%  |
| alternating sparse/full | +1.12%  |
| burst after idle        | -72.2%  |
| bimodal                 | -3.45%  |

The alternating case is the one that killed the rejected EWMA estimator, which
allocated *more* than doing nothing there. Recent-max keeps it inside the +2%
budget.

These figures are **measured evidence, not an enforced gate.** `BenchmarkCapacity*`
reports allocated bytes for each workload but does not compare them against the +2%
limit, and `capacity_test.go` asserts estimator behaviour (bounds, adaptation,
decay) rather than allocation deltas. Re-check the numbers with:

```sh
go test -run='^$' -bench='BenchmarkCapacity' -benchmem -count=1 ./pkg/batcher
```

Turning this into a gate needs a stored allocation baseline for the reference
runner, which does not exist yet for the same reason the throughput gate is still
advisory. Until then, a regression here is caught by reading the benchmark output,
not by CI.

The second gate must hold for alternating sparse/full, burst-after-idle, and
bimodal workloads, not just the sparse case the optimisation targets. An
EWMA-of-mean estimator was rejected precisely because it regressed alternating
traffic to 2.80 MB against 1.64 MB for the current full-capacity strategy.

## Baselines

Stored benchmark baselines live in `docs/improvements/baselines/`.

- `enqueue-darwin-arm64.txt` — local developer baseline (Apple M4 Pro, Go 1.26.5).
  Useful for spotting a large local regression while iterating. **Not** the CI
  reference; do not compare CI results against it.

A baseline for the CI reference runner (`ubuntu-latest`, amd64) must be recorded
from a scheduled run of `performance.yml` before the ns/op gate can be enforced.
Until then, treat the throughput gate as advisory and rely on the allocation and
race gates.

Regenerate with:

```sh
make bench-enqueue > docs/improvements/baselines/enqueue-<env>.txt
```

Compare two files with `benchstat old.txt new.txt`.
