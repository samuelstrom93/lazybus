# Releasing

Merging to main ships nothing. A release is an annotated `vX.Y.Z` tag on the tip of `origin/main`, made by `scripts/release.sh` on framen. Pushing the tag runs `.github/workflows/release.yml`, which tests the tag, gates on its benchmark snapshot and publishes the GitHub release with GoReleaser.

## The flow

```sh
./scripts/release.sh v0.2.0 --dry-run   # everything up to the tag; commits, pushes and tags nothing
./scripts/release.sh v0.2.0             # asks before it commits, pushes or tags
```

`scripts/release.sh vX.Y.Z`:

1. Checks the version is semver (`v1.2.3`, optionally `v1.2.3-rc.1`), fetches origin, and checks the tag is free locally and on origin.
2. Takes the tip of `origin/main` and lists the commits since the previous `v*` tag; no commits means nothing to release.
3. Checks out that commit in a temporary detached worktree, so nothing in your checkout leaks into the measurement.
4. If `benchmarks/release/bench-vX.Y.Z.json` is already committed there, reuses it. Otherwise measures it with `go run ./tools/benchreport run -version vX.Y.Z` (about 2 minutes).
5. Runs `go run ./tools/benchreport compare -version vX.Y.Z` and prints the Markdown report. A material regression aborts here; nothing is committed or tagged.
6. Asks for confirmation. If it measured a new snapshot, commits it (`chore(release): benchmark snapshot vX.Y.Z`) and pushes it to main as a plain fast-forward; if main moved meanwhile the push is rejected and nothing is tagged (rerun to release the new tip).
7. Tags that commit (`Release vX.Y.Z`) and pushes only the tag, from the worktree, so the pre-push hook runs the full gate on the tagged tree. If the push fails the local tag is deleted; a snapshot commit already pushed to main is reused by the rerun.

The worktree is removed on every exit. To follow the run: `gh run list --workflow release.yml --limit 1`, then `gh run watch`.

`--dry-run --target <rev>` measures and compares any commit instead of `origin/main`, for example to try the script or the benchmarks before they land on main. `--target` without `--dry-run` is refused: only the tip of main is ever tagged.

### The workflow

`release.yml` runs on a pushed `v*` tag:

| Job | What it does |
|---|---|
| guard | The tag is semver and its commit is an ancestor of `origin/main`; otherwise the run stops. |
| test | `go test ./...`, staticcheck, `go build ./...`, and every benchmark once (`-benchtime 1x`). |
| benchmark-gate | Fails if `benchmarks/release/bench-<tag>.json` is not in the tag. Runs `benchreport compare` (no measuring in CI: GitHub runners are too noisy and a different machine class), writes the report to the job summary and uploads it with the snapshot as the `benchmark-report` artifact. |
| release | Needs test and benchmark-gate. The only job with `contents: write`. GoReleaser builds the archives and publishes the release (not a draft), with the benchmark report as the release-notes footer and the snapshot attached. |

`workflow_dispatch` with input `tag` is the break-glass path: it re-runs the whole workflow for an existing tag, for example after a transient GoReleaser failure. It does not create tags. If the tag's GitHub release already exists, GoReleaser updates it: assets that already exist are deleted and uploaded again (`release.replace_existing_artifacts: true`), and the existing release notes are kept (GoReleaser's default `keep-existing` mode). A release GitHub marks immutable cannot be updated: GoReleaser refuses it and the re-run fails.

A `vX.Y.Z-rc.N` tag is published as a GitHub prerelease (`release.prerelease: auto`), so it never becomes the latest release.

Every action is pinned to a commit SHA with its version in a comment; update them together.

## Benchmarks

`internal/ui/bench_test.go`, all in-process on the fake bus with a fixed clock, in a 120×30 window. The names are stable: they become the metric names (`ViewLargeList/ns_per_op`), and renaming one drops its history and fails the next gate as `missing`.

| Benchmark | Measures |
|---|---|
| StartupFirstFrame | A fresh backend and model, the window size, the startup cascade (namespaces, entities, first peek) and the first `View`. |
| LoadLargeList | Opening a 10,000-message DLQ and paging through all of it (`G` until an empty page). Also `heap-bytes`: the live heap one fully loaded model retains (heap after GC with the model alive, minus the heap after GC before loading). |
| ViewLargeList | One `View` with the 10,000 rows loaded and the cursor mid-list. |
| NavStepLargeList | One cursor step (`j`, then `k`) through `Update` plus the `View` after it, 10,000 rows loaded. Paging is over, so no step loads; checked after the loop. |
| RepairFlow | `r` (pre-check, confirm popup), `y` (resubmit) and the `View` after it, on a fresh backend each time (set up outside the timer); checks the broker DLQ shrank by one. |

