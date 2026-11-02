# CRAP report

**CRAP** (Change Risk Anti-Patterns) weighs complexity against coverage per function:

    CRAP = cx² × (1 − cov)³ + cx

`cx` is cyclomatic complexity (gocyclo, the same measure as golangci-lint's `gocyclo`
rule), `cov` is statement coverage 0–1. A fully covered function scores cx; an uncovered
one cx² + cx. Complex, untested code ends up at the top.

```sh
./scripts/crap.sh            # top 15
./scripts/crap.sh --top 40
```

Coverage comes from the gate's suite (`go test ./...`, with `-coverpkg=./...` so the UI
tests count for `internal/bus` and `internal/bus/fake`). The emulator/azure E2E suites do
not run, so `internal/bus/azure` is only partly covered and `internal/seed` barely; read their
rows with that in mind. Test files, generated code and build-tagged files are excluded.
Output goes to `.tmp/crap/` (`crap.tsv` is the full list). gocyclo v0.6.0 is pinned and
fetched with `go run`.

## How to use it

The report is a **list of where tests help most**, not a gate: no threshold, and it never
runs in pre-push. High CRAP in `tools/*` weighs less than in `internal/bus` or `internal/ui`.

Coverage only says the code *ran*. Whether the tests would have *noticed* a bug is what
mutation testing answers: `scripts/mutation.sh` (see [mutation-testing.md](mutation-testing.md)).
A function at 100 % coverage can still have living mutants.
