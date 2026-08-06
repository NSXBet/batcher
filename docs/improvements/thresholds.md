# Performance thresholds

Predeclared gates for the blocking CI lane. Values are committed here so a
regression is caught by a number agreed in advance, not by opinion during review.

Latency percentiles are deliberately absent. Shared CI runners are far too noisy
for a p99 gate to be meaningful — measured spread within a single benchmark
configuration has exceeded the difference between configurations. Latency is
reported by the scenario matrix and compared as a trend; it never fails a PR.

## Reference environment

Thresholds are keyed to the environment. Re-baseline when any of these change.

| Field      | Value                                    |
| ---------- | ---------------------------------------- |
| Go version | 1.22.4                                   |
| Runner     | `ubuntu-latest` (GitHub-hosted, mutable) |
| Arch       | `amd64`                                  |

Local development figures in this repository's history were taken on Apple M4
Pro / darwin / arm64 / GOMAXPROCS=12 / Go 1.26.5. Those are recorded for
orientation only and are not gates.

## Allocation gates (blocking)

Allocation counts are stable even on noisy shared runners, which is why they are
the blocking signal rather than timings.

| Gate                                    | Threshold | Enforced by                              |
| --------------------------------------- | --------- | ---------------------------------------- |
| `Add` allocations, unbounded path       | exactly 0 | `TestAddAllocatesNothingPerCall`         |
| `Stats()` allocations                   | exactly 0 | added with `Stats()` in Phase 2          |
| Recovery wrapper allocations, non-panic | exactly 0 | added with panic recovery in Phase 2     |
| Scenario recorder allocations per item  | ≤ 1 total, and must not grow with run length | `TestHarnessRecorderDoesNotAllocatePerItem` (`test/scenario`) |

The recorder threshold is not "exactly 0" because `AllocsPerItem` measures the
whole pipeline, including Batcher's own per-batch allocations, not just the
recorder. Measured values are 0.03-0.04 allocations per item and do not grow with
run length, which is the property that matters: a recorder that allocated per item
would make every allocation figure it reports a measurement of itself.

`Add` currently allocates 0 per call in the timed region because the item is
constructed by the caller. Phase 2 replaces `chann` with a slice-backed unbounded
queue, at which point the gate becomes "zero allocations per `Add` in steady
state": `append` must allocate when it grows its backing array, so the gate is
measured after a stated warmup with queue capacity retained across drains, and
growth-path allocations are the one named exemption.

## Throughput gate (advisory until CI baseline exists)

| Gate                                        | Threshold                 |
| ------------------------------------------- | ------------------------- |
| `Add` ns/op regression (enqueue microbench) | ≤ +10% vs stored baseline |

Compare with `benchstat` over `-count=10`. A single run is not evidence. This is advisory until a scheduled run stores an ubuntu-latest/amd64 baseline; the current workflow uploads raw input but intentionally does not fail on this threshold.

## Goroutine gates (blocking, from Phase 2 onward)

| Gate                                | Threshold                          |
| ----------------------------------- | ---------------------------------- |
| Goroutines per idle batcher         | exactly 0                          |
| Goroutines per running `n=1`        | exactly 1 (aggregator)             |
| Goroutines per running `n>1`        | exactly 1 + n                      |
| Goroutines after terminal `closed`  | equal to pre-construction baseline |

Current `main` owns 6 goroutines per batcher. Phase 2.1 removes `rill`
(**measured 6 → 5**) and Phase 2.2 removes both `chann` relays and the input
forwarder they require (5 → 2: aggregator plus processor).

The 6 → 5 figure supersedes earlier 6 → 3 and 6 → 4 estimates. Fewer goroutines
were unreachable without changing observable behaviour: merging aggregation into
the processing loop inverted the documented latency baseline, and merging input
draining into aggregation regressed sequential `Add` by 39-50% because the
`chann` relay's bounded ingress was not drained promptly. The "exactly 1" row
above applies only from Phase 3, once the worker model is owned.

## Conditional gates (Milestone 4.2 only)

| Gate                                       | Threshold                    |
| ------------------------------------------ | ---------------------------- |
| Sparse-window allocated bytes/flush        | > 2 KB/flush to justify work |
| Allocation regression ceiling, any scenario | ≤ +2% allocated bytes        |

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
