# Changelog

All notable changes to lazybus are listed here. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow [Semantic Versioning](https://semver.org/).

## [Unreleased]

The first release, planned as 0.1.0.

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
- Emulator end-to-end tests (`go test -tags emulator ./...`) and real-namespace end-to-end tests (`go test -tags azure ./...`).

[Unreleased]: https://github.com/samuelstrom93/lazybus/commits/main
