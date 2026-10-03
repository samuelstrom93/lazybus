# lazybus v0.1 — spec

A lazygit-style terminal UI for **Azure Service Bus only**. Built for the on-call moment: open the dead-letter queue, look at the first message, fix it, put it back.

Status: ready for the build session. Section 12 answered from ServiceBusExplorer's source; conflicts with §2 confirmed with Samuel. Reviewed by oracle (Fable) 2026-10-03.

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
- Service Bus: `github.com/Azure/azure-sdk-for-go/sdk/messaging/azservicebus` v1.10.0 (data plane) + its `admin` package (entities + runtime counts); `github.com/Azure/go-amqp` v1.4.0 directly for `*amqp.Error` classification.
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
- Must render cleanly at 120×30 and degrade down to 80×24: list rows and box borders truncate with `…` and never wrap; the Body tab **wraps** long lines (it is content, not a box).

Panel contents:
1. **Namespaces** — discovered via ARM for the `az` login (all subscriptions the user can read), plus any `--connection-string`/`--emulator` entry. Shows name; subscription in the main pane when focused.
2. **Entities** — queues and topic subscriptions of the selected namespace (topics themselves hidden; they have no DLQ). Columns: path, DLQ count (`?` when runtime counts are unavailable, e.g. emulator); active count in the main pane. Sorted by path; `s` toggles sort by DLQ count (desc).
3. **Messages** — panel tabs **DLQ │ Active** (`tab` when this panel is focused). Rows: sequence number, enqueued time (local), dead-letter reason (DLQ) or subject (Active), message id. Order = peek order (lowest sequence first). Active tab on a sessionful entity shows "sessionful: active messages need a session lock (not in v0.1)" (plain-receiver peek fails with `amqp:not-allowed`; the DLQ works). Page of 50; the next page loads automatically when the cursor reaches the last row (lazygit commits behaviour).

Main pane tabs:
- **Body** — pretty-printed JSON when it parses, raw text otherwise; scrollable (viewport). Pending body edit shown with a `(edited)` marker.
- **Properties** — application properties: key, type, value. Dead-letter markers (`DeadLetterReason`, `DeadLetterErrorDescription`) shown dimmed with `✕ removed on resubmit`. Pending Property Edits shown inline: `*` changed (with old value), `+` added, `−` removed.
- **System** — MessageId, CorrelationId, Subject ✎, ContentType ✎, SessionId, PartitionKey, To, ReplyTo, TTL, DeliveryCount, EnqueuedTime, SequenceNumber, DeadLetterSource. ✎ = editable in repair.

## 5. Keymap

Rule: same letter = analogous action in every context; lowercase = common action, uppercase = heavier variant. Only the top context of the context stack receives keys, so text fields in popups never trigger commands.

| Scope | Key | Action |
|---|---|---|
| Global | `1` `2` `3` | focus side panel |
| | `0` | focus main pane |
| | `h` `l` / `←` `→` | previous / next panel |
| | `j` `k` / `↓` `↑` | move |
| | `g` `G` | top / bottom |
| | `ctrl-d` `ctrl-u` | half page down / up |
| | `[` `]` | previous / next **main-pane tab** (Body / Properties / System) — works from any side panel (lazydocker convention) |
| | `/` | filter focused list (`esc` clears) |
| | `:` | jump to entity by name (fuzzy, current namespace) |
| | `R` | refresh focused panel |
| | `y` | copy the selected thing: body (Body tab), value (Properties), field (System) — OSC 52 |
| | `Y` | copy MessageId |
| | `?` | searchable keybindings menu |
| | `esc` | back / close popup |
| | `q` | quit (from root context) |
| Namespaces | `enter` | open → focus Entities |
| Entities | `enter` | open → focus Messages |
| | `s` | toggle sort path / DLQ count |
| Messages | `tab` | toggle DLQ / Active |
| | `enter` | focus main pane |
| | `r` | resubmit selected DLQ message (DLQ Repair) — confirm popup |
| | `E` | edit body in `$EDITOR` (`$VISUAL`, then `$EDITOR`, then `vi`) |
| | `e` | add a Property Edit (empty key) |
| | `x` | discard Pending Edits for this message (confirm if any) |
| | `c` | Finish Cleanup on a CleanupPending row (§6) — confirm popup |
| Main pane: Body | `E` | edit body in `$EDITOR` |
| Main pane: Properties | `e` | edit selected property (popup prefilled) |
| | `a` | add property |
| | `d` | mark selected property for removal (toggle) |
| Main pane: System | `e` | edit Subject / ContentType (only those rows) |
| Main pane (any tab) | `r` `x` | same as in Messages |
| Popups | `enter` / `y` | confirm |
| | `esc` / `n` | cancel |
| | `tab` | next field |

