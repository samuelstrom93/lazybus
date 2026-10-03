# lazybus v0.1 — spec

A lazygit-style terminal UI for **Azure Service Bus only**. Built for the on-call moment: open the dead-letter queue, look at the first message, fix it, put it back.

Status: ready for the build session. Section "Open questions" is asked to Samuel in **one round** before slice S0; everything else is decided.

## 1. Why it exists (the wedge)

No existing terminal tool does all four in one move (prior-art check 2026-10-03: lazyaz, Quetty, service-bus-tui, CosX service-bus-explorer-tui, Net.Azure.ServiceBusConsole):

1. edit a dead-lettered message — application properties, Subject/ContentType, body in your own `$EDITOR`,
2. **move** it out of the dead-letter queue (no copy left behind),
3. strip the dead-letter markers from the outgoing copy,
4. in a lazygit-style UI made only for Service Bus.

That is **DLQ Repair** (see `CONTEXT.md`). Everything in v0.1 serves it or the browsing that leads to it.

## 2. Principles (non-negotiable)

- **Never hold a lock outside one operation.** Browsing is peek only. A lock exists only inside a DLQ Repair call and is released (complete/abandon) before the call returns. See ADR 0002.
- **Peek is real peek.** Never emulate peek with peek-lock + abandon (it bumps `DeliveryCount`).
- **Destructive = confirm.** Every action that changes broker state opens a confirm popup that defaults to cancel.
- **`--read-only`** disables every state-changing key; the UI shows `READ-ONLY` in the status bar.
- **Transparency.** Every broker call that changes state is written to the command log with entity, sequence number and outcome.
- **State is visible.** After every action the screen shows the new state and the outcome; nothing happens silently.

## 3. Stack (decided, ADR 0001)

- Go (toolchain on framen: 1.27.1; `go.mod` targets the oldest Go the deps require).
- TUI: `charm.land/bubbletea/v2` (or `github.com/charmbracelet/bubbletea/v2` — use whatever path the current v2 release documents), `bubbles` (list, table, textinput, viewport, help, key), `lipgloss`. Golden tests with `teatest` (v2-compatible variant).
- Service Bus: `github.com/Azure/azure-sdk-for-go/sdk/messaging/azservicebus` (data plane) + its `admin` package (entities + runtime counts).
- Auth/discovery: `azidentity` (`AzureCLICredential` first), `armsubscriptions` + `armservicebus` for namespace discovery.
- Clipboard: OSC 52 escape sequence (works over SSH; Samuel runs it on framen over SSH from a MacBook).
- Module: `github.com/samuelstrom93/lazybus`. Binary: `lazybus`.

Check each dependency's current major version and docs (Context7 / pkg.go.dev) before use; don't code from memory.

## 4. Layout

```
┌ 1 Namespaces ─────────┐┌ Body │ Properties │ System ─────────────────────────────┐
│▸ sb-prod-weu          ││ {                                                      │
│  sb-test-weu          ││   "orderId": 1002,                                      │
├ 2 Entities ───────────┤│   "status": "pending"                                  │
│▸ orders        DLQ 37 ││ }                                                      │
│  invoices      DLQ  3 ││                                                        │
│  order-events/billing ││                                                        │
│                DLQ 12 ││                                                        │
├ 3 Messages  DLQ│Active┤│                                                        │
│▸  2  13:00 SchemaMis… ││                                                        │
│   3  14:00 MaxDelive… ││                                                        │
│   4  15:00 Downstrea… │├ Log ───────────────────────────────────────────────────┤
│                       ││ 13:02 resubmit orders/$DLQ seq 1 → orders  ok          │
└───────────────────────┘└────────────────────────────────────────────────────────┘
 r resubmit  e edit prop  E edit body  [ ] tab  / filter  : jump  ? help   READ-ONLY
```

