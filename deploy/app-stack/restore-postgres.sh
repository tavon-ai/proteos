#!/usr/bin/env bash
# restore-postgres.sh — restore a pg_dump custom-format backup (written by
# backup-postgres.sh) into the app-stack Postgres. TAV-31.
#
# DESTRUCTIVE: drops and recreates every object in the target database before
# restoring. Stops the control plane first so it doesn't race the restore or
# crash-loop against a half-restored schema.
#
#   ./restore-postgres.sh --latest
#   ./restore-postgres.sh /var/backups/proteos/postgres/proteos-postgres-2026....dump
#
# See docs/disaster-recovery.md for the full procedure.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
COMPOSE_FILE="$SCRIPT_DIR/compose.yaml"

[[ -f "$SCRIPT_DIR/.env" ]] && { set -a; . "$SCRIPT_DIR/.env"; set +a; }
[[ -f "$SCRIPT_DIR/backup.env" ]] && { set -a; . "$SCRIPT_DIR/backup.env"; set +a; }

: "${POSTGRES_USER:=proteos}"
: "${POSTGRES_DB:=proteos}"
: "${PROTEOS_PG_BACKUP_DIR:=/var/backups/proteos/postgres}"

fail() { echo "[fail] $*" >&2; exit 1; }
ok()   { echo "[ ok ] $*"; }

FORCE=0
SRC=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --latest) SRC="$(ls -1t "$PROTEOS_PG_BACKUP_DIR"/proteos-postgres-*.dump 2>/dev/null | head -n1)" ;;
    --force) FORCE=1 ;;
    -h|--help) echo "usage: $0 (--latest | <dump-file>) [--force]"; exit 0 ;;
    *) SRC="$1" ;;
  esac
  shift
done

[[ -n "$SRC" ]] || fail "no backup file given (pass a path or --latest)"
[[ -f "$SRC" ]] || fail "backup file not found: $SRC"
command -v docker >/dev/null || fail "docker not found"
[[ -f "$COMPOSE_FILE" ]] || fail "compose file not found: $COMPOSE_FILE"

echo "About to DROP and RESTORE database '$POSTGRES_DB' from:"
echo "  $SRC"
if [[ "$FORCE" -ne 1 ]]; then
  read -r -p "This destroys all current data in '$POSTGRES_DB'. Type the database name to confirm: " CONFIRM
  [[ "$CONFIRM" == "$POSTGRES_DB" ]] || fail "confirmation did not match; aborting"
fi

echo "==> Stopping the control plane so it doesn't race the restore"
docker compose -f "$COMPOSE_FILE" stop controlplane 2>/dev/null || true

echo "==> Restoring into $POSTGRES_DB"
docker compose -f "$COMPOSE_FILE" exec -T postgres \
  pg_restore -U "$POSTGRES_USER" -d "$POSTGRES_DB" --clean --if-exists --no-owner < "$SRC"
ok "restore complete"

echo "==> Bringing the control plane back up (it applies pending migrations on boot)"
docker compose -f "$COMPOSE_FILE" up -d controlplane
ok "done — check: docker compose -f $COMPOSE_FILE logs -f controlplane"
