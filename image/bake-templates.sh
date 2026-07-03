#!/usr/bin/env bash
# bake-templates.sh — bake the standard machine-template image set (Slice 4) by
# invoking build-rootfs.sh once per template, each tagged with --template <id> so
# the images share one out-dir without clobbering (each writes its own
# manifest-<id>.lock). Run on the Firecracker host: same prerequisites as
# build-rootfs.sh (Linux + KVM + loop-mount + working apt + network).
#
# Usage:
#   image/bake-templates.sh --base <pinned-ci.ext4> --out-dir <images dir> \
#     [--templates base,go,node,python,full] \
#     [-- <extra flags forwarded to every build-rootfs.sh, e.g. --claude-bootstrap>]
#
# The platform layer (guest agent, git, vim, taskfile, dev user, code-server, and
# — when forwarded — claude/providers) is build-rootfs.sh's defaults and is
# identical across templates; this script only varies the Go/Node/Python/Rust
# language layers:
#   base   = platform only (Go off)        go     = + Go
#   node   = + Node                        python = + Python (pip/venv/build tools)
#   full   = + Go + Node + Python + Rust
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BUILD="$SCRIPT_DIR/build-rootfs.sh"

log() { printf '\e[1;35m[bake-templates]\e[0m %s\n' "$*"; }
die() {
  printf '\e[1;31m[bake-templates:fail]\e[0m %s\n' "$*" >&2
  exit 1
}

BASE=""
OUT_DIR=""
TEMPLATES="base,go,node,python,full"
PASSTHROUGH=()
while [[ $# -gt 0 ]]; do
  case "$1" in
    --base) BASE=$2; shift 2 ;;
    --out-dir) OUT_DIR=$2; shift 2 ;;
    --templates) TEMPLATES=$2; shift 2 ;;
    --)
      shift
      PASSTHROUGH=("$@")
      break
      ;;
    *) die "unknown arg: $1 (extra build-rootfs.sh flags go after a literal --)" ;;
  esac
done
[[ -n $BASE ]] || die "--base <pinned firecracker-ci ext4> is required"
[[ -f $BASE ]] || die "base rootfs not found: $BASE"
[[ -n $OUT_DIR ]] || die "--out-dir <images dir> is required"
[[ -x $BUILD ]] || die "build-rootfs.sh not found/executable: $BUILD"

# lang_flags <template-id> — echo the language-layer flags for one template. Only
# Go/Node/Python are toggled here; everything else comes from build-rootfs.sh
# defaults (the shared platform layer). base turns the default-on Go off.
lang_flags() {
  case "$1" in
    base) echo "--no-go" ;;
    go) echo "--go" ;;
    node) echo "--no-go --node" ;;
    python) echo "--no-go --python" ;;
    full) echo "--go --node --python --rust" ;;
    *) die "unknown template id: $1 (known: base go node python full)" ;;
  esac
}

IFS=',' read -ra IDS <<<"$TEMPLATES"
log "baking ${#IDS[@]} template(s): ${IDS[*]}"
[[ ${#PASSTHROUGH[@]} -gt 0 ]] && log "common passthrough → build-rootfs.sh: ${PASSTHROUGH[*]}"

BAKED=()
for id in "${IDS[@]}"; do
  id="$(echo "$id" | tr -d '[:space:]')"
  [[ -n $id ]] || continue
  flags="$(lang_flags "$id")"
  log "=== template '$id' ($flags) ==="
  # $flags is intentionally word-split into separate args.
  # shellcheck disable=SC2086
  if [[ ${#PASSTHROUGH[@]} -gt 0 ]]; then
    "$BUILD" --base "$BASE" --out-dir "$OUT_DIR" --template "$id" $flags "${PASSTHROUGH[@]}"
  else
    "$BUILD" --base "$BASE" --out-dir "$OUT_DIR" --template "$id" $flags
  fi
  img="$(awk -F'= *' '/^image /{print $2}' "$OUT_DIR/manifest-${id}.lock" 2>/dev/null || true)"
  BAKED+=("$id → ${img:-<no manifest?>}")
done

log "done — baked ${#BAKED[@]} image(s):"
for b in "${BAKED[@]}"; do log "  $b"; done
log "register these rootfs_ref names in the control plane's PROTEOS_TEMPLATES_FILE"
log "(one entry per template, id = the template id above)."
