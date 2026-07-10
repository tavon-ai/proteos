#!/usr/bin/env bash
# restore-volumes.sh — restore one or all per-machine LUKS volume containers
# from a snapshot written by backup-volumes.sh. TAV-31.
#
# Restoring the ciphertext is only half the job: each machine's volume KEY
# lives in OpenBao at secret/machines/<machine_id>/volume-key and must match
# what the container was formatted with, or the node-agent's next `ensure`
# will fail to luksOpen it. See docs/disaster-recovery.md.
#
#   sudo ./restore-volumes.sh --latest --machine <machine-id>
#   sudo ./restore-volumes.sh /var/backups/proteos/volumes/20260710T030000Z --all
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

[[ -f "$SCRIPT_DIR/.env" ]] && { set -a; . "$SCRIPT_DIR/.env"; set +a; }
[[ -f "$SCRIPT_DIR/backup.env" ]] && { set -a; . "$SCRIPT_DIR/backup.env"; set +a; }

: "${PROTEOS_AGENT_VOLUMES_DIR:=/var/lib/proteos/volumes}"
: "${PROTEOS_VOLUME_BACKUP_DIR:=/var/backups/proteos/volumes}"
: "${PROTEOS_CRYPTSETUP_BIN:=/usr/sbin/cryptsetup}"

fail() { echo "[fail] $*" >&2; exit 1; }
ok()   { echo "[ ok ] $*"; }

CRYPTSETUP="$PROTEOS_CRYPTSETUP_BIN"
command -v "$CRYPTSETUP" >/dev/null 2>&1 || CRYPTSETUP="cryptsetup"

SNAPSHOT=""
MACHINE=""
ALL=0
FORCE=0
while [[ $# -gt 0 ]]; do
  case "$1" in
    --latest) SNAPSHOT="$PROTEOS_VOLUME_BACKUP_DIR/latest" ;;
    --machine) [[ $# -ge 2 ]] || fail "--machine requires an argument"; MACHINE="$2"; shift ;;
    --all) ALL=1 ;;
    --force) FORCE=1 ;;
    -h|--help)
      echo "usage: $0 (--latest | <snapshot-dir>) (--machine <id> | --all) [--force]"
      exit 0 ;;
    *) SNAPSHOT="$1" ;;
  esac
  shift
done

[[ -n "$SNAPSHOT" ]] || fail "no snapshot given (pass a path or --latest)"
SNAPSHOT="${SNAPSHOT%/}"
[[ -d "$SNAPSHOT" ]] || fail "snapshot not found: $SNAPSHOT"
[[ -n "$MACHINE" || "$ALL" -eq 1 ]] || fail "pass --machine <id> or --all"
[[ $EUID -eq 0 ]] || fail "must run as root (writes into $PROTEOS_AGENT_VOLUMES_DIR)"

mkdir -p "$PROTEOS_AGENT_VOLUMES_DIR"
chmod 700 "$PROTEOS_AGENT_VOLUMES_DIR"

restore_one() {
  local id="$1"
  local src="$SNAPSHOT/$id.luks"
  local dst="$PROTEOS_AGENT_VOLUMES_DIR/$id.luks"
  [[ -f "$src" ]] || fail "no $id.luks in $SNAPSHOT"
  local mapper="proteos-$(echo -n "$id" | tr -d '-' | head -c 8)"
  if "$CRYPTSETUP" status "$mapper" >/dev/null 2>&1; then
    fail "$mapper is currently open (machine $id is running/mounted) — stop the node-agent or the machine first"
  fi
  if [[ -f "$dst" && "$FORCE" -ne 1 ]]; then
    read -r -p "$dst already exists. Overwrite? [y/N] " CONFIRM
    [[ "$CONFIRM" == "y" || "$CONFIRM" == "Y" ]] || { echo "skipping $id"; return; }
  fi
  cp "$src" "$dst"
  chmod 600 "$dst"
  ok "restored $id -> $dst"
}

if [[ "$ALL" -eq 1 ]]; then
  shopt -s nullglob
  found=0
  for f in "$SNAPSHOT"/*.luks; do
    restore_one "$(basename "$f" .luks)"
    found=1
  done
  shopt -u nullglob
  [[ "$found" -eq 1 ]] || fail "no *.luks files in $SNAPSHOT"
else
  restore_one "$MACHINE"
fi

cat <<'DONE'

Reminder: the ciphertext is restored, but each machine's volume KEY lives in
OpenBao (secret/machines/<machine_id>/volume-key), not here. Make sure OpenBao
was restored to a matching point in time (see docs/disaster-recovery.md) —
otherwise the node-agent's next `ensure` will fail to luksOpen with a
wrong-key error.
DONE
