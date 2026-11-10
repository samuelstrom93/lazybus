#!/usr/bin/env bash
# Optional: switches the standing Service Bus test namespace between Basic
# (the default, queues only, no base fee) and Standard (topics, sessions,
# duplicate detection, forwarding; billed per hour until switched back).
#
# Nothing runs this for you: not go test, CI, the git hooks or the release
# script. For the pre-release full E2E run use scripts/e2e-standard.sh,
# which uses a temporary namespace instead. This is the backup path, for
# longer integration work that needs Standard on the standing namespace.
#
#   ./scripts/test-namespace.sh status
#   ./scripts/test-namespace.sh up   [--dry-run] [--yes] [--from <backup.json>]
#   ./scripts/test-namespace.sh down [--dry-run] [--yes]
#
# up:   SKU to Standard, recreate the demo entities that need it, restore
#       their messages from the newest backup (or --from).
# down: back up the demo messages, delete the Standard-only entities, cap
#       queue TTLs at P14D, SKU to Basic.
# Both print their plan and ask before changing anything (--yes skips the
# question, --dry-run stops after the plan). Details: tools/testns/main.go.
#
# Needs: az (logged in; may update the namespace and has Azure Service Bus
# Data Owner on it), go.
#
# Environment (all optional):
#   LAZYBUS_TESTNS_NAMESPACE       default sb-lazybus-test-81abd072
#   LAZYBUS_TESTNS_SUBSCRIPTION    default lazybus-dev
#   LAZYBUS_TESTNS_RESOURCE_GROUP  default rg-lazybus-test
#   XDG_STATE_HOME                 backups go to $XDG_STATE_HOME/lazybus/testns
#                                  (default ~/.local/state/lazybus/testns)
set -euo pipefail

for tool in az go; do
  command -v "$tool" >/dev/null 2>&1 || { echo "test-namespace: $tool not found" >&2; exit 1; }
done

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

# Built, not `go run`: the tool then runs in the caller's directory, so a
# relative --from path means what it says.
go build -C "$ROOT" -o "$TMP/testns" ./tools/testns
"$TMP/testns" "$@"
