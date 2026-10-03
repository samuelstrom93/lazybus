# lazybus

A lazygit-style terminal UI for **Azure Service Bus**, built for the on-call moment: open the dead-letter queue, look at the first message, fix it, put it back.

**Status:** pre-release, v0.1 in progress. This build is the skeleton (slice S0): the UI runs against built-in demo data only. Connecting to a real namespace comes in the next slice. See [`docs/spec.md`](docs/spec.md) for the plan and [`CONTEXT.md`](CONTEXT.md) for the vocabulary.

## Try it

```sh
go run ./cmd/lazybus --demo
```

Press `?` for the keybindings, `q` to quit.

## Development

Install the git hooks once per clone:

```sh
./scripts/install-hooks.sh
```

- `pre-commit`: `gofmt -l .` must be empty, `go vet ./...`.
- `pre-push`: `go test ./...` (unit tests and golden screens), `staticcheck ./...` if installed, `go build ./...`.

Golden screens live in `internal/ui/testdata/*.golden`. Regenerate them after an intended UI change and inspect the diff before committing:

```sh
go test ./internal/ui -update
```

Render all goldens into one HTML page:

```sh
go run ./tools/gallery -out <dir>
```

### Local Service Bus emulator

`emulator/` holds a Docker Compose setup for the Azure Service Bus emulator with lazybus' test entities. It maps the emulator to non-default host ports so it can run next to another emulator: AMQP on **5682**, admin/health HTTP on **5310**.

```sh
cd emulator && cp .env.example .env && docker compose up -d
```

Connecting lazybus to it lands in slice S1a:

```sh
go run ./cmd/lazybus --emulator --emulator-amqp-port 5682 --emulator-admin-port 5310
```

Emulator end-to-end tests are not part of the hooks. Run them by hand against the running emulator:

```sh
go test -tags emulator ./...
```

## License

Dual-licensed under either of [MIT](LICENSE-MIT) or [Apache-2.0](LICENSE-APACHE), at your option.
