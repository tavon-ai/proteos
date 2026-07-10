#!/usr/bin/env bash
# backup-volumes.sh — back up every per-machine LUKS volume container
# (<id>.luks) under PROTEOS_AGENT_VOLUMES_DIR using rsync's --link-dest
# incremental-snapshot pattern: each run produces a full, independently
# restorable directory tree, but files unchanged since the previous run are
# hardlinked instead of copied, so disk usage only grows with what actually
# changed. TAV-31.
#
# The LUKS containers are plain files opened directly by cryptsetup (no loop
# device); this backs up the *ciphertext* only — the volume key never touches
# disk here. It lives in OpenBao at secret/machines/<machine_id>/volume-key,
# delivered to the node-agent on every `ensure` call (see
# nodeagent/internal/driver/firecracker/volume.go) and never persisted
# host-side. Back up OpenBao too (deploy/app-stack/backup-openbao.sh) — a
# volume backup without the matching key backup is ciphertext nobody can open.
#
# A volume whose mapper is currently open (its microVM is running) is copied
# live: this is CRASH-CONSISTENT, not application-consistent — ext4's journal
# replays any in-flight write on next open, the same guarantee as pulling
# power on a running VM's disk. Stop or hibernate a machine first for a fully
# quiesced copy of its volume.
#
#   sudo ./backup-volumes.sh
#   PROTEOS_VOLUME_BACKUP_DIR=/mnt/nas/volumes sudo ./backup-volumes.sh
#
# Config comes from deploy/node-agent/.env (PROTEOS_AGENT_VOLUMES_DIR,
# PROTEOS_CRYPTSETUP_BIN — the same file the node-agent itself uses) and the
# optional deploy/node-agent/backup.env. See docs/disaster-recovery.md and
# restore-volumes.sh for the restore side.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

[[ -f "$SCRIPT_DIR/.env" ]] && { set -a; . "$SCRIPT_DIR/.env"; set +a; }
[[ -f "$SCRIPT_DIR/backup.env" ]] && { set -a; . "$SCRIPT_DIR/backup.env"; set +a; }

: "${PROTEOS_AGENT_VOLUMES_DIR:=/var/lib/proteos/volumes}"
: "${PROTEOS_VOLUME_BACKUP_DIR:=/var/backups/proteos/volumes}"
: "${PROTEOS_VOLUME_BACKUP_RETENTION:=7}"    # snapshots to keep, not days (sizes vary a lot)
: "${PROTEOS_CRYPTSETUP_BIN:=/usr/sbin/cryptsetup}"
: "${PROTEOS_VOLUME_BACKUP_REMOTE:=}"

fail() { echo "[fail] $*" >&2; exit 1; }
ok()   { echo "[ ok ] $*"; }

# rsync does the local link-dest snapshot unconditionally (not just the
# optional remote push), so it's required regardless of PROTEOS_VOLUME_BACKUP_REMOTE.
command -v rsync >/dev/null || fail "rsync not found"
[[ -d "$PROTEOS_AGENT_VOLUMES_DIR" ]] || fail "volumes dir not found: $PROTEOS_AGENT_VOLUMES_DIR"

CRYPTSETUP="$PROTEOS_CRYPTSETUP_BIN"
command -v "$CRYPTSETUP" >/dev/null 2>&1 || CRYPTSETUP="cryptsetup"

mkdir -p "$PROTEOS_VOLUME_BACKUP_DIR"
exec 200>"$PROTEOS_VOLUME_BACKUP_DIR/.backup.lock"
flock -n 200 || fail "another backup-volumes.sh run is already in progress"

shopt -s nullglob
VOLUMES=("$PROTEOS_AGENT_VOLUMES_DIR"/*.luks)
shopt -u nullglob
if [[ ${#VOLUMES[@]} -eq 0 ]]; then
  echo "no *.luks volumes found in $PROTEOS_AGENT_VOLUMES_DIR; nothing to do"
  exit 0
fi

echo "==> ${#VOLUMES[@]} volume(s) found"
for v in "${VOLUMES[@]}"; do
  id="$(basename "$v" .luks)"
  mapper="proteos-$(echo -n "$id" | tr -d '-' | head -c 8)"
  if "$CRYPTSETUP" status "$mapper" 2>/dev/null | grep -q ' is active'; then
    echo "  - $id (hot: mapper $mapper is open — crash-consistent copy only)"
  else
    echo "  - $id (cold)"
  fi
done

TIMESTAMP="$(date -u +%Y%m%dT%H%M%SZ)"
DEST="$PROTEOS_VOLUME_BACKUP_DIR/$TIMESTAMP"
INCOMING="$PROTEOS_VOLUME_BACKUP_DIR/.incoming"
LATEST_LINK="$PROTEOS_VOLUME_BACKUP_DIR/latest"

rm -rf "$INCOMING"
mkdir -p "$INCOMING"

# --inplace is deliberately NOT used: it would let rsync overwrite bytes
# in-place on a file that --link-dest just hardlinked from the previous
# snapshot, corrupting that older snapshot too. The default copy-then-rename
# behavior is what makes --link-dest safe.
LINK_DEST_ARGS=()
if [[ -d "$LATEST_LINK" ]]; then
  LINK_DEST_ARGS=(--link-dest="$LATEST_LINK")
fi

echo "==> rsync -> $INCOMING (link-dest=${LATEST_LINK:-<none, first run>})"
rsync -a "${LINK_DEST_ARGS[@]}" "$PROTEOS_AGENT_VOLUMES_DIR"/*.luks "$INCOMING/"

mv "$INCOMING" "$DEST"
ln -sfn "$DEST" "$LATEST_LINK"
ok "snapshot at $DEST ($(du -sh "$DEST" | cut -f1))"

# Retention: keep the N most recent snapshots (dirs named as UTC timestamps).
mapfile -t SNAPSHOTS < <(find "$PROTEOS_VOLUME_BACKUP_DIR" -maxdepth 1 -mindepth 1 -type d \
  -regextype posix-extended -regex '.*/[0-9]{8}T[0-9]{6}Z' | sort -r)
if [[ ${#SNAPSHOTS[@]} -gt "$PROTEOS_VOLUME_BACKUP_RETENTION" ]]; then
  for old in "${SNAPSHOTS[@]:$PROTEOS_VOLUME_BACKUP_RETENTION}"; do
    echo "  pruning $old"
    rm -rf "$old"
  done
fi

# Off-host copy — a backup on the SAME disk as the primary is not a backup.
if [[ -n "$PROTEOS_VOLUME_BACKUP_REMOTE" ]]; then
  echo "==> Syncing $DEST -> $PROTEOS_VOLUME_BACKUP_REMOTE/$TIMESTAMP"
  rsync -a "$DEST"/ "$PROTEOS_VOLUME_BACKUP_REMOTE/$TIMESTAMP/"
  ok "synced to $PROTEOS_VOLUME_BACKUP_REMOTE/$TIMESTAMP"
fi
