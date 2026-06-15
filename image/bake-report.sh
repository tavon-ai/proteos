#!/usr/bin/env bash
# bake-report.sh — turn a baked image's manifest.lock into a human report, and
# (with --write) stamp the captured image size + build time + resolved provider
# versions into image/PROVIDERS.md. Keeps the "how big / how long / which
# versions" facts scripted instead of hand-copied (Phase 6 task 6.0/6.3).
#
# build-rootfs.sh records image_size_mib + build_seconds + the resolved *_version
# pins into manifest.lock on every bake; this script just formats them.
#
# Usage:
#   image/bake-report.sh [--manifest PATH] [--write]
#     --manifest PATH  manifest.lock to read (default: first of
#                      /var/lib/proteos/images/manifest.lock, then <repo>/image/manifest.lock)
#     --write          update the "## Image size" section of image/PROVIDERS.md
#                      in place (otherwise the report is printed to stdout)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROVIDERS_MD="$SCRIPT_DIR/PROVIDERS.md"

MANIFEST=""
WRITE=0
while [[ $# -gt 0 ]]; do
  case "$1" in
    --manifest) MANIFEST=$2; shift 2 ;;
    --write) WRITE=1; shift ;;
    -h | --help) sed -n '2,18p' "$0"; exit 0 ;;
    *) echo "unknown arg: $1" >&2; exit 1 ;;
  esac
done

if [[ -z $MANIFEST ]]; then
  for c in /var/lib/proteos/images/manifest.lock "$SCRIPT_DIR/manifest.lock"; do
    [[ -f $c ]] && { MANIFEST=$c; break; }
  done
fi
[[ -f $MANIFEST ]] || { echo "no manifest.lock found (pass --manifest PATH)" >&2; exit 1; }

# field KEY — read "key = value" from the manifest (trims surrounding spaces).
field() { awk -F= -v k="$1" '$1 ~ "^[[:space:]]*"k"[[:space:]]*$" {sub(/^[[:space:]]*/,"",$2); sub(/[[:space:]]*$/,"",$2); print $2}' "$MANIFEST"; }

SIZE_MIB="$(field image_size_mib)"
SECS="$(field build_seconds)"
[[ -n $SIZE_MIB && $SIZE_MIB != "(unbuilt)" ]] || { echo "manifest has no image_size_mib — re-bake with the current build-rootfs.sh" >&2; exit 1; }

# Pretty size: GiB with one decimal when ≥1024 MiB, else MiB.
human_size() {
  if [[ $SIZE_MIB -ge 1024 ]]; then awk -v m="$SIZE_MIB" 'BEGIN{printf "%.1f GiB (%d MiB)", m/1024, m}'; else echo "${SIZE_MIB} MiB"; fi
}
# Pretty duration: "Nm Ss".
human_time() { printf '%dm %ds' $((SECS / 60)) $((SECS % 60)); }

REPORT="$(cat <<EOF
| field | value |
|---|---|
| image | $(field image) |
| image size | $(human_size) |
| build time | $(human_time) |
| base rootfs | $(field base_rootfs) |
| guest agent | $(field guestagent) |
| features | $(field features) |
| claude | $(field claude_version) |
| node | $(field node_version) |
| gemini | $(field gemini_version) |
| codex | $(field codex_version) |
| pi | $(field pi_version) |
| built at | $(field built_at) |
EOF
)"

if [[ $WRITE -eq 0 ]]; then
  echo "$REPORT"
  exit 0
fi

[[ -f $PROVIDERS_MD ]] || { echo "not found: $PROVIDERS_MD" >&2; exit 1; }

# Replace the body of the "## Image size" section (up to the next "## ") with the
# captured numbers + report table, leaving the rest of PROVIDERS.md untouched.
NEW_SECTION="$(cat <<EOF
## Image size

Recorded by \`image/bake-report.sh\` from \`manifest.lock\`
(image size **$(human_size)**, build time **$(human_time)**):

$REPORT
EOF
)"

tmp="$(mktemp)"
replf="$(mktemp)"
printf '%s\n' "$NEW_SECTION" >"$replf"
# Emit the replacement from a file (not awk -v) so a multi-line value is portable
# across gawk (bake host) and BSD awk (macOS dev).
awk -v rf="$replf" '
  /^## Image size$/ { while ((getline line < rf) > 0) print line; close(rf); skip=1; next }
  skip && /^## / { skip=0 }
  !skip { print }
' "$PROVIDERS_MD" >"$tmp"
mv "$tmp" "$PROVIDERS_MD"
rm -f "$replf"
echo "updated $PROVIDERS_MD (Image size section)"
