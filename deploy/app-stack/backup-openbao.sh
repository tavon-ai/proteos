#!/usr/bin/env bash
# backup-openbao.sh — snapshot the OpenBao file-storage volume plus the
# operator material (unseal key, AppRole secret_id, init.json) that is USELESS
# without each other. TAV-31.
#
# By default this briefly stops openbao + bao-unsealer for a consistent tar of
# the file backend (the "file" storage backend has no built-in snapshot
# primitive — that's raft-only). Pass --online to skip the stop and accept a
# live, best-effort-consistent copy instead (each KV entry is its own file
# written via rename, so a live copy is usually fine, but a bundle taken
# --online was not proven point-in-time consistent).
#
#   ./backup-openbao.sh              # brief openbao downtime, most consistent
#   ./backup-openbao.sh --online     # no downtime
#
# Config comes from deploy/app-stack/.env and the optional
# deploy/app-stack/backup.env. See docs/disaster-recovery.md and
# restore-openbao.sh for the restore side.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
COMPOSE_FILE="$SCRIPT_DIR/compose.yaml"

[[ -f "$SCRIPT_DIR/.env" ]] && { set -a; . "$SCRIPT_DIR/.env"; set +a; }
[[ -f "$SCRIPT_DIR/backup.env" ]] && { set -a; . "$SCRIPT_DIR/backup.env"; set +a; }

: "${PROTEOS_OPENBAO_BACKUP_DIR:=/var/backups/proteos/openbao}"
: "${PROTEOS_OPENBAO_BACKUP_RETENTION_DAYS:=14}"
: "${PROTEOS_OPENBAO_BACKUP_REMOTE:=}"
# The docker volume name is the compose project name ("proteos", pinned by
# compose.yaml's top-level `name:`) + "_baodata".
: "${PROTEOS_BAODATA_VOLUME:=proteos_baodata}"

fail() { echo "[fail] $*" >&2; exit 1; }
ok()   { echo "[ ok ] $*"; }

ONLINE=0
while [[ $# -gt 0 ]]; do
  case "$1" in
    --online) ONLINE=1 ;;
    -h|--help) echo "usage: $0 [--online]"; exit 0 ;;
    *) fail "unknown argument: $1" ;;
  esac
  shift
done

command -v docker >/dev/null || fail "docker not found"
command -v rsync >/dev/null 2>&1 || [[ -z "$PROTEOS_OPENBAO_BACKUP_REMOTE" ]] || fail "rsync not found (needed for PROTEOS_OPENBAO_BACKUP_REMOTE)"
[[ -f "$COMPOSE_FILE" ]] || fail "compose file not found: $COMPOSE_FILE"
docker volume inspect "$PROTEOS_BAODATA_VOLUME" >/dev/null 2>&1 || \
  fail "docker volume '$PROTEOS_BAODATA_VOLUME' not found (set PROTEOS_BAODATA_VOLUME)"

mkdir -p "$PROTEOS_OPENBAO_BACKUP_DIR"
exec 200>"$PROTEOS_OPENBAO_BACKUP_DIR/.backup.lock"
flock -n 200 || fail "another backup-openbao.sh run is already in progress"

TIMESTAMP="$(date -u +%Y%m%dT%H%M%SZ)"
BUNDLE_DIR="$PROTEOS_OPENBAO_BACKUP_DIR/proteos-openbao-$TIMESTAMP"
mkdir -p "$BUNDLE_DIR"

STOPPED=0
cleanup() {
  if [[ "$STOPPED" -eq 1 ]]; then
    echo "==> Restarting openbao + bao-unsealer"
    docker compose -f "$COMPOSE_FILE" start openbao bao-unsealer
    STOPPED=0
  fi
}
# Registered BEFORE the stop call below: if `docker compose stop` itself exits
# non-zero partway through (e.g. one container stops, the other errors),
# set -e must still trigger a restart attempt rather than leaving OpenBao down
# with no automatic recovery.
trap cleanup EXIT

if [[ "$ONLINE" -ne 1 ]]; then
  echo "==> Stopping openbao + bao-unsealer for a consistent snapshot (--online to skip)"
  STOPPED=1
  docker compose -f "$COMPOSE_FILE" stop bao-unsealer openbao
fi

echo "==> Archiving volume $PROTEOS_BAODATA_VOLUME"
docker run --rm \
  -v "$PROTEOS_BAODATA_VOLUME":/d:ro \
  -v "$BUNDLE_DIR":/b \
  alpine tar czf /b/baodata.tgz -C /d .

# The operator material: without these, baodata.tgz cannot be unsealed or
# reached by the control plane's AppRole. All three are gitignored on purpose.
for f in openbao-init.json bao-unseal-key openbao-secret-id; do
  if [[ -s "$SCRIPT_DIR/$f" ]]; then
    cp "$SCRIPT_DIR/$f" "$BUNDLE_DIR/$f"
  else
    echo "  (warning: $f missing/empty next to the script; skipped)" >&2
  fi
done
chmod -R go-rwx "$BUNDLE_DIR"

cleanup
trap - EXIT

ok "wrote bundle $BUNDLE_DIR"

if [[ "$PROTEOS_OPENBAO_BACKUP_RETENTION_DAYS" -gt 0 ]]; then
  find "$PROTEOS_OPENBAO_BACKUP_DIR" -maxdepth 1 -type d -name 'proteos-openbao-*' \
    -mtime "+$PROTEOS_OPENBAO_BACKUP_RETENTION_DAYS" -exec rm -rf {} +
fi

if [[ -n "$PROTEOS_OPENBAO_BACKUP_REMOTE" ]]; then
  echo "==> Syncing $PROTEOS_OPENBAO_BACKUP_DIR -> $PROTEOS_OPENBAO_BACKUP_REMOTE"
  rsync -a --delete "$PROTEOS_OPENBAO_BACKUP_DIR"/ "$PROTEOS_OPENBAO_BACKUP_REMOTE"
  ok "synced to $PROTEOS_OPENBAO_BACKUP_REMOTE"
fi
