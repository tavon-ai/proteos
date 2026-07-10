#!/usr/bin/env bash
# backup-postgres.sh — pg_dump the app-stack Postgres to a timestamped,
# custom-format dump, prune old dumps, and (optionally) push the result
# off-host. TAV-31.
#
# Runs pg_dump *inside* the running postgres container via `docker compose
# exec`, so it needs no network creds beyond the container's own trust auth —
# the same path RUNBOOK Part E1 already uses to spot-check dumps for leaked
# secrets.
#
#   ./backup-postgres.sh
#   PROTEOS_PG_BACKUP_DIR=/mnt/nas/pg ./backup-postgres.sh
#
# Config comes from deploy/app-stack/.env (POSTGRES_USER/POSTGRES_DB — the
# same file the stack itself uses) and the optional deploy/app-stack/backup.env
# (backup-specific overrides). See docs/disaster-recovery.md for the full
# restore procedure and restore-postgres.sh for the companion script.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
COMPOSE_FILE="$SCRIPT_DIR/compose.yaml"

[[ -f "$SCRIPT_DIR/.env" ]] && { set -a; . "$SCRIPT_DIR/.env"; set +a; }
[[ -f "$SCRIPT_DIR/backup.env" ]] && { set -a; . "$SCRIPT_DIR/backup.env"; set +a; }

: "${POSTGRES_USER:=proteos}"
: "${POSTGRES_DB:=proteos}"
: "${PROTEOS_PG_BACKUP_DIR:=/var/backups/proteos/postgres}"
: "${PROTEOS_PG_BACKUP_RETENTION_DAYS:=14}"
: "${PROTEOS_PG_BACKUP_REMOTE:=}"    # e.g. proteos-backup@offsite.example:/backups/proteos/postgres/

fail() { echo "[fail] $*" >&2; exit 1; }
ok()   { echo "[ ok ] $*"; }

command -v docker >/dev/null || fail "docker not found"
command -v rsync >/dev/null 2>&1 || [[ -z "$PROTEOS_PG_BACKUP_REMOTE" ]] || fail "rsync not found (needed for PROTEOS_PG_BACKUP_REMOTE)"
[[ -f "$COMPOSE_FILE" ]] || fail "compose file not found: $COMPOSE_FILE"

mkdir -p "$PROTEOS_PG_BACKUP_DIR"

# Serialize concurrent runs (a manual run racing the timer, etc).
exec 200>"$PROTEOS_PG_BACKUP_DIR/.backup.lock"
flock -n 200 || fail "another backup-postgres.sh run is already in progress"

TIMESTAMP="$(date -u +%Y%m%dT%H%M%SZ)"
DEST="$PROTEOS_PG_BACKUP_DIR/proteos-postgres-$TIMESTAMP.dump"
TMP="$DEST.tmp"
trap 'rm -f "$TMP"' EXIT

echo "==> Dumping $POSTGRES_DB (custom format) -> $DEST"
docker compose -f "$COMPOSE_FILE" exec -T postgres \
  pg_dump -U "$POSTGRES_USER" -d "$POSTGRES_DB" --format=custom --compress=6 > "$TMP"
chmod 600 "$TMP"
mv "$TMP" "$DEST"
trap - EXIT
ok "wrote $(du -h "$DEST" | cut -f1) to $DEST"

# Retention: delete local dumps older than N days.
if [[ "$PROTEOS_PG_BACKUP_RETENTION_DAYS" -gt 0 ]]; then
  find "$PROTEOS_PG_BACKUP_DIR" -maxdepth 1 -name 'proteos-postgres-*.dump' \
    -mtime "+$PROTEOS_PG_BACKUP_RETENTION_DAYS" -print -delete
fi

# Off-host copy — a backup on the SAME disk as the primary is not a backup.
if [[ -n "$PROTEOS_PG_BACKUP_REMOTE" ]]; then
  echo "==> Syncing $PROTEOS_PG_BACKUP_DIR -> $PROTEOS_PG_BACKUP_REMOTE"
  rsync -a --delete "$PROTEOS_PG_BACKUP_DIR"/ "$PROTEOS_PG_BACKUP_REMOTE"
  ok "synced to $PROTEOS_PG_BACKUP_REMOTE"
fi
