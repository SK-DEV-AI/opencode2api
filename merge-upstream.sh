#!/bin/bash
# merge-upstream.sh — safe upstream release merger for the opencode2api fork.
#
# Policy: auto-merge ONLY when none of our local patch files are touched by
# the incoming upstream range. Anything else -> report + stop, human ports.
#
# Local patch files (fork-owned, from docs/FORK.md + later work):
#   internal/gateway/{gateway,pool,probe,refresh,upstream}.go
#   internal/protocol/{stream,stream_emitter}.go
#   internal/identity/request.go        (shared with upstream: #69/#70 proved
#                                        upstream rewrites this file; every
#                                        upstream touch here needs a human port)
#   internal/telemetry/{http,metrics}.go
#   internal/jsonutil/value.go
#
# Usage:
#   ./merge-upstream.sh                 # check only: fetch + report, change nothing
#   ./merge-upstream.sh --merge         # merge when safe, else report + exit 1
#   ./merge-upstream.sh --merge v1.3.8  # merge a specific tag/branch instead of origin/main
#
# On safe merge: creates backup tag, merges with --no-ff, builds + vets +
# tests gateway+identity, and prints the deploy ritual. Push is NEVER
# automatic — you review `git log -p` first, then push.
set -u

REPO="$(cd "$(dirname "$0")" && pwd)"
cd "$REPO" || exit 1

# Fork-owned files. Upstream touching any of these blocks auto-merge.
# NOTE: internal/identity/request.go is deliberately listed even though
# upstream owns its evolution (#69/#70) — every upstream rewrite there has
# needed a careful human port, never a blind merge.
PATCH_FILES=(
  internal/gateway/gateway.go
  internal/gateway/pool.go
  internal/gateway/probe.go
  internal/gateway/refresh.go
  internal/gateway/upstream.go
  internal/protocol/stream.go
  internal/protocol/stream_emitter.go
  internal/identity/request.go
  internal/telemetry/http.go
  internal/telemetry/metrics.go
  internal/jsonutil/value.go
)

MODE="check"
TARGET="origin/main"
if [ "${1:-}" = "--merge" ]; then
  MODE="merge"
  [ -n "${2:-}" ] && TARGET="$2"
elif [ -n "${1:-}" ]; then
  echo "usage: $0 [--merge [tag|branch]]" >&2
  exit 2
fi

if ! git diff --quiet || [ -n "$(git ls-files --others --exclude-standard)" ]; then
  echo "STOP: working tree dirty (uncommitted or untracked files). Commit or stash first." >&2
  git status --short | head -10 >&2
  exit 1
fi

echo "fetching origin..."
git fetch origin --tags 2>&1 | tail -2

BASE="$(git merge-base HEAD "$TARGET")"
if [ "$BASE" = "$(git rev-parse "$TARGET")" ]; then
  echo "CURRENT: $TARGET is already merged (base == target). Nothing to do."
  exit 0
fi

echo "--- incoming upstream range: $BASE..$TARGET ---"
git log --oneline "$BASE..$TARGET" | head -20
N_INCOMING=$(git rev-list --count "$BASE..$TARGET")
echo "incoming commits: $N_INCOMING"

echo "--- patch-file overlap check ---"
OVERLAP=""
for f in "${PATCH_FILES[@]}"; do
  if git diff --quiet "$BASE" "$TARGET" -- "$f" 2>/dev/null; then
    echo "  clean: $f"
  else
    echo "  TOUCHED: $f"
    OVERLAP="$OVERLAP $f"
  fi
done

if [ -n "$OVERLAP" ]; then
  echo
  echo "BLOCKED: upstream touches fork-owned files:$OVERLAP"
  echo "Human port required — see precedent: 7b458a2 (upstream #70 ported"
  echo "function-for-function into internal/identity/request.go, verified"
  echo "byte-identical, build+vet+gateway tests green)."
  echo "Per-file incoming diffs:"
  for f in $OVERLAP; do
    echo "  git diff $BASE $TARGET -- $f"
  done
  exit 1
fi

echo
echo "SAFE: upstream touches none of the ${#PATCH_FILES[@]} fork-owned files."
if [ "$MODE" = "check" ]; then
  echo "(check mode — no changes made. Re-run with --merge to merge.)"
  exit 0
fi

# --- merge path ---
STAMP="$(date +%Y%m%d)"
BACKUP_TAG="backup-pre-merge-$STAMP"
git tag "$BACKUP_TAG"
echo "backup tag: $BACKUP_TAG"

if ! git merge --no-ff -m "merge: upstream $(git describe --tags "$TARGET" 2>/dev/null || echo "$TARGET") (auto: no patch-file overlap)" "$TARGET"; then
  echo "STOP: merge conflicts — resolve manually, backup tag $BACKUP_TAG exists." >&2
  exit 1
fi

echo "--- gates ---"
go build ./... || { echo "STOP: build failed after merge." >&2; exit 1; }
echo "build OK"
go vet ./internal/identity/ ./internal/gateway/ || { echo "STOP: vet failed after merge." >&2; exit 1; }
echo "vet OK"
go test ./internal/gateway/ || { echo "STOP: gateway tests failed after merge." >&2; exit 1; }
echo "tests OK"

echo
echo "MERGED + GREEN. Review: git log -p $BASE..HEAD"
echo "Then push (manual, never automatic): git push fork <branch>"
echo "Deploy ritual: systemctl --user stop opencode2api && cp <new-binary> ./opencode2api &&"
echo "  systemctl --user start opencode2api && curl -sf http://127.0.0.1:8787/healthz"
echo "Docs: record the merge in docs/FORK.md + AGENT.md (follow the v1.3.7 entry pattern)."
