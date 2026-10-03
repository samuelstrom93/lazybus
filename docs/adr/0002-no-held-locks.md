# 0002 — lazybus never holds a lock outside one operation

## Status
Accepted 2026-10-03. Same rule as BusX ADR 0041.

## Decision
Browsing is peek only. A message lock exists only inside a DLQ Repair call and is completed or abandoned before the call returns. There is no receive mode, no settle UI and no lock renewal.

## Why
lazybus is meant to be safe to point at production during on-call. A TUI that holds locks while a human reads competes with real consumers, increments delivery counts and can push messages into the DLQ by itself.

## Consequences
- Active-queue messages are read-only.
- Finding a dead-letter message by sequence number means receiving in peek-lock and abandoning every non-matching message at once.