- Left column: three numbered side panels. The focused one grows (lazygit "expandFocusedSidePanel").
- Right: main pane with tabs **Body / Properties / System**, always previewing the selected message. Below it a small **Log** (command log).
- Bottom: options bar with the keys valid in the current context, plus mode flags (`READ-ONLY`, pending-edit count).
- Must render cleanly at 120×30 and degrade (truncate with `…`, never wrap boxes) down to 80×24.

Panel contents:
1. **Namespaces** — discovered via ARM for the `az` login (all subscriptions the user can read), plus any `--connection-string`/`--emulator` entry. Shows name; subscription in the main pane when focused.
2. **Entities** — queues and topic subscriptions of the selected namespace (topics themselves hidden; they have no DLQ). Columns: path, DLQ count; active count in the main pane. Sorted by path; `s` toggles sort by DLQ count (desc).
3. **Messages** — panel tabs **DLQ │ Active** (`[`/`]` when this panel is focused). Rows: sequence number, enqueued time (local), dead-letter reason (DLQ) or subject (Active), message id. Order = peek order (lowest sequence first). Page of 50; `L` loads the next page.

Main pane tabs:
- **Body** — pretty-printed JSON when it parses, raw text otherwise; scrollable (viewport). Pending body edit shown with a `(edited)` marker.
- **Properties** — application properties: key, type, value. Dead-letter markers (`DeadLetterReason`, `DeadLetterErrorDescription`) shown dimmed with `✕ removed on resubmit`. Pending Property Edits shown inline: `*` changed (with old value), `+` added, `−` removed.
- **System** — MessageId, CorrelationId, Subject ✎, ContentType ✎, SessionId, PartitionKey, To, ReplyTo, TTL, DeliveryCount, EnqueuedTime, SequenceNumber, DeadLetterSource. ✎ = editable in repair.

## 5. Keymap

Rule: same letter = analogous action in every context; lowercase = common action, uppercase = heavier variant. Only the top context of the context stack receives keys, so text fields in popups never trigger commands.

| Scope | Key | Action |
|---|---|---|
| Global | `1` `2` `3` | focus side panel |
| | `0` / `enter` on a message | focus main pane |
| | `h` `l` / `←` `→` | previous / next panel |
| | `j` `k` / `↓` `↑` | move |
| | `g` `G` | top / bottom |
| | `ctrl-d` `ctrl-u` | half page down / up |
| | `[` `]` | previous / next tab of the focused panel |
| | `/` | filter focused list (`esc` clears) |
| | `:` | jump to entity by name (fuzzy, current namespace) |
| | `R` | refresh focused panel |
| | `y` / `Y` | copy body / copy MessageId (OSC 52) |
| | `?` | searchable keybindings menu |
| | `esc` | back / close popup |
| | `q` | quit (from root context) |
| Messages (DLQ tab) / main pane | `r` | resubmit (DLQ Repair) — confirm popup |
| | `e` | Property Edit popup (key, type, value) — in Properties tab prefilled from the selected row; in System tab edits Subject/ContentType |
| | `a` | add property (Properties tab) |
| | `d` | mark selected property for removal (Properties tab); toggles |
| | `E` | edit body in `$EDITOR` (`$VISUAL`, then `$EDITOR`, then `vi`) |
| | `x` | discard all pending edits for this message (confirm if any) |
| Messages | `L` | load next page |
| Entities | `s` | toggle sort path / DLQ count |
| Popups | `enter` / `y` | confirm |
| | `esc` / `n` | cancel |
| | `tab` | next field |

Property Edit popup: fields Key, Type (selector: String, Int, Long, Double, Bool, Guid, DateTime — String default for new keys; existing keys keep their type), Value. Validation inline; invalid value keeps the popup open with the error.

## 6. DLQ Repair — behaviour

Pending edits live per message in memory (lost on quit; the help says so). Edits apply only to the DLQ tab; Active is read-only (needs locks, principle 1).