Plus `binary/size_bytes`: the size of `go build -trimpath -ldflags "-s -w"` of `./cmd/lazybus` with `CGO_ENABLED=0`, like the release build. One sample, the build is deterministic.

Not measured:

- **Process startup.** The real program needs a TTY and go.mod has no pty library; StartupFirstFrame covers the same work in-process.
- **Process RSS.** A benchmark process's RSS includes every benchmark that ran before; `heap-bytes` measures the retained heap of one model instead.

Run them by hand:

```sh
go test -run '^$' -bench . -benchmem ./internal/ui                        # quick look
go run ./tools/benchreport run -version v0.0.0-local -out /tmp/bench.json # the release measurement
go run ./tools/benchreport compare -head /tmp/bench.json -base benchmarks/release/bench-v0.1.0.json
```

### Measurement

`benchreport run` runs `go test -run '^$' -bench . -benchmem -count 1 -benchtime 1s -cpu 4 ./internal/ui` 15 times and takes one sample per benchmark per run (about 100 s on framen, plus the build). Repeating the whole run instead of `-count 15` spreads each benchmark's samples over the whole minute, so a burst of other load on the machine skews one sample of each benchmark rather than all samples of one. `-cpu 4` pins GOMAXPROCS so the GC-heavy benchmarks don't depend on how many cores happen to be idle. Flags: `-count`, `-benchtime`, `-cpu` (0 leaves GOMAXPROCS alone), `-pkg`, `-out`.

### Snapshot

`benchmarks/release/bench-vX.Y.Z.json`, one per release, committed to main before the tag:

```json
{
  "schemaVersion": 1,
  "version": "v0.1.0",
  "gitSha": "<commit measured; -dirty if the tree had changes>",
  "generatedAt": "2026-10-04T16:00:00Z",
  "runtime": {"goVersion": "go1.27.1", "goos": "linux", "goarch": "amd64", "cpu": "AMD RYZEN AI MAX+ 395 w/ Radeon 8060S"},
  "benchCommand": "go test -run '^$' -bench . -benchmem -count 1 -benchtime 1s -cpu 4 ./internal/ui",
  "samplesPerBenchmark": 15,
  "results": [
    {"name": "ViewLargeList/ns_per_op", "unit": "ns", "samples": [/* in measured order */],
     "median": 0, "p75": 0, "p95": 0, "min": 0, "max": 0, "comparable": true,
     "threshold": {"maxRegressionRatio": 1.15, "minAbsoluteRegression": 150000}}
  ],
  "acceptedRegressions": []
}
```

Metrics per benchmark: `ns_per_op` (unit `ns`), `bytes_per_op` (`bytes`), `allocs_per_op` (`count`), and `heap_bytes` (`bytes`) for LoadLargeList. Percentiles are nearest-rank: index `min(n-1, max(0, ceil(p/100·n)-1))` of the sorted samples. The `gitSha` of a snapshot is the commit that was measured; the snapshot commit itself is its child.

**Same machine class.** Timings are only comparable when measured on the same kind of machine: every snapshot comes from framen. The report warns (it does not fail) when base and head differ in Go version, OS, architecture or CPU. CI never measures, it only compares the committed snapshots.

### Thresholds

A metric is a material regression only when its head median exceeds the base median by more than **both** the ratio and the absolute floor; growth of exactly +15% or exactly the floor passes. The time floors are 5–8% of the medians below, rounded, and at least 3× the run-to-run difference measured below; at today's medians the ratio is the stricter test, and the floors keep a fast benchmark's small absolute wobble from failing a release. They live in one table, `tools/benchreport/thresholds.go`; a metric without an entry never fails as a regression (it still fails as `missing` if a later release drops it).

| Metric | Ratio | Floor | Median on framen |
|---|---:|---:|---:|
| StartupFirstFrame/ns_per_op | 1.15 | 20 µs | ~0.35 ms |
| LoadLargeList/ns_per_op | 1.15 | 4 ms | ~49 ms |
| ViewLargeList/ns_per_op | 1.15 | 150 µs | ~2.6 ms |
| NavStepLargeList/ns_per_op | 1.15 | 150 µs | ~2.5 ms |
| RepairFlow/ns_per_op | 1.15 | 30 µs | ~0.39 ms |
| StartupFirstFrame, ViewLargeList, NavStepLargeList, RepairFlow /bytes_per_op | 1.20 | 32 KiB | 416–478 KiB |
| LoadLargeList/bytes_per_op | 1.20 | 16 MiB | ~383 MiB |
| all /allocs_per_op | 1.20 | 100 | 1,518–11,285 |
| LoadLargeList/heap_bytes | 1.20 | 1 MiB | ~3.7 MiB |
| binary/size_bytes | 1.10 | 256 KiB | ~10.4 MiB |

