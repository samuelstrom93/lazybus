#!/usr/bin/env bash
#
# mutation.sh: mutation testing with gremlins (on demand, never in the gate).
# See docs/mutation-testing.md.
#
#   ./scripts/mutation.sh                        # whole module (except tools/ and internal/seed)
#   ./scripts/mutation.sh --since                # only lines changed vs origin/main (before handoff)
#   ./scripts/mutation.sh --since HEAD~3         # only lines changed vs a ref
#   ./scripts/mutation.sh --pkg internal/bus     # a directory and everything below it; repeatable
#   ./scripts/mutation.sh -- --workers 4         # anything after -- goes straight to gremlins
#
# Output: .tmp/mutation/result.json (gitignored) plus a summary on stdout.
#
set -euo pipefail

GREMLINS_VERSION="v0.6.0"
ROOT="$(git rev-parse --show-toplevel)"
OUT_DIR="$ROOT/.tmp/mutation"
OUT="$OUT_DIR/result.json"

usage() { sed -n '3,12p' "$0" | sed 's/^# \{0,1\}//'; exit "${1:-0}"; }

since=""
pkgs=()
extra=()
while [ "$#" -gt 0 ]; do
  case "$1" in
    --since)
      if [ "$#" -gt 1 ] && [ "${2#-}" = "$2" ]; then since="$2"; shift; else since="origin/main"; fi
      ;;
    --pkg)
      [ "$#" -gt 1 ] || { echo "--pkg needs a directory, e.g. internal/bus" >&2; exit 2; }
      p="${2#./}"; p="${p%/}"; shift
      if [ -z "$p" ] || [ "$p" = "." ] || [ ! -d "$ROOT/$p" ]; then
        echo "--pkg: not a package directory: $p (whole module: omit --pkg)" >&2
        exit 2
      fi
      pkgs+=("$p")
      ;;
    -h|--help) usage 0 ;;
    --) shift; extra=("$@"); break ;;
    *) echo "unknown argument: $1" >&2; usage 2 ;;
  esac
  shift
done

cd "$ROOT"

args=(unleash
  # Dev-only code: tools/* (benchreport, gallery, seed) and the emulator seeder they share
  # with the E2E tests. Neither ships in the lazybus binary.
  --exclude-files '^tools/'
  --exclude-files '^internal/seed/'
  # Default 3x times out under load and hides survivors as TIMED OUT.
  --timeout-coefficient 10
  # Run the whole suite per mutant. Package mode guesses the package from the directory name
  # == Go package name, so `package main` dirs (cmd/lazybus) become the module root, `go test`
  # fails setup and the mutant is falsely counted KILLED. Integration mode also lets the UI
  # tests (which drive the fake transport) kill mutants in internal/bus and internal/bus/fake.
  --integration
  --coverpkg ./...
  --output "$OUT")

# Generated code and files behind a build tag (the emulator/azure E2E suites are all
# _test.go today, which gremlins never mutates; this keeps a future tagged helper out too:
# the gate's `go test ./...` would never run it).
while IFS= read -r f; do
  f="${f#./}"
  args+=(--exclude-files "^${f//./\\.}\$")
done < <(find . -name '*.go' -not -name '*_test.go' -not -path './.*' \
  -exec grep -lE '^// Code generated .* DO NOT EDIT\.$|^//go:build ' {} +)

if [ -n "$since" ]; then
  git rev-parse --verify -q "$since^{commit}" >/dev/null \
    || { echo "--since: unknown ref '$since' (git fetch?)" >&2; exit 2; }
  # Diff from the merge-base, so commits that landed on main after you branched don't count.
  base="$(git merge-base "$since" HEAD)"
  # gremlins treats an empty diff as "everything changed" and runs the full suite.
  # Test files, tools/ and internal/seed are never mutated, so they do not count.
  # Untracked files are invisible to git diff: `git add -N <file>` to include a new file.
  if git diff --quiet "$base" -- '*.go' ':!*_test.go' ':!tools/' ':!internal/seed/'; then
    echo "mutation: no mutable Go changes since $since (merge-base ${base:0:10}) — nothing to mutate."
    exit 0
  fi
  # gremlins runs `git diff <ref>` (against the working tree, so uncommitted edits count too)
  # and matches its paths against module-relative ones. go.mod is at the repo root, so they
  # are the same paths.
  args+=(--diff "$base")
  printf 'mutation: only lines changed since %s (merge-base %s, incl. uncommitted)\n' "$since" "${base:0:10}"
fi

if [ "${#pkgs[@]}" -gt 0 ]; then
  # gremlins' [path] argument rebases file names and shrinks test timeouts, so run on the
  # whole module and exclude every Go file outside the packages instead (Go regexp has no
  # lookahead, hence one exclude per file).
  not_in=()
  for p in "${pkgs[@]}"; do not_in+=(-not -path "./$p/*"); done
  while IFS= read -r f; do
    f="${f#./}"
    args+=(--exclude-files "^${f//./\\.}\$")
  done < <(find . -name '*.go' -not -name '*_test.go' -not -path './.*' "${not_in[@]}")
fi

# gremlins sets each mutant's timeout to 3x the duration of its initial coverage run. A
# cached `go test` makes that run a few ms and every mutant TIMES OUT; -count=1 forces a real run.
export GOFLAGS="${GOFLAGS:+$GOFLAGS }-count=1"

mkdir -p "$OUT_DIR"
rm -f "$OUT"
start=$(date +%s)
# Pinned and fetched by `go run`; the binary is cached in the Go build cache after the first run.
go run "github.com/go-gremlins/gremlins/cmd/gremlins@$GREMLINS_VERSION" "${args[@]}" ${extra[@]+"${extra[@]}"} -S lt || {
  echo "mutation: gremlins failed" >&2
  exit 1
}
elapsed=$(( $(date +%s) - start ))

python3 - "$OUT" "$elapsed" <<'PY'
import json, sys
path, elapsed = sys.argv[1], int(sys.argv[2])
d = json.load(open(path))
lived, uncovered = [], 0
for f in d.get("files", []):
    for m in f["mutations"]:
        if m["status"] == "LIVED":
            lived.append((f["file_name"], m["line"], m["column"], m["type"]))
        elif m["status"] == "NOT COVERED":
            uncovered += 1
print()
print(f"== mutation summary ({elapsed}s wall) ==")
print(f"efficacy (killed / (killed+lived)): {d['test_efficacy']:.1f}%")
print(f"mutant coverage:                    {d['mutations_coverage']:.1f}%")
print(f"killed {d['mutants_killed']}  lived {d['mutants_lived']}  not covered {uncovered}  "
      f"not viable {d['mutants_not_viable']}")
if lived:
    print("\nLIVED (a test ran the mutated line but no assertion failed):")
    for name, line, col, typ in sorted(lived):
        print(f"  {name}:{line}:{col}  {typ}")
print(f"\nfull result: {path}")
PY
