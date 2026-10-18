# Changelog

All notable changes to lazybus are listed here. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow [Semantic Versioning](https://semver.org/).

## [Unreleased]

### Added

- `ö` and `ä` switch main-pane tabs like `[` and `]`, for Swedish keyboard layouts where those need AltGr.

### Fixed

- `l` and `→` on Namespaces and Entities open the selected row like `Enter` when it is not the one already open, instead of showing the previous row's content.
- The resubmit confirm popup always shows the MessageId line and the target warnings, also when long edits fill it.
- A refresh that was in flight during a resubmit no longer brings the resubmitted message back into the list.
- "quit after the call finishes" shows while the call runs, not only after it.
- `y` on the Body tab copies the edited body when there is one.
- `R` on Messages keeps the cursor row when it is past the first page.
- `e` and `enter` on a property with a null value no longer turn it into the string `"null"`.
- An unchanged body with CRLF line endings is no longer counted as a Pending Edit.
- The edit popup keeps the cursor visible on lines with wide characters.
- The body's temporary file is removed when lazybus exits while the editor is open.
- A resubmit refuses a message whose AMQP correlation-id is not a string (uuid, ulong or binary), instead of sending the copy without it.
- A resubmit sends the copy with SDK retries off, so `SendFailed (nothing changed)` no longer hides an earlier attempt that may have delivered a copy. The SDK still makes one quick link-recovery retry after a link error: in the rare case where the link detaches after the broker accepted the copy, a SendFailed can still follow a delivered copy. A failure while the send link attaches or authorizes shows as SendUncertain, although nothing was sent.
- The command log shows each failed abandon once, instead of repeating earlier ones after the send.
- The entity-listing hint for an HTTP 401/403 says the credential lacks Manage rights or is invalid, since Azure answers both the same way; before, a wrong key was reported as a missing Manage right only.
- The Active tab says "sessionful" only when the entity requires sessions; other `amqp:not-allowed` refusals show the broker's reason.
- After a SIGINT signal (`kill -INT` while the input is not a terminal) that waited for a resubmit or cleanup, lazybus exits 0 when the outcome is Resubmitted, Cleaned or NotFound, without a "program was interrupted" line.
- `--help` says that discovery also runs with `--connection-string` and `--emulator`, which add their namespace on top.

## [0.1.0] - 2026-10-10

The first release.

### Added

- Terminal UI for Azure Service Bus in the lazygit layout: Namespaces, Entities and Messages panels, a main pane with Body, Properties and System tabs, a command log, an options bar with the keys that work right now, and a searchable `?` keybindings menu.
- Namespace discovery from `az login` across every subscription you can read, one subscription at a time; a failing subscription goes to the log and the rest still load. `--namespace` opens one namespace without discovery, `--connection-string` (or `LAZYBUS_CONNECTION_STRING`) opens a namespace by SAS connection string, `--emulator` opens the local Service Bus emulator, `--demo` runs on built-in demo data.
- Queues and topic subscriptions with their dead-letter and active counts (`?` on the emulator, which has no runtime counts); `s` sorts by dead-letter count.
- Dead-letter and active messages by peek, which never locks a message: 50 per page, the next page loads when the cursor reaches the last row. `/` filters a list, `:` jumps to an entity by name, `R` refreshes, `y` and `Y` copy over OSC 52 (also inside tmux and over SSH).
- DLQ Repair (`r`): moves a dead-lettered message back to its queue, or to the parent topic of its subscription, without the dead-letter markers. A pre-check, a confirm popup that defaults to cancel, a by-sequence scan whose locks last only for the call, and distinct outcomes: Resubmitted, NotFound, LockLost, SendFailed, SendUncertain and CleanupPending. Finish Cleanup (`c`) removes the original of a CleanupPending message.
- A SendFailed status shows a short reason (for example `unauthorized: 'Send' claim(s) are required to perform…`); the full error is in the log.
- A SIGTERM (`kill`) while a resubmit or cleanup runs quits only after its outcome is logged, prints that outcome to stderr, and exits 1 when the message did not get through.
- Repair guards: only from a dead-letter queue, only to an existing queue or topic, never to a topic without subscriptions; a new MessageId by default when the target has duplicate detection (`m` toggles); messages whose AMQP body or message-id the copy can't carry are refused.
- Pending Edits before a resubmit: typed application properties (String, Int, Long, Double, Bool, Guid, DateTime; add, change, remove), Subject, ContentType, and the body in `$VISUAL` or `$EDITOR`. `x` discards them. They live in memory only.
- `--read-only` turns off every key that changes broker state (`r`, `c`).
- `--entity <queue|topic/subscription>` (repeatable) opens those entities without listing the namespace, which needs Manage rights: a Listen and Send credential can peek and resubmit. Without Manage rights a listing error says so, and a resubmit assumes the target has duplicate detection (new MessageId by default; the confirm popup says so).
- The Entities panel and the `:` jump list show the active count next to the dead-letter count.
- `--version` and a `--help` with examples.
- Release archives for Linux and macOS (amd64 and arm64), built with GoReleaser.
- Tag-driven releases: `scripts/release.sh vX.Y.Z` measures the release benchmarks (startup, a 10,000-message DLQ, repair, binary size), compares them with the previous release and tags `origin/main`; the tag runs a GitHub workflow that refuses a material regression and publishes the release with the benchmark report.
- Emulator end-to-end tests (`go test -tags emulator ./...`) and real-namespace end-to-end tests (`go test -tags azure ./...`).

[Unreleased]: https://github.com/samuelstrom93/lazybus/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/samuelstrom93/lazybus/releases/tag/v0.1.0