Edit and resubmit keys only act on the DLQ tab; on Active they show "read-only: Active messages can't be repaired" in the status bar.

Property Edit popup: fields Key, Type (selector: String, Int, Long, Double, Bool, Guid, DateTime — String default for new keys; existing keys keep their type), Value. Validation inline; invalid value keeps the popup open with the error.

## 6. DLQ Repair — behaviour

Pending edits live per message in memory (lost on quit; the help says so). Edits apply only to the DLQ tab; Active is read-only (needs locks, principle 1).

On `r`:
1. **Pre-check without a lock:** peek 1 message from sequence N on the DLQ. If the returned sequence number ≠ N → **NotFound** ("already gone"), zero messages touched.
2. Confirm popup lists: source `entity/$DLQ seq N` → target, body (unchanged / edited, N lines, invalid-JSON flag), each Property Edit, Subject/ContentType changes, `markers removed: DeadLetterReason, DeadLetterErrorDescription`, MessageId (kept / new, see §6.1), the scan cost ("locks up to K messages ahead of it briefly; their DeliveryCount may increase by 1"), and target warnings (§6.1). `[y/n]`.
3. Execute in one call, lock held only here:
   1. One receiver per DLQ, no prefetch (Go SDK: credits = batch size per `ReceiveMessages`; leftover credits are released by the SDK), kept open across consecutive repairs (a link holds no lock). Receive in peek-lock, batches of 10, until sequence N is found. **Keep the locks on non-matching messages until the target is found or the scan limit is reached, then abandon all of them at once — including the rest of the batch after the match** — await every abandon, log abandon errors (never swallow them) (BusX #196; same as SBE, §12.2). Abandoning before the next receive puts the message back at the head and the scan never advances (S−1 spike). The locks live only for the scan and are released before the call returns. Scan limit K = the row's index in the peeked list + one page, and the scan also stops before `LockedUntil` of the first held message minus the safety margin (10 s, used for every lock check in §6); exceeding either → **NotFound**. Sequence numbers are unique and immutable, so no MessageId check.
   2. Build the outgoing message: body (edited or original bytes), application properties = original − markers + Property Edits, Subject/ContentType (edited or original), MessageId (§6.1), and copy CorrelationId, SessionId, PartitionKey, To, ReplyTo, ReplyToSessionId, TimeToLive. Do not copy broker-owned fields. Strip only the application-property keys `DeadLetterReason` and `DeadLetterErrorDescription` (`DeadLetterSource` is a broker annotation and cannot be set). Copy application property values as-is to keep their types. SessionId must be copied: a sessionful target rejects a message without one (`amqp:not-allowed`, definite).
   3. Check the lock locally (`LockedUntil` of the received message minus a safety margin; never of a peeked one). Expired → abandon → **LockLost** (nothing changed).
   4. Send to the target.
      - Definite rejection → abandon → **SendFailed** (nothing changed). Definite = `*amqp.Error` conditions not-found, not-allowed, unauthorized-access, link:message-size-exceeded, resource-limit-exceeded, com.microsoft:entity-disabled, com.microsoft:server-busy; `azservicebus.Error` codes unauthorized / not found; `ErrMessageTooLarge`. Everything else is ambiguous (S−1 spike).
      - Ambiguous (timeout, context deadline, link drop) → abandon → **SendUncertain**: the copy may or may not exist in the target. Amber, never retried automatically.
   5. Complete the original. Failure after a successful send → **CleanupPending**: the copy is in the target **and** the original is still in the DLQ. Red, with both locations; never reported as success.
4. Outcome in status bar + log. Row handling:
   - **Resubmitted**: row and its Pending Edits removed; cursor moves to the next message and the main pane shows it, so `r y r y …` works through a queue.
   - **NotFound**: row and its Pending Edits removed; cursor stays at that index.
   - **LockLost / SendFailed**: row kept with its Pending Edits; the user can retry.
   - **CleanupPending / SendUncertain**: row kept, marked red/amber with the outcome; Pending Edits kept.
   - On a **SendUncertain** row `r` is allowed again; its confirm popup adds a red line "the previous attempt may have delivered a copy — check the target first". Never retried automatically.
   - On a **CleanupPending** row `r` is blocked (status bar: "copy already in target — press c to finish cleanup"). `c` runs **Finish Cleanup**: pre-check, confirm popup (`entity/$DLQ seq N` will be removed; nothing is sent), the same by-sequence scan as step 3.1, then complete the match. Outcomes: Cleaned (row removed, cursor as Resubmitted), NotFound, LockLost. `--read-only` disables `c`. Added to the keymap for Messages and Main pane.

Target: the entity the user opened, not `DeadLetterSource` (usually empty). Queue DLQ → that queue. Subscription DLQ → its parent topic (§12.1).

### 6.1 Guards (Curation, enforced in the service layer, not the UI)

- Repair only from a dead-letter path.
- Target must be a queue or a topic on the same namespace; a topic with zero subscriptions is rejected (the message would be dropped and the original deleted; verified in S−1: the send returns nil). Count subscriptions with `NewListSubscriptionsPager`, not runtime properties (they fail on the emulator).
- **Duplicate detection:** a message with a MessageId already seen inside the target's detection window is accepted and then discarded (documented Service Bus behaviour; verified in S−1: a same-MessageId repair within the window drops the copy and completes the original, so the message is lost with no error). When the target has `RequiresDuplicateDetection`, the repair uses a **new MessageId** by default and logs old → new; the confirm popup shows it and `m` toggles back to keeping the original id. Detect `RequiresDuplicateDetection` via `admin.GetQueue`/`GetTopic`.
- **Topic targets:** a topic delivers only to subscriptions whose rules match; if none match, the broker drops the copy and the original would still be completed. The confirm popup says so ("delivered only to subscriptions whose rules match; no match = dropped"). Showing the rules is v0.2.

## 7. Auth and connection

- Default: `AzureCLICredential` (`az login`; azure-cli is installed on framen and Samuel logs in once with `az login --use-device-code`). Discovery lists namespaces across readable subscriptions; failures per subscription are shown in the log, not fatal.
- `--connection-string <cs>` or `LAZYBUS_CONNECTION_STRING`: one namespace entry from SAS.
- `--emulator`: shortcut for the local Service Bus emulator connection string (`UseDevelopmentEmulator=true`, `localhost`). Ports: `--emulator-amqp-port` (default 5672) and `--emulator-admin-port` (default 5300); lazybus' own compose in `emulator/` uses 5682 / 5310. The admin client needs an HTTP-only `azcore` transport (the SDK always uses https). Entity listing, properties and rules work; **runtime counts do not** (the emulator omits `CountDetails`; subscription runtime calls panic in SDK v1.10.0). With `--emulator`, never call the runtime-properties APIs; counts show `?`. Wrap all admin calls in `recover()`. `Get*` returns `(nil, nil)` for a missing entity. (S−1 spike.)
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
- After each slice: render all goldens into one HTML page under `~/artifacts/scratch/<date>-lazybus-<slice>/index.html` (same style as the BusX slice 1 gallery) and send it with `to-macbook`; if the MacBook is offline, print the URL and continue (all gallery links go in the final report).
- `staticcheck` is installed on framen and runs in pre-push.
- Conventional Commits, direct to main behind the gate.

## 10. Slices

Each slice ends green on the gate, with goldens inspected and the gallery sent.

- **S−1 — emulator spike (≤ 1 hour, throwaway).** Against the emulator with the Go SDK: peek from sequence on a DLQ, receive + abandon, send, complete, duplicate-detection drop with the same MessageId, DLQ of a sessionful queue with a plain receiver, and the Go admin client on port 5300. Write the findings into `docs/spike-s-1.md`; adjust the spec where an assumption fails. Not merged as code.
- **S0 — skeleton.** Repo hygiene (README stub, LICENSE-MIT + LICENSE-APACHE, `.gitignore`, hooks, go.mod). Root model, Context Stack, three side panels + main pane + log + options bar, focus keys, `[`/`]` tabs, `?` menu, `q`, `--demo` with fake data. Goldens: initial, each panel focused, each main tab, `?` open, 80×24.
- **S1a — on-call browse.** `--connection-string`, `--namespace` (az CLI credential), `--emulator`; entities with DLQ counts; DLQ peek with auto-paging; Body (wrapping) / Properties / System tabs; loading and error states. `tools/seed` puts JSON + non-JSON messages with typed application properties into DLQs of a queue and a subscription on the emulator. `tools/seed` also creates `empty-topic` at runtime via admin (the emulator config cannot hold a topic without subscriptions; runtime entities vanish on restart). Emulator E2E: entity listing with `?` counts; peek DLQ → correct properties shown. E2E timeouts ≥ 30 s per operation; don't loop abandons (emulator throttles).
- **S2 — resubmit.** DLQ Repair with zero edits: pre-check, confirm popup, the §6 algorithm, all outcomes (Resubmitted, NotFound, LockLost, SendFailed, SendUncertain, CleanupPending) with their row handling, jump-to-next, guards incl. duplicate detection and topic warning, `--read-only`. Unit tests on the fake for every outcome; emulator E2E: seed → resubmit → DLQ count −1, target got the message without markers. **After S2 lazybus is usable on-call.**
- **S1b — navigation.** ARM discovery across subscriptions (lazy per subscription; failures to the log), Active tab, `/` filter, `:` jump, `s` sort, `y`/`Y` OSC 52, `R`.
- **S3 — edits.** First: Property Edit popup with types and validation, add/remove, Subject/ContentType, pending-edit markers, confirm diff, `x`. Then: body via `$EDITOR` (`tea.ExecProcess`; for a JSON content type, invalid JSON is kept as the pending edit but flagged in the Body tab and the confirm popup, and `E` re-opens it). Emulator E2E: edit prop + body → target message carries the edits.
- **S4 — release prep.** README (what/why, install, keys, safety model incl. the DeliveryCount cost of the by-sequence scan, screenshot or VHS GIF), `goreleaser` config for linux/darwin amd64/arm64 (GitHub release archives + `go install`; no Homebrew tap in v0.1), checked with `goreleaser check` and `goreleaser release --snapshot --clean` only, `go install` path works, `CHANGELOG.md`, README GIF made with `vhs` against `--demo`. Real-namespace E2E (`go test -tags azure ./...`) against the Standard test namespace `sb-lazybus-test-81abd072.servicebus.windows.net` (subscription `lazybus-dev`, resource group `rg-lazybus-test`, westeurope; Samuel has Azure Service Bus Data Owner on it; kept after v0.1 for integration tests), verifying the S−1 gaps: DLQ-abandon DeliveryCount effect, `DeadLetterSource`, auth errors, runtime counts, partitioned-entity peek order; README and confirm wording follow the result. **Stop before tagging or publishing a release; Samuel approves v0.1.0.**

Order: S−1 → S0 → S1a → S2 → S1b → S3 → S4.

## 11. Out of scope for v0.1

Send/compose, schedule, import/export, purge, delete, bulk/multi-select, sessions-aware receive, receive mode / any held lock, Windows, saved config/keybinding remap, themes, transactions (Go SDK lacks them; revisit if it adds them or CleanupPending shows up in practice), device-code auth.

## 12. Open questions — answered from ServiceBusExplorer

Samuel's rule (2026-10-03): do what Paolo Salvatori's ServiceBusExplorer (SBE) does, unless it conflicts with §2. Source read: SBE `master` @ `b07ef0d` (2026-09-29), static reading. All SBE DLQ resubmit paths (queue + subscription, "Repair and Resubmit" and batch "Resubmit") end in `MessageForm.btnSubmit_Click` → `DeadLetterMessageHandler.MoveMessages`; legacy `Microsoft.ServiceBus.Messaging` code only (the `Azure.Messaging.ServiceBus` code has no resubmit).

1. **Subscription DLQ target.** SBE: the user picks a queue or topic in "Select a target Queue or Topic"; for a subscription source the parent topic is preselected and subscriptions are never offered as targets, so the copy fans out to every subscription whose rules match ([MessageForm.cs#L372-L388](https://github.com/paolosalvatori/ServiceBusExplorer/blob/b07ef0d66c0ea193ae690030b7be70f0b9766675/src/ServiceBusExplorer/Forms/MessageForm.cs#L372-L388), [#L628-L632](https://github.com/paolosalvatori/ServiceBusExplorer/blob/b07ef0d66c0ea193ae690030b7be70f0b9766675/src/ServiceBusExplorer/Forms/MessageForm.cs#L628-L632), [SelectEntityForm.cs#L160-L216](https://github.com/paolosalvatori/ServiceBusExplorer/blob/b07ef0d66c0ea193ae690030b7be70f0b9766675/src/ServiceBusExplorer/Forms/SelectEntityForm.cs#L160-L216)). The original is completed only when "Remove message from DLQ" is ticked, which is **off by default** — default is copy-and-leave ([MessageForm.Designer.cs#L421-L429](https://github.com/paolosalvatori/ServiceBusExplorer/blob/b07ef0d66c0ea193ae690030b7be70f0b9766675/src/ServiceBusExplorer/Forms/MessageForm.Designer.cs#L421-L429), [MessageForm.cs#L514-L565](https://github.com/paolosalvatori/ServiceBusExplorer/blob/b07ef0d66c0ea193ae690030b7be70f0b9766675/src/ServiceBusExplorer/Forms/MessageForm.cs#L514-L565)).
   **lazybus:** option (a) — send to the parent topic; confirm shows "fans out to N subscriptions; delivered only where rules match". The original is always completed (DLQ Repair = move); SBE's copy-and-leave default is not adopted (conflicts with §1/§2; Samuel confirmed 2026-10-03).
2. **Scan cost.** SBE: no receive-by-sequence; it receives the DLQ in PeekLock and scans until the sequence number matches, sends, then completes the match (abandon + rethrow on error). Non-matching messages are **held locked until the scan ends** and abandoned together in `finally`; the scan stops on a null receive or at `LockDuration − 3 s`, no message-count limit ([DeadLetterMessageHandler.cs#L188-L296](https://github.com/paolosalvatori/ServiceBusExplorer/blob/b07ef0d66c0ea193ae690030b7be70f0b9766675/src/Common/Helpers/DeadLetterMessageHandler.cs#L188-L296), [#L347-L359](https://github.com/paolosalvatori/ServiceBusExplorer/blob/b07ef0d66c0ea193ae690030b7be70f0b9766675/src/Common/Helpers/DeadLetterMessageHandler.cs#L347-L359)). Every scanned message gets DeliveryCount +1 (Service Bus behaviour on peek-lock receive). No lock is held while the user edits (the grid is peeked).
   **lazybus:** same as SBE — by-sequence PeekLock scan, non-matches held only for the scan and then all abandoned (incl. post-match siblings, BusX #196), plus a scan limit K. The DeliveryCount cost is shown in the confirm popup and README as "may increase DeliveryCount by 1" (the S−1 spike saw +1 on active-queue abandon but no change on emulator DLQ abandon; verify on a real namespace before S4). Correction: Samuel first confirmed "abandon immediately" (2026-10-03), but the S−1 spike showed it never finds the target — an abandoned message returns to the head of the queue. Holding the locks inside the one call is allowed by §2 / ADR 0002.
3. **MessageId.** SBE: keeps the original MessageId by default; "Generate new MessageId" (off by default) sets a new GUID ([ServiceBusHelper.cs#L2427](https://github.com/paolosalvatori/ServiceBusExplorer/blob/b07ef0d66c0ea193ae690030b7be70f0b9766675/src/Common/Helpers/ServiceBusHelper.cs#L2427), [MessageForm.Designer.cs#L411-L420](https://github.com/paolosalvatori/ServiceBusExplorer/blob/b07ef0d66c0ea193ae690030b7be70f0b9766675/src/ServiceBusExplorer/Forms/MessageForm.Designer.cs#L411-L420)). No duplicate-detection handling: with "Remove from DLQ" ticked, a same-id copy dropped by the target's detection window still completes the original, so the message is lost. SBE strips `DeadLetterReason`, `DeadLetterErrorDescription`, `NServiceBus.Transport.Recovery` from edited properties ([Constants.cs#L91](https://github.com/paolosalvatori/ServiceBusExplorer/blob/b07ef0d66c0ea193ae690030b7be70f0b9766675/src/Common/Helpers/Constants.cs#L91)); copies Label, ContentType, CorrelationId, SessionId, To, ReplyTo, ReplyToSessionId, TimeToLive and all application properties ([ServiceBusHelper.cs#L2407-L2469](https://github.com/paolosalvatori/ServiceBusExplorer/blob/b07ef0d66c0ea193ae690030b7be70f0b9766675/src/Common/Helpers/ServiceBusHelper.cs#L2407-L2469)).
   **lazybus:** keep the original MessageId by default, as SBE; `m` toggles a new one. Exception (Samuel confirmed 2026-10-03): on a target with `RequiresDuplicateDetection`, default to a new MessageId (§6.1), because keeping it can silently drop the copy and then delete the original.

Decided without asking: `--namespace` first and ARM discovery of all subscriptions in S1b; page size 50 with manual refresh; repo `samuelstrom93/lazybus`, binary `lazybus`.

## 13. Carried-over knowledge from BusX

- DLQ Repair semantics and outcome names (BusX ADR 0028.1, 0031, `CONTEXT.md`).
- No held locks (BusX ADR 0041).
- Resubmit Target rules incl. rejecting topics without subscriptions (BusX ADR 0040).
- By-sequence search must abandon siblings at once (BusX #196).
- On-call flows from Samuel: peek the first DLQ message, sometimes Active; resubmit easily; edit application properties (most often) or message properties.
