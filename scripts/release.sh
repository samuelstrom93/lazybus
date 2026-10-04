#!/usr/bin/env bash
#
# Cut a release. Releases are tag-gated: pushing an annotated `vX.Y.Z` tag
# whose commit is on origin/main runs .github/workflows/release.yml (tests,
# benchmark gate, GoReleaser, GitHub release). Merging to main ships nothing.
#
# Before tagging, the release benchmarks are measured in a temporary worktree
# of the release commit and compared with the previous stable release
# (docs/releasing.md). A new snapshot (benchmarks/release/bench-vX.Y.Z.json)
# is committed and pushed to main, and the tag goes on that commit. A snapshot
# already committed for the version is reused instead of measuring again.
#
# Usage:
#   ./scripts/release.sh v1.2.3                           # release origin/main HEAD
#   ./scripts/release.sh v1.2.3 --dry-run                 # measure + compare, commit/push/tag nothing
#   ./scripts/release.sh v1.2.3 --dry-run --target <rev>  # same for another commit, e.g. to test
#                                                         # this script before it lands on main
#
set -uo pipefail

cd "$(git rev-parse --show-toplevel)" || exit 1

RED=$'\033[0;31m'
GREEN=$'\033[0;32m'
YELLOW=$'\033[0;33m'
BOLD=$'\033[1m'
RESET=$'\033[0m'

USAGE="./scripts/release.sh vX.Y.Z [--dry-run [--target <rev>]]"

fail() {
  printf '%s✗ %s%s\n' "$RED" "$1" "$RESET" >&2
  exit 1
}

step() {
  printf '%s==> %s%s\n' "$BOLD" "$1" "$RESET"
}

VERSION="${1:-}"
DRY_RUN=0
TARGET_REV=""

[ -n "$VERSION" ] || fail "Usage: $USAGE"
shift

# A mistyped flag (--dryrun) must not silently become a real release.
while [ $# -gt 0 ]; do
  case "$1" in
    --dry-run) DRY_RUN=1 ;;
    --target)
      [ $# -ge 2 ] || fail "--target needs a revision (usage: $USAGE)"
      TARGET_REV="$2"
      shift
      ;;
    *) fail "Unknown argument: $1 (usage: $USAGE)" ;;
  esac
  shift
done

# A release is always the tip of origin/main; another commit can only be
# measured, never tagged.
[ -z "$TARGET_REV" ] || [ "$DRY_RUN" = "1" ] ||
  fail "--target is only allowed with --dry-run"

# The release workflow triggers on `v*`; keeping the local check stricter
# (semver) stops a typo like `v1.2` from becoming a real release.
[[ "$VERSION" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]] ||
  fail "Version must look like v1.2.3 (optionally v1.2.3-rc.1), got: $VERSION"

step "Fetching origin"
git fetch origin main --tags --quiet || fail "git fetch failed"

git rev-parse --verify --quiet "refs/tags/$VERSION" >/dev/null &&
  fail "Tag $VERSION already exists locally"

# Exit codes matter here: 0 = tag exists, 2 = --exit-code's "no match", anything
# else is a real failure (network, auth). Treating those as "no match" would
# hide the error and only surface as a rejected push later.
git ls-remote --exit-code --tags origin "refs/tags/$VERSION" >/dev/null
LS_REMOTE_STATUS=$?
case "$LS_REMOTE_STATUS" in
  0) fail "Tag $VERSION already exists on origin" ;;
  2) ;;
  *) fail "git ls-remote failed (exit $LS_REMOTE_STATUS) — cannot verify the tag is free" ;;
esac

if [ -n "$TARGET_REV" ]; then
  TARGET="$(git rev-parse --verify --quiet "$TARGET_REV^{commit}")" ||
    fail "Unknown revision: $TARGET_REV"
  step "Dry run of $TARGET_REV @ ${TARGET:0:9} (not origin/main)"
else
  # Always release the tip of origin/main: this is what makes the workflow's
  # ancestor guard impossible to trip from here.
  TARGET="$(git rev-parse origin/main)"
  step "Releasing origin/main @ ${TARGET:0:9}"
fi
git --no-pager log -1 --format='    %h %s (%an, %ar)' "$TARGET"

PREV_TAG="$(git tag --sort=-v:refname --list 'v*' | head -1)"
if [ -n "$PREV_TAG" ]; then
  printf '\n%sChanges since %s:%s\n' "$BOLD" "$PREV_TAG" "$RESET"
  git --no-pager log --format='    %h %s' "$PREV_TAG..$TARGET"
  COUNT="$(git rev-list --count "$PREV_TAG..$TARGET")"
  [ "$COUNT" -gt 0 ] || fail "No commits since $PREV_TAG — nothing to release"
else
  printf '\n%sFirst release — no previous v* tag.%s\n' "$YELLOW" "$RESET"
fi