On `r`:
1. Confirm popup lists: source `entity/$DLQ seq N` → target, body (unchanged / edited, N lines), each Property Edit, Subject/ContentType changes, `markers removed: DeadLetterReason, DeadLetterErrorDescription`, and any warnings (§6.1). `[y/n]`.
2. Execute in one call, lock held only here:
   1. Receive from the DLQ in peek-lock, small batches, until the message with that sequence number is found. **Abandon every non-matching message immediately** (BusX bug #196: siblings left locked for a full lock duration). Give up after a bounded scan → **NotFound**.
   2. Verify MessageId matches what the user looked at; else abandon → **NotFound** ("changed since you peeked").
   3. Build the outgoing message: body (edited or original bytes), application properties = original − markers + Property Edits, Subject/ContentType (edited or original), and copy MessageId, CorrelationId, SessionId, PartitionKey, To, ReplyTo, ReplyToSessionId, TimeToLive. Do not copy broker-owned fields.
   4. Send to the target. Failure → abandon → **SendFailed** (nothing changed).
   5. Complete the original. Failure after a successful send → **CleanupPending**: the copy is in the target **and** the original is still in the DLQ. Reported in red with both locations; never reported as success.
   6. Lock lost before send → **LockLost** (nothing changed).
3. Outcome in status bar + log. On **Resubmitted**: the row disappears, the cursor moves to the next message and the main pane shows it, so `r y r y …` works through a queue.

Target: the dead-letter message's source entity. Queue DLQ → that queue. Subscription DLQ → see open question 1.

### 6.1 Guards (Curation, enforced in the service layer, not the UI)

- Repair only from a dead-letter path.
- Target must be a queue or a topic on the same namespace; a topic with zero subscriptions is rejected (the message would be dropped and the original deleted).
- **Duplicate detection (verify first):** if the target has `RequiresDuplicateDetection` and the MessageId is unchanged, the broker may silently drop the copy within the detection window while we complete the original — the message is lost. Confirm the behaviour against the SDK/docs in S2; if real, the confirm popup warns and offers `n` new MessageId (default on).

## 7. Auth and connection

- Default: `AzureCLICredential` (`az login`). Discovery lists namespaces across readable subscriptions; failures per subscription are shown in the log, not fatal.
- `--connection-string <cs>` or `LAZYBUS_CONNECTION_STRING`: one namespace entry from SAS.
- `--emulator`: shortcut for the local Service Bus emulator connection string (`UseDevelopmentEmulator=true`; admin endpoint on port 5300 — verify the Go admin client works against it in S1; if not, entity listing falls back to the emulator's config and counts show `?`).
- `--namespace <fqdn>`: skip discovery, open that namespace with the az credential.
- No secrets written to disk in v0.1.

## 8. Architecture

```
cmd/lazybus/            main, flags
internal/bus/           service layer: Discovery, Entities, Peek, Repair (+ guards, outcomes)
internal/bus/fake/      in-memory fake implementing the same interfaces (tests + `--demo`)
internal/ui/            bubbletea root model, context stack, panels, popups, keymap, styles
internal/ui/testdata/   golden screens
tools/seed/             emulator seeder (go run ./tools/seed)
```

- The UI talks only to interfaces in `internal/bus`; Azure SDK types never leak into `internal/ui`.
- All broker calls run as `tea.Cmd` and return messages; the UI never blocks. Spinners/“loading…” in panel titles.
- One **context stack**: side panels, main pane, popups, `?` menu, filter, `:` jump. Top context owns keys; `esc` pops.
- `--demo` runs against the fake backend with seeded data (lets anyone try it, and powers golden tests).

## 9. Quality gate

- `.githooks/pre-commit`: `gofmt -l` (fail on diff), `go vet ./...`.
- `.githooks/pre-push` (main + tags): `go test ./...` (unit + goldens), `staticcheck` if installed, `go build ./...`. Emulator E2E excluded (`go test -tags emulator ./...` by hand; documented in README).
- `scripts/install-hooks.sh` sets `core.hooksPath`.
- Golden screens at 120×30 for every state listed in the slice acceptance; `go test ./internal/ui -update` regenerates. Inspect changed goldens before committing.
- After each slice: render all goldens into one HTML page under `~/artifacts/scratch/<date>-lazybus-<slice>/index.html` (same style as the BusX slice 1 gallery) and send it with `to-macbook`.
- Conventional Commits, direct to main behind the gate.

## 10. Slices

Each slice ends green on the gate, with goldens inspected and the gallery sent.

- **S0 — skeleton.** Repo hygiene (README stub, LICENSE-MIT + LICENSE-APACHE, `.gitignore`, hooks, `CONTEXT.md` and ADRs already present). Root model, context stack, three side panels + main pane + log + options bar, focus keys, `?` menu, `q`, `--demo` with fake data. Goldens: initial, each panel focused, `?` open, 80×24.
- **S1 — browse.** Auth (az CLI, connection string, emulator, `--namespace`), discovery, entities with DLQ counts (admin runtime-properties listing, one paged call per kind where the SDK allows), peek DLQ/Active with paging, Body/Properties/System tabs, `/` filter, `:` jump, `R`, `s` sort, `y`/`Y` OSC 52, loading and error states. `tools/seed` puts JSON + non-JSON messages with typed application properties into DLQs of a queue and a subscription on the emulator. Emulator E2E: discover → peek DLQ → correct properties shown.
- **S2 — resubmit.** DLQ Repair with zero edits: confirm popup, the §6 algorithm, all outcomes (Resubmitted, NotFound, LockLost, SendFailed, CleanupPending), jump-to-next, guards incl. the duplicate-detection check, `--read-only`. Unit tests on the fake for every outcome; emulator E2E: seed → resubmit → DLQ count −1, target got the message without markers.
- **S3 — edits.** Property Edit popup with types and validation, add/remove, Subject/ContentType, body via `$EDITOR` (`tea.ExecProcess`; for a JSON content type, invalid JSON is kept as the pending edit but flagged in the Body tab and the confirm popup, and the user can re-open with `E`), pending-edit markers, confirm popup diff, `x`. Emulator E2E: edit prop + body → target message carries the edits.
- **S4 — release prep.** README (what/why, install, keys, safety model, screenshot or VHS GIF), `goreleaser` config for linux/darwin amd64/arm64, `go install` path works, `CHANGELOG.md`. **Stop before tagging or publishing a release; Samuel approves v0.1.0.**

## 11. Out of scope for v0.1

Send/compose, schedule, import/export, purge, delete, bulk/multi-select, sessions-aware receive, receive mode / any held lock, Windows, saved config/keybinding remap, themes, transactions (Go SDK lacks them; revisit if it adds them or CleanupPending shows up in practice), device-code auth.

## 12. Open questions (ask Samuel in one round, with a recommendation each, before S0)

1. **Subscription DLQ target.** Service Bus cannot send to one subscription; resubmitting to the topic fans out to every subscription whose filter matches. Options: (a) send to the topic and show “fans out to N subscriptions” in the confirm, (b) refuse subscription-DLQ repair in v0.1, (c) send to the topic with an application property the subscription filter can match (needs a rule — out of scope). Recommendation: (a).
2. **Discovery scope.** All subscriptions from `az account list`, or only the current `az` subscription with a key to switch? Recommendation: all, loaded lazily per subscription.
3. **Peek page size and refresh.** 50 per page and manual `R` only, or auto-refresh counts every N seconds? Recommendation: 50, manual.
4. **Name/brand check.** GitHub user `lazybus` exists (empty). Keep `samuelstrom93/lazybus` and binary `lazybus`? Recommendation: yes.

## 13. Carried-over knowledge from BusX

- DLQ Repair semantics and outcome names (BusX ADR 0028.1, 0031, `CONTEXT.md`).
- No held locks (BusX ADR 0041).
- Resubmit Target rules incl. rejecting topics without subscriptions (BusX ADR 0040).
- By-sequence search must abandon siblings at once (BusX #196).
- On-call flows from Samuel: peek the first DLQ message, sometimes Active; resubmit easily; edit application properties (most often) or message properties.
