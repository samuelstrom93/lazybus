# Complexity gate

`golangci-lint` fails the gate on a function that is too complex to read safely. It runs in
`.githooks/pre-push` and in CI (`ci.yml`, `release.yml`), pinned and fetched with `go run`:

```sh
go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.14.0 run ./...
```

The config is `.golangci.yml`. It enables three linters and nothing else; staticcheck runs on
its own as before.

| Linter | Measures | Fails when |
|---|---|---|
| `gocognit` | cognitive complexity: each branch and loop, plus one per level of nesting | > 20 |
| `gocyclo` | cyclomatic complexity: number of paths through the function | > 20 |
| `nolintlint` | the exemptions themselves | a `//nolint` without a named linter, without a reason, or that no longer suppresses anything |

Both thresholds are strict: a function at exactly 20 passes, 21 fails (the linters report
"is high (> 20)").

Not checked: `_test.go` files (table tests are long by design), `tools/` and `internal/seed/`
(dev tooling and emulator seeding, not part of the binary).

## Why cognitive complexity comes first

Cyclomatic complexity counts paths, so a flat `switch` with 40 one-line cases scores 40 even
though it reads like a table. Cognitive complexity charges for nesting: an `if` inside a `for`
inside a `switch` costs more than three `if`s in a row, which matches what makes code hard to
follow. A function over 20 on `gocognit` almost always wants a coherent helper pulled out. One
over 20 on `gocyclo` only (a message or key dispatch, an error mapping) is often fine as it is.

## When a function trips the gate

1. Extract a coherent helper: something with a name you would use in conversation ("the
   destinations of a send", "the pre-check of every item"), not a split to hit the number.
   Behaviour stays the same; pin it with tests first when coverage or mutation testing shows a
   gap (see [mutation-testing.md](mutation-testing.md), [crap-report.md](crap-report.md)).
2. If a split would make the code harder to read, exempt the function on its declaration line,
   naming only the linter that fires and saying concretely why:

   ```go
   func classify(err error) (bus.ErrorKind, string) { //nolint:gocyclo // flat mapping of SDK error codes ...
   ```

   A new exemption is called out in the handoff or PR, so a reviewer can disagree.

Current exemptions: `handlePanelKey` and `optionsBindings` (`internal/ui`, both linters),
`Update` (`internal/ui`, gocyclo) and `classify` (`internal/bus/azure`, gocyclo). Each says why
next to it.

## Next step

Lower both thresholds to 15 once the functions now between 16 and 20 are split where that reads
better. To list them, run the pinned command with a config copy that sets `min-complexity: 15`.