The heap floor is 1 MiB, not 8 MiB: 10,000 loaded rows retain about 3.7 MiB, so an 8 MiB floor could never fire.

Run-to-run noise on framen. Measured 2026-10-04 with the defaults (two runs back to back, with other agents' work on the machine, load average 1.5–5):

| Metric | Run 1 median (min–max) | Run 2 median (min–max) | Δ median |
|---|---:|---:|---:|
| StartupFirstFrame/ns_per_op | 355.0 µs (320.3–585.6 µs) | 352.6 µs (323.9–372.5 µs) | -0.7% |
| LoadLargeList/ns_per_op | 49.33 ms (47.89–57.73 ms) | 48.04 ms (46.80–50.38 ms) | -2.6% |
| ViewLargeList/ns_per_op | 2.58 ms (2.55–2.85 ms) | 2.58 ms (2.55–2.66 ms) | -0.0% |
| NavStepLargeList/ns_per_op | 2.47 ms (2.45–2.55 ms) | 2.47 ms (2.44–2.66 ms) | -0.0% |
| RepairFlow/ns_per_op | 387.2 µs (378.8–467.9 µs) | 388.5 µs (378.4–455.2 µs) | +0.3% |
| LoadLargeList/heap_bytes | 3.7 MiB | 3.7 MiB | +0.0% |
| every bytes_per_op, allocs_per_op, binary/size_bytes | | | within ±0.1% |

How the defaults were chosen, worst time-median difference between two back-to-back runs:

| Setup | Worst Δ median |
|---|---:|
| `-count 10`, all cores | 8.0% (LoadLargeList) |
| `-count 10 -cpu 4` | 10.5% (LoadLargeList) |
| 10 interleaved runs, `-cpu 4` | 5.8% (StartupFirstFrame) |
| 15 interleaved runs, `-cpu 4` (default) | 2.6% (LoadLargeList) |

### Compare rules

`benchreport compare -version vX.Y.Z` compares `benchmarks/release/bench-vX.Y.Z.json` (head) with the snapshot of the latest **stable** version strictly below it in the same directory (base). Prerelease snapshots are never a base; a prerelease head is below its own stable version (`v0.2.0-rc.1` compares with `v0.1.x`, not `v0.2.0`). `-head` and `-base` override the files. With no base it passes: "First baseline — nothing to compare against."

Per metric (the union of both snapshots, sorted by name), comparing medians, with the threshold from head (else base):

| Case | Status | Fails |
|---|---|---|
| Only in head | new | no |
| Only in base, base comparable | missing | yes |
| Head not comparable, or no threshold | info | no |
| Grew by more than both the ratio and the floor (base 0: by more than the floor) | regression | yes |
| … and named in head's `acceptedRegressions` | accepted | no |
| Otherwise | ok | no |

Exit 1 when anything fails. The Markdown report goes to stdout, `-out` writes it, `-summary` appends it (the workflow's job summary).

### Accepting a regression

When a release knowingly gets slower (a feature that costs time, say):

1. On framen, on a clean checkout of main: `go run ./tools/benchreport run -version vX.Y.Z`.
2. Add an entry per metric to `acceptedRegressions` in `benchmarks/release/bench-vX.Y.Z.json`:
   ```json
   "acceptedRegressions": [
     {"name": "ViewLargeList/ns_per_op", "reason": "rows render the new age column"}
   ]
   ```
3. Commit and push the snapshot to main (the pre-push gate is skipped, see below).
4. `./scripts/release.sh vX.Y.Z`: it reuses the committed snapshot. The reason shows under the report in the release notes.

`benchreport run` keeps the `acceptedRegressions` of an existing snapshot it overwrites. A `missing` metric (a benchmark removed or renamed) cannot be accepted.

## Pre-push skip

The pre-push hook runs the full gate (`go test ./...`, staticcheck, `go build ./...`) except when every pushed update moves an existing ref (known, non-zero remote sha present locally) and `git diff --name-only <remote>..<local>` lists only paths under `benchmarks/release/`. That is the snapshot commit `release.sh` pushes to main. New refs (every tag push), deletions, unknown remote shas and mixed changes run the full gate. `scripts/test-pre-push.sh` checks these cases; CI runs it.
