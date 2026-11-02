# Mutation testing

Mutation testing changes the code a little at a time (`<` becomes `<=`, `==` becomes `!=`,
`+` becomes `-`) and runs the tests against each change (a *mutant*). If a test fails, the
mutant is **killed**. If everything stays green, it **lived**: the tests ran the line, but
no assertion noticed that it now means something else. Coverage says the code ran;
mutation testing says the tests would have noticed if it were wrong.

The tool is [gremlins](https://github.com/go-gremlins/gremlins) **v0.6.0**, pinned in
`scripts/mutation.sh` and fetched with `go run` (no go.mod change). On demand only, never
in the gate.

## Running it

```sh
./scripts/mutation.sh                          # whole module (~6.5 min on 32 cores)
./scripts/mutation.sh --since                  # only lines changed since origin/main (merge-base)
./scripts/mutation.sh --since HEAD~3           # ... since another ref
./scripts/mutation.sh --pkg internal/bus       # a directory and everything below it; repeatable
./scripts/mutation.sh -- --workers 4           # the rest goes straight to gremlins
```

Output: `.tmp/mutation/result.json` (gitignored) and a summary listing every LIVED mutant
as `file:line:column MUTATOR`. Excluded: `tools/*` and `internal/seed` (dev-only), test
files, generated code and build-tagged files. The emulator/azure E2E suites never run.

**When:** after a non-trivial logic change (repair, batch, edits, scan, guards), run
`--since` before handoff and report LIVED mutants in lines you touched. `--since` includes
uncommitted edits; a new file that is not `git add`-ed is invisible to it, so run
`git add -N <file>` first.

## Reading the result

| Status | Means | Do |
|---|---|---|
| KILLED | A test failed. | Nothing. |
| LIVED | The line ran; no assertion noticed. | A missing assertion, a real bug, or an *equivalent* mutant (the change cannot alter behaviour, e.g. a defensive bound that never triggers). Decide which. |
| NOT COVERED | No test runs the line. | A coverage question; see `scripts/crap.sh`. |
| TIMED OUT | The test did not finish. | Usually an infinite loop, so effectively killed. |

**Efficacy** = killed / (killed + lived). **Mutant coverage** = (killed + lived) / all.

## Gremlins pitfalls the script works around

1. **An empty `--diff` means "everything changed"** to gremlins, so the script exits itself
   when there are no Go changes. `go.mod` is at the repo root, so gremlins' `git diff`
   paths match module paths and `--diff` works as is (checked in a worktree too).
2. **Every mutant TIMES OUT** when `go test` answers from the test cache: gremlins sets
   each timeout to a multiple of the first coverage run. The script sets `GOFLAGS=-count=1`
   and `--timeout-coefficient 10`.
3. **False KILLED in `package main`** (`cmd/lazybus`): package mode guesses the package from
   the directory name, `go test` fails setup and the mutant counts as killed. The script
   always runs `--integration` (the whole suite per mutant, `-coverpkg ./...`), which also
   lets the UI tests kill mutants in `internal/bus` and `internal/bus/fake`.

## Baseline (2026-11-02, whole module, 6 min 30 s)

| | killed | lived | not covered | efficacy |
|---|---:|---:|---:|---:|
| Total | 1315 | 271 | 358 | 82.9 % (mutant coverage 81.6 %) |
| `internal/bus` | 131 | 20 | 28 | 86.8 % |
| `internal/bus/fake` | 96 | 14 | 7 | 87.3 % |
| `internal/bus/azure` | 94 | 9 | 104 | 91.3 % |
| `internal/recent` | 17 | 2 | 0 | 89.5 % |
| `internal/ui` | 940 | 222 | 208 | 80.9 % |
| `cmd/lazybus` | 37 | 4 | 11 | 90.2 % |

Many UI survivors are layout arithmetic (widths, padding, truncation) in `view.go`,
`search.go`, `jump.go` and the popups, where the golden screens cover the common sizes only.
In the repair flow (`internal/bus/repair.go`, `batch.go`, `internal/ui/repair.go`,
`internal/ui/batch.go`) no survivor was a real bug at the baseline; the notable gaps are a
caller cancel during the scan's receive (`repair.go` `timedOut`), the scan's batch size
near the limit, and which stop hint a bulk resubmit shows after a SendUncertain.

No threshold yet.