# Benchmarks run in a throwaway worktree of exactly the release commit, so
# local changes in this checkout can't leak into the measurement.
WT_PARENT="$(mktemp -d "${TMPDIR:-/tmp}/lazybus-release.XXXXXX")" || fail "mktemp failed"
WT="$WT_PARENT/lazybus"
cleanup() {
  git worktree remove --force "$WT" >/dev/null 2>&1
  rm -rf "$WT_PARENT"
  git worktree prune
}
trap cleanup EXIT
trap 'exit 130' INT TERM

SNAPSHOT="benchmarks/release/bench-$VERSION.json"
printf '\n'
step "Checking out ${TARGET:0:9} in a temporary worktree"
git worktree add --quiet --detach "$WT" "$TARGET" || fail "git worktree add failed"
[ -d "$WT/tools/benchreport" ] ||
  fail "${TARGET:0:9} has no tools/benchreport — the release tooling must be on the release commit"

NEW_SNAPSHOT=0
if [ -f "$WT/$SNAPSHOT" ]; then
  # The snapshot may predate TARGET (an accepted regression committed
  # earlier): show which commit it measured.
  MEASURED="$(sed -n 's/^  "gitSha": "\(.*\)",$/\1/p' "$WT/$SNAPSHOT")"
  printf '    Reusing %s committed at %s, measured at %s (not measuring again).\n' \
    "$SNAPSHOT" "${TARGET:0:9}" "${MEASURED:0:9}"
else
  step "Measuring the release benchmarks (a few minutes)"
  (cd "$WT" && go run ./tools/benchreport run -version "$VERSION") ||
    fail "Benchmark run failed — nothing committed or tagged"
  NEW_SNAPSHOT=1
fi

step "Comparing with the previous release"
(cd "$WT" && go run ./tools/benchreport compare -version "$VERSION")
COMPARE_STATUS=$?
if [ "$COMPARE_STATUS" -ne 0 ]; then
  fail "Benchmark gate failed — nothing committed or tagged.
    Fix the regression on main and rerun, or ship it knowingly:
      1. on a clean checkout of main (on framen): go run ./tools/benchreport run -version $VERSION
      2. add {\"name\": \"<metric>\", \"reason\": \"<why>\"} to acceptedRegressions in $SNAPSHOT
      3. commit and push the snapshot to main
      4. rerun ./scripts/release.sh $VERSION — the committed snapshot is reused"
fi

if [ "$DRY_RUN" = "1" ]; then
  printf '\n%sDry run: nothing committed, pushed or tagged.%s\n' "$YELLOW" "$RESET"
  if [ "$NEW_SNAPSHOT" = "1" ]; then
    printf '%sWould commit %s ("chore(release): benchmark snapshot %s") on %s, push it to main, then tag that commit %s and push the tag.%s\n' \
      "$YELLOW" "$SNAPSHOT" "$VERSION" "${TARGET:0:9}" "$VERSION" "$RESET"
  else
    printf '%sWould tag %s at %s and push the tag.%s\n' "$YELLOW" "$VERSION" "${TARGET:0:9}" "$RESET"
  fi
  exit 0
fi

printf '\n%sTag %s and publish the GitHub release? [y/N] %s' "$BOLD" "$VERSION" "$RESET"
read -r CONFIRM
case "$CONFIRM" in
  y | Y | yes | YES) ;;
  *) fail "Aborted" ;;
esac

TAG_TARGET="$TARGET"
if [ "$NEW_SNAPSHOT" = "1" ]; then
  step "Committing $SNAPSHOT"
  git -C "$WT" add "$SNAPSHOT" || fail "git add failed"
  git -C "$WT" commit --quiet -m "chore(release): benchmark snapshot $VERSION" ||
    fail "git commit failed — nothing pushed or tagged"
  TAG_TARGET="$(git -C "$WT" rev-parse HEAD)"

  # A plain fast-forward: if main moved since the fetch the push is
  # rejected, and the new tip has to be measured instead.
  step "Pushing the snapshot commit to main"
  git -C "$WT" push origin HEAD:refs/heads/main ||
    fail "Pushing the snapshot to main failed (did main move?) — nothing tagged. Rerun to release the new tip."
fi

step "Creating annotated tag $VERSION"
git tag -a "$VERSION" "$TAG_TARGET" -m "Release $VERSION" || fail "git tag failed"

# Pushed from the worktree, so the pre-push hook gates the tagged tree.
step "Pushing tag"
if ! git -C "$WT" push origin "refs/tags/$VERSION"; then
  git tag -d "$VERSION" >/dev/null 2>&1
  if [ "$NEW_SNAPSHOT" = "1" ]; then
    fail "git push failed — local tag removed, nothing was released. The snapshot commit is already on main; rerunning reuses it."
  fi
  fail "git push failed — local tag removed, nothing was released"
fi

printf '\n%s✓ %s pushed. Watch the release:%s\n' "$GREEN" "$VERSION" "$RESET"
printf '    gh run list --workflow release.yml --limit 1\n'
printf '    gh run watch\n'
