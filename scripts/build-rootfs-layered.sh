#!/usr/bin/env bash
# Stage 19: build a LAYERED template's rootfs as a layout-preserving copy-on-write child of a base.
#
# Stage 18 produced a layered child by re-`mkfs.ext4 -d`-ing a fresh `docker export`, which reshuffles the
# ext4 block layout when a RUN layer is added -> ~half the content blocks move -> a huge diff (measured 278 MiB
# for a one-line RUN; see docs/STAGE18_DESIGN.md). This script instead does what E2B does: it **mutates a copy
# of the base's rootfs image in place**, writing only the child's delta, so every unchanged file keeps its block
# and header.BuildDiff stores ~the genuine delta. The edit uses `debugfs` (e2fsprogs) directly on the image
# file -- no mount, no loop device, no root -- keeping build-rootfs.sh's "entirely without root" property.
#
# The delta is "what the child's RUN layers added/changed/removed vs the image they were built FROM": we
# `docker export` both the child image and its FROM image and diff the two trees (rsync -c). The base rootfs
# is the base TEMPLATE's image (e.g. default = FROM-image + the injected daemon/init); since neither docker
# tree contains the daemon/init, they never appear in the delta and the base copy keeps them untouched. This
# requires the child recipe to `FROM` the base template's image (Decision 3 in docs/STAGE19_DESIGN.md).
#
# Usage: scripts/build-rootfs-layered.sh <child_image> <from_image> <base_rootfs.ext4> <output_path>
set -euo pipefail

CHILD_IMAGE="${1:?child docker image}"
FROM_IMAGE="${2:?FROM (base) docker image the child was built on}"
BASE_ROOTFS="${3:?base template rootfs ext4 image to layer over}"
OUT="${4:?output rootfs path}"

command -v debugfs >/dev/null || { echo "missing debugfs (apt install e2fsprogs)" >&2; exit 1; }
command -v rsync   >/dev/null || { echo "missing rsync" >&2; exit 1; }
for img in "$CHILD_IMAGE" "$FROM_IMAGE"; do
  docker image inspect "$img" >/dev/null 2>&1 || { echo "missing image $img" >&2; exit 1; }
done
test -f "$BASE_ROOTFS" || { echo "missing base rootfs $BASE_ROOTFS" >&2; exit 1; }

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
FROM_TREE="$WORK/from" CHILD_TREE="$WORK/child" CMDS="$WORK/debugfs.cmds"
mkdir -p "$FROM_TREE" "$CHILD_TREE"

# 1) Start from a byte-for-byte copy of the base rootfs: identical layout, so an unchanged file costs zero
#    diff. --sparse=always keeps the image's holes sparse on disk.
echo "[layered] copying base rootfs $BASE_ROOTFS -> $OUT ..."
mkdir -p "$(dirname "$OUT")"
cp --sparse=always "$BASE_ROOTFS" "$OUT"

# 2) Export both image filesystems (a created container is enough -- no run needed; `docker diff` is the wrong
#    tool here, it reports a container's *runtime* writes, not the image's build-time layers).
export_image() {
  local image="$1" dest="$2" cid
  cid="$(docker create "$image")"
  docker export "$cid" | tar -x -C "$dest" --exclude='dev/*' 2>/dev/null || true
  docker rm "$cid" >/dev/null
}
echo "[layered] exporting $FROM_IMAGE and $CHILD_IMAGE ..."
export_image "$FROM_IMAGE" "$FROM_TREE"
export_image "$CHILD_IMAGE" "$CHILD_TREE"

# 3) Diff the child tree against the FROM tree (content checksum) and turn each change into a debugfs command.
#    rsync -rcni --delete CHILD/ FROM/ itemizes how FROM would have to change to become CHILD -- exactly the
#    delta to apply onto the base copy. We decide the op from the child's file type + whether the path already
#    exists in FROM (so a new file is `write`, a changed file is `rm` then `write`). Pure attribute-only changes
#    (leading '.') are skipped -- a documented residual (see docs/STAGE19_DESIGN.md §7); paths with spaces too.
echo "[layered] computing delta (rsync -c) ..."
: > "$CMDS"
while IFS= read -r line; do
  case "$line" in
    "*deleting "*)
      # rsync pads "*deleting" with spaces before the path; `read` splits off the path cleanly
      # (leading whitespace stripped, so the debugfs path has no stray leading spaces).
      read -r _ p <<<"$line"
      case "$p" in
        */) printf 'rmdir /%s\n' "${p%/}" >> "$CMDS" ;;
        *)  printf 'rm /%s\n'    "$p"      >> "$CMDS" ;;
      esac
      continue ;;
  esac
  code="${line%% *}"; path="${line#* }"
  [ "$code" = "$line" ] && continue          # no space -> not an itemized line, skip
  case "${code:0:1}" in '>'|c) ;; *) continue ;; esac   # only transfers/creations, not attr-only ('.')
  # Note: each guard is a full `if` (not `[ cond ] && cmd`): under `set -e` a false `&&` test would abort
  # the whole loop. A changed path (already in FROM) is `rm`'d before re-creating; a new one is created directly.
  case "${code:1:1}" in
    f) if [ -e "$FROM_TREE/$path" ]; then printf 'rm /%s\n' "$path" >> "$CMDS"; fi
       printf 'write %s/%s /%s\n' "$CHILD_TREE" "$path" "$path" >> "$CMDS" ;;
    d) if [ ! -d "$FROM_TREE/$path" ]; then printf 'mkdir /%s\n' "${path%/}" >> "$CMDS"; fi ;;
    L) # rsync itemizes a symlink as "linkpath -> target"; take both straight from the line (no readlink).
       lp="${path%% -> *}"; tgt="${path#* -> }"
       if [ -e "$FROM_TREE/$lp" ] || [ -L "$FROM_TREE/$lp" ]; then printf 'rm /%s\n' "$lp" >> "$CMDS"; fi
       printf 'symlink /%s %s\n' "$lp" "$tgt" >> "$CMDS" ;;
  esac
done < <(rsync -rlcni --delete "$CHILD_TREE/" "$FROM_TREE/" 2>/dev/null) # -l so symlinks are itemized, not skipped

ops=$(wc -l < "$CMDS")
echo "[layered] applying $ops debugfs op(s) in place (no mount, without root) ..."
# 4) Apply the whole delta to the copy in one debugfs pass. -f reads the command file; the edit only touches
#    the changed files' blocks + their metadata, so the diff vs the base stays the genuine delta.
if [ "$ops" -gt 0 ]; then
  debugfs -w -f "$CMDS" "$OUT" >/dev/null 2>&1 || debugfs -w -f "$CMDS" "$OUT"
fi

echo "[layered] done: $OUT ($(du -h "$OUT" | cut -f1) on disk, $ops op(s) applied)"
