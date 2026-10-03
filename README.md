# lazybus

A lazygit-style terminal UI for **Azure Service Bus**, built for the on-call moment: open the dead-letter queue, look at the first message, fix it, put it back.

**Status:** pre-release, v0.1 in progress. This build browses and resubmits (slices S1a and S2): it lists the queues and topic subscriptions of a namespace, peeks their dead-letter queues, and moves a dead-lettered message back to its queue or topic unchanged (DLQ Repair). Editing a message before resubmitting comes in a later slice. See [`docs/spec.md`](docs/spec.md) for the plan and [`CONTEXT.md`](CONTEXT.md) for the vocabulary.

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

The Messages panel shows 50 messages at a time; moving onto the last row loads the next 50. Every broker call times out after 30 s (a resubmit and its pre-check, which are several calls each, after 2 min); errors show in the panel and the log.

## Resubmit (DLQ Repair)

| Key | Where | Action |
|---|---|---|
| `r` | Messages, main pane (DLQ tab) | Resubmit the selected message: checks it is still there, then opens a confirm popup. |
| `y` | confirm popup | Confirm. Only `y` confirms; `enter` does nothing here. |
| `n` / `esc` | confirm popup | Cancel. Every other key is ignored: nothing happens until you confirm. |
| `m` | resubmit popup | Toggle the copy's MessageId between the original and a new one. |
| `c` | Messages, main pane | Finish Cleanup on a CleanupPending row (confirm popup; nothing is sent). |

After a successful resubmit the cursor stays on the same row, which now holds the next message, so `r y r y …` works down the list. `--read-only` disables `r` and `c`. The Active tab is read-only. While a resubmit or cleanup runs, keys wait, and `ctrl-c` and a closed terminal (SIGHUP) do not quit, so its outcome is never lost.

### Safety model

Peek never locks a message. A resubmit is a move: send a copy, then delete the original. It holds locks only inside that one call:

1. **Pre-check.** Peek the message by its sequence number, without a lock. If it is gone, the row is removed and nothing is touched. A message whose AMQP body is not a single data section (an AMQP value or sequence body) or whose message-id is not a string is refused, because the copy would lose data.
2. **Confirm.** The popup lists the source and target (a queue's DLQ goes back to that queue; a subscription's DLQ goes to its parent topic), the body, the dead-letter markers that are removed, the MessageId, the scan cost and target warnings: a topic delivers only to subscriptions whose rules match (no match means the copy is dropped), and a topic with no subscriptions is refused.
3. **By-sequence scan.** Service Bus cannot receive one message by sequence number, so lazybus opens a peek-lock receiver for this one call (closed before the call returns) and receives the DLQ until it finds the message. Messages ahead of it stay locked until the scan ends, then all are abandoned; **their DeliveryCount may increase by 1.** The scan locks at most the row's position plus 50 messages and stops well before the first lock expires.
4. **Send, then complete.** The copy keeps the body, application properties (with their types) and the copied system fields; `DeadLetterReason` and `DeadLetterErrorDescription` are removed. On a target with duplicate detection the copy gets a new MessageId by default, because a copy with the original id can be dropped as a duplicate and the original would still be deleted.

Every outcome is shown in the status bar and every broker call that changes state goes to the log:

| Outcome | Meaning | Row |
|---|---|---|
| Resubmitted | The copy is in the target and the original is gone. | Removed. |
| NotFound | The message was already gone (pre-check), or the scan stopped before reaching it; nothing sent. | Removed when gone; kept when the scan stopped, since it may still be in the DLQ. |
| LockLost | The lock (or the call's time) ran out before the send; nothing changed. | Kept; `r` retries. |
| SendFailed | The target rejected the copy; nothing changed. | Kept; `r` retries. |
| SendUncertain | The send ended without a clear answer: the copy may or may not be in the target. Never retried automatically. | Amber. `r` is allowed after you check the target; the retry reuses the same MessageId, so a target with duplicate detection drops a second copy. |
| CleanupPending | The copy is in the target but deleting the original failed. Never reported as success. | Red. `r` is blocked; `c` removes the original. |

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

Seed it with dead-lettered messages (JSON and plain-text bodies with typed application properties in `orders/$DLQ` and `order-events/billing/$DLQ`, one message in `orders-dedup/$DLQ` (a queue with duplicate detection), plus the subscription-less topic `empty-topic`), then open it:

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
