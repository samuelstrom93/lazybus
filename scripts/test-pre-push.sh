#!/usr/bin/env bash
# Checks that .githooks/pre-push skips the full gate only for a push that
# changes nothing but benchmarks/release/. Runs the hook in a scratch repo
# with stub go/staticcheck that record their calls.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

mkdir "$TMP/bin"
for tool in go staticcheck; do
  printf '#!/bin/sh\necho "%s $*" >>"%s/calls"\n' "$tool" "$TMP" >"$TMP/bin/$tool"
  chmod +x "$TMP/bin/$tool"
done
export PATH="$TMP/bin:$PATH"
cp "$ROOT/.githooks/pre-push" "$TMP/pre-push"

cd "$TMP"
git init --quiet repo
cd repo
git config user.name test
git config user.email test@example.com
git config commit.gpgsign false

commit() { # message, then files to write
  local msg="$1" f
  shift
  for f in "$@"; do
    mkdir -p "$(dirname "$f")"
    echo "$msg" >>"$f"
    git add "$f"
  done
  git commit --quiet -m "$msg"
  git rev-parse HEAD
}

ZERO=0000000000000000000000000000000000000000
BASE="$(commit base main.go)"
SNAP="$(commit snapshot benchmarks/release/bench-v0.1.0.json)"
MIXED="$(commit mixed main.go benchmarks/release/bench-v0.2.0.json)"
git mv main.go benchmarks/release/main.go
git commit --quiet -m rename
RENAME="$(git rev-parse HEAD)"

FAILED=0
check() { # name, want (skip|gate), stdin lines
  local name="$1" want="$2" got
  rm -f "$TMP/calls"
  if ! printf '%s\n' "$3" | "$TMP/pre-push" origin git@example.com:x.git >/dev/null 2>&1; then
    echo "FAIL $name: hook exited non-zero"
    FAILED=1
    return
  fi
  got=skip
  grep -q '^go test ./...$' "$TMP/calls" 2>/dev/null && got=gate
  if [ "$got" = "$want" ]; then
    echo "ok   $name: $got"
  else
    echo "FAIL $name: $got, want $want"
    FAILED=1
  fi
}

check "snapshot-only push" skip "refs/heads/main $SNAP refs/heads/main $BASE"
check "mixed push" gate "refs/heads/main $MIXED refs/heads/main $SNAP"
check "snapshot-only and mixed updates" gate "refs/heads/main $SNAP refs/heads/main $BASE
refs/heads/other $MIXED refs/heads/other $SNAP"
check "rename into benchmarks/release/" gate "refs/heads/main $RENAME refs/heads/main $MIXED"
check "new branch" gate "refs/heads/new $SNAP refs/heads/new $ZERO"
check "tag push" gate "refs/tags/v0.1.0 $SNAP refs/tags/v0.1.0 $ZERO"
check "deletion" gate "(delete) $ZERO refs/heads/old $SNAP"
check "unknown remote sha" gate "refs/heads/main $SNAP refs/heads/main 1111111111111111111111111111111111111111"

exit "$FAILED"
