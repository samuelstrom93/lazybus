# 0001 — Go and Bubble Tea

## Status
Accepted 2026-10-03.

## Decision
lazybus is written in Go with Bubble Tea v2 (+ bubbles, lipgloss, teatest) on the official `azservicebus` SDK, its `admin` package, `azidentity` and the ARM clients.

## Why
- Rust (first wish: fast, memory-light): the official `azure_messaging_servicebus` rewrite is unreleased (`publish = false`, "not production ready", Entra-only, no connection string so no emulator), no Rust crate has an admin client, and the community AMQP crate has been quiet since 2025-07. Both Rust Service Bus TUIs work around this with hand-written REST; one emulates peek with peek-lock + abandon, which bumps DeliveryCount.
- .NET (reuse BusX.Core): the most complete SDK and it has transactions, but Core lives in a private, unlicensed repo, and Terminal.Gui v2 caused friction (obsolete static facade, no cell editing). lazybus is meant to be a standalone public project.
- Go: GA SDK with admin client, mature `az` auth, works with the emulator, single static binary, and the lazy family (lazygit, lazydocker, lazyaz) is Go.
- Bubble Tea over gocui: explicit state model, ready components, `teatest` golden output, `tea.ExecProcess` for `$EDITOR`. Panel focus and popups are built once as a Context Stack — lazygit had to build the same on top of gocui.

## Consequences
- The Go SDK has no transactions, so DLQ Repair is send-then-complete with Resubmit Cleanup Pending as a reported outcome (same semantics BusX ships). Revisit if the SDK adds transactions.
- Repair logic from BusX is re-implemented, not reused.
