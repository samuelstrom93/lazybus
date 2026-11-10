# Agent notes

Contributor docs live in README.md (Development) and docs/. Short checklist for coding agents:

- **Gate** (what `.githooks/pre-push` runs): `go test ./...`, `staticcheck ./...` (if installed), the complexity gate, `go build ./...`. Install the hooks with `./scripts/install-hooks.sh`.
- **Complexity gate** (pre-push and CI): golangci-lint with gocognit and gocyclo, a function fails above 20 (`.golangci.yml`, docs/complexity.md). Prefer extracting a coherent helper over `//nolint`. A new `//nolint:<linter> // <reason>` needs a concrete reason and must be called out in your handoff.
- **Architecture tests** (`internal/archtest`, part of `go test ./...`) guard the package layering: the UI talks only to `bus.Backend`, transports are wired in `cmd/lazybus`, the Azure SDK stays in `internal/bus/azure`. When one fails, fix the dependency. Do not widen an allow-list silently: every entry needs a comment saying why, and the change must be mentioned in your handoff.
- **Mutation testing** (on demand, not in the gate): after a non-trivial logic change, run `./scripts/mutation.sh --since` before handoff and report LIVED mutants in lines you touched (missing assertion, real bug, or equivalent mutant — say which). See docs/mutation-testing.md.
- **CRAP report**: `./scripts/crap.sh` lists complex, poorly covered functions — where tests help most. See docs/crap-report.md.
- **Golden screens**: `go test ./internal/ui -update` after an intended UI change; inspect the diff.
- **E2E tests are manual**: `go test -tags emulator ./...` needs the local emulator (README, End-to-end tests). They are not in the gate, mutation or CRAP runs. The real-namespace suite (`go test -tags azure ./...`) runs against a standing Basic namespace (queues only; Standard-only subtests skip); `./scripts/e2e-standard.sh` runs all of it on a temporary Standard namespace before a release. It creates billable resources: run it only when asked.
- Changelog: user-facing changes go in CHANGELOG.md under Unreleased.
