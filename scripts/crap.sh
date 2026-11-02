#!/usr/bin/env bash
#
# crap.sh: CRAP report (on demand, no threshold, never in the gate).
# See docs/crap-report.md.
#
#   ./scripts/crap.sh            # top 15
#   ./scripts/crap.sh --top 40
#
# CRAP = cx² × (1 − cov)³ + cx, per function: cx = cyclomatic complexity (gocyclo, the
# same algorithm as the golangci-lint rule), cov = statement coverage from the gate's
# suite (go test ./..., with -coverpkg so cross-package tests count). Emulator/azure
# E2E tests are not run; generated and build-tagged files are excluded.
# Output: .tmp/crap/ (gitignored).
#
set -euo pipefail

GOCYCLO_VERSION="v0.6.0"
ROOT="$(git rev-parse --show-toplevel)"
OUT_DIR="$ROOT/.tmp/crap"

top=15
while [ "$#" -gt 0 ]; do
  case "$1" in
    --top)
      [ "$#" -gt 1 ] && [[ "$2" =~ ^[1-9][0-9]*$ ]] || { echo "--top needs a positive number" >&2; exit 2; }
      top="$2"; shift
      ;;
    -h|--help) sed -n '3,13p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
  shift
done

mkdir -p "$OUT_DIR"
cd "$ROOT"
module="$(go list -m)"

# Test files, generated code and files behind a build tag (the gate never compiles those).
ignore='_test\.go$'
while IFS= read -r f; do
  f="${f#./}"
  ignore+="|^${f//./\\.}\$"
done < <(find . -name '*.go' -not -name '*_test.go' -not -path './.*' \
  -exec grep -lE '^// Code generated .* DO NOT EDIT\.$|^//go:build ' {} +)

echo "crap: running go test ./... with coverage…" >&2
go test -count=1 -coverpkg=./... -coverprofile="$OUT_DIR/cover.out" ./... >"$OUT_DIR/test.log" 2>&1 || {
  cat "$OUT_DIR/test.log" >&2
  echo "crap: tests failed — fix them first" >&2
  exit 1
}
# "<module>/<file>:<line>:\t<func>\t<pct>%" — the line of the func keyword.
go tool cover -func="$OUT_DIR/cover.out" >"$OUT_DIR/cover-func.txt"
# "<cx> <pkg> <func> <file>:<line>:<col>" — same line.
go run "github.com/fzipp/gocyclo/cmd/gocyclo@$GOCYCLO_VERSION" -ignore "$ignore" . >"$OUT_DIR/gocyclo.txt"

awk -v module="$module/" '
  FNR == NR {
    if ($1 == "total:") next
    split($1, loc, ":"); file = loc[1]; sub("^" module, "", file)
    pct = $NF; sub("%", "", pct)
    cov[file ":" loc[2]] = pct / 100
    next
  }
  {
    split($4, loc, ":"); key = loc[1] ":" loc[2]
    if (!(key in cov)) { missing++; next }
    c = cov[key]; cx = $1
    crap = cx * cx * (1 - c) ^ 3 + cx
    dir = loc[1]; sub("/[^/]*$", "", dir)
    printf "%.1f\t%d\t%.0f%%\t%s.%s\t%s:%s\n", crap, cx, c * 100, dir, $3, loc[1], loc[2]
  }
  END { if (missing) printf "crap: %d function(s) without coverage data skipped\n", missing > "/dev/stderr" }
' "$OUT_DIR/cover-func.txt" "$OUT_DIR/gocyclo.txt" | sort -t$'\t' -k1,1 -rn >"$OUT_DIR/crap.tsv"

total="$(wc -l <"$OUT_DIR/crap.tsv" | tr -d ' ')"
printf '\nCRAP top %s of %s functions (no threshold — a where-to-test list)\n\n' "$top" "$total"
{
  printf 'CRAP\tcx\tcov\tfunction\tlocation\n'
  head -n "$top" "$OUT_DIR/crap.tsv"
} | column -t -s$'\t'
printf '\nfull list: %s\n' "$OUT_DIR/crap.tsv"
