# lazybus

A lazygit-style terminal UI for **Azure Service Bus**, built for the on-call moment: open the dead-letter queue, look at the first message, fix it, put it back.

**Status:** pre-release, v0.1 in progress. This build browses (slice S1a): it lists the queues and topic subscriptions of a namespace and peeks their dead-letter queues. Peek never locks a message and never changes its DeliveryCount. Resubmit comes in the next slice. See [`docs/spec.md`](docs/spec.md) for the plan and [`CONTEXT.md`](CONTEXT.md) for the vocabulary.

## Try it

```sh
go run ./cmd/lazybus --demo
```

Press `?` for the keybindings, `q` to quit.

## Usage

```sh
lazybus --namespace sb-prod-weu.servicebus.windows.net   # az login credential
lazybus --connection-string 'Endpoint=sb://…;SharedAccessKeyName=…;SharedAccessKey=…'
lazybus --emulator                                       # local emulator, ports 5672 / 5300
lazybus --demo                                           # built-in demo data, no Azure
```

| Flag | Meaning |
|---|---|
| `--namespace <fqdn>` | Open this namespace with the Azure CLI credential (`az login`). A bare name gets `.servicebus.windows.net` appended. |
| `--connection-string <cs>` | Open the namespace of a SAS connection string. Also read from `LAZYBUS_CONNECTION_STRING`; the flag wins. |
| `--emulator` | Open the local Service Bus emulator on `localhost` with its development connection string. |
| `--emulator-amqp-port <n>` | Emulator AMQP port (default 5672). |
| `--emulator-admin-port <n>` | Emulator admin HTTP port (default 5300). Also used for a `--connection-string` with `UseDevelopmentEmulator=true`. |
| `--read-only` | Disable every state-changing key. |
| `--demo` | Run against built-in demo data. |

The sources combine: each one adds a namespace to the Namespaces panel. Discovering namespaces across subscriptions from `az login` comes in a later slice.

On the emulator the DLQ counts show `?`: its admin API does not report runtime counts.

The Messages panel shows 50 messages at a time; moving onto the last row loads the next 50. Every broker call times out after 30 s; errors show in the panel and the log.

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

Seed it with dead-lettered messages (JSON and plain-text bodies with typed application properties in `orders/$DLQ` and `order-events/billing/$DLQ`, plus the subscription-less topic `empty-topic`), then open it:

```sh
go run ./tools/seed          # drains the seeded DLQs first; -reset=false appends
go run ./cmd/lazybus --emulator --emulator-amqp-port 5682 --emulator-admin-port 5310
```

The seeder refuses connection strings without `UseDevelopmentEmulator=true`. Runtime entities such as `empty-topic` vanish when the emulator restarts; rerun the seeder.

Emulator end-to-end tests are not part of the hooks. Run them by hand against the running emulator (they seed it first; ports override with `LAZYBUS_EMULATOR_AMQP_PORT` / `LAZYBUS_EMULATOR_ADMIN_PORT`):

```sh
go test -tags emulator ./...
```

## License

Dual-licensed under either of [MIT](LICENSE-MIT) or [Apache-2.0](LICENSE-APACHE), at your option.
