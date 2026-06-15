#!/usr/bin/env bash
# Stage 6: build a named template -- a custom (rootfs, snapshot) image a sandbox can
# boot from. The local equivalent of E2B's `e2b template build`. See docs/STAGE6_DESIGN.md.
#
# Pipeline:
#   1. docker build the recipe templates/<name>/Dockerfile into an image
#      (templates usually `FROM microsandbox-agent`, so build that base first:
#       docker build -t microsandbox-agent .);
#   2. export it to vendor/templates/<name>/rootfs.ext4   (reusing build-rootfs.sh);
#   3. unless --no-snapshot, build a warm snapshot into vendor/templates/<name>/snapshot
#      (reusing build-snapshot.sh) for millisecond from_snapshot starts.
#
# Recipes live in templates/<name>/ (source, in git); built artifacts land under
# vendor/templates/<name>/ (regenerable, gitignored like every vendor/ artifact).
#
# Usage: scripts/build-template.sh <name> [dockerfile] [--no-snapshot]
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

NAME="${1:-}"
[ -n "$NAME" ] || { echo "usage: scripts/build-template.sh <name> [dockerfile] [--no-snapshot]" >&2; exit 1; }
# Mirror the control plane's name rule (control-plane/template.go), so a name that
# builds is also a name that resolves at runtime.
echo "$NAME" | grep -Eq '^[a-z0-9][a-z0-9_-]{0,63}$' || {
  echo "invalid template name '$NAME': must match [a-z0-9][a-z0-9_-]* (max 64 chars)" >&2; exit 1; }
shift

DOCKERFILE="$REPO_ROOT/templates/$NAME/Dockerfile"
WANT_SNAPSHOT=1
for arg in "$@"; do
  case "$arg" in
    --no-snapshot) WANT_SNAPSHOT=0 ;;
    -*) echo "unknown flag: $arg" >&2; exit 1 ;;
    *) DOCKERFILE="$arg" ;;
  esac
done

[ -f "$DOCKERFILE" ] || { echo "no Dockerfile at $DOCKERFILE (create templates/$NAME/Dockerfile)" >&2; exit 1; }

IMAGE="microsandbox-tmpl-$NAME"
OUT_DIR="$REPO_ROOT/vendor/templates/$NAME"
mkdir -p "$OUT_DIR"

echo "[build-template] docker build $IMAGE from $DOCKERFILE ..."
docker build -f "$DOCKERFILE" -t "$IMAGE" "$REPO_ROOT"

echo "[build-template] exporting rootfs -> $OUT_DIR/rootfs.ext4 ..."
"$REPO_ROOT/scripts/build-rootfs.sh" "$IMAGE" "$OUT_DIR/rootfs.ext4"

if [ "$WANT_SNAPSHOT" = 1 ]; then
  echo "[build-template] building snapshot -> $OUT_DIR/snapshot ..."
  "$REPO_ROOT/scripts/build-snapshot.sh" "$OUT_DIR/rootfs.ext4" "$OUT_DIR/snapshot"
else
  echo "[build-template] skipping snapshot (--no-snapshot); from_snapshot won't work for '$NAME' until built"
fi

echo "[build-template] done: template '$NAME' at $OUT_DIR"
echo "                 use it with  Sandbox(template=\"$NAME\")  (after Stage 6b)"
