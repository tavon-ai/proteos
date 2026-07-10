#!/usr/bin/env bash
# restore-openbao.sh — restore an OpenBao backup bundle written by
# backup-openbao.sh: the baodata volume plus the unseal key / AppRole
# secret_id / init.json operator material. TAV-31.
#
# DESTRUCTIVE: replaces the current baodata volume contents entirely.
#
#   ./restore-openbao.sh --latest
#   ./restore-openbao.sh /var/backups/proteos/openbao/proteos-openbao-2026.../
#
# See docs/disaster-recovery.md for the full procedure, including what to do
# if this OpenBao held per-machine volume keys for LUKS volumes restored from
# a different point in time (restore-volumes.sh).
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
COMPOSE_FILE="$SCRIPT_DIR/compose.yaml"

[[ -f "$SCRIPT_DIR/.env" ]] && { set -a; . "$SCRIPT_DIR/.env"; set +a; }
[[ -f "$SCRIPT_DIR/backup.env" ]] && { set -a; . "$SCRIPT_DIR/backup.env"; set +a; }

: "${PROTEOS_OPENBAO_BACKUP_DIR:=/var/backups/proteos/openbao}"
: "${PROTEOS_BAODATA_VOLUME:=proteos_baodata}"

fail() { echo "[fail] $*" >&2; exit 1; }
ok()   { echo "[ ok ] $*"; }

FORCE=0
SRC=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --latest) SRC="$(ls -1dt "$PROTEOS_OPENBAO_BACKUP_DIR"/proteos-openbao-*/ 2>/dev/null | head -n1)" ;;
    --force) FORCE=1 ;;
    -h|--help) echo "usage: $0 (--latest | <bundle-dir>) [--force]"; exit 0 ;;
    *) SRC="$1" ;;
  esac
  shift
done

[[ -n "$SRC" ]] || fail "no backup bundle given (pass a path or --latest)"
SRC="${SRC%/}"
[[ -d "$SRC" ]] || fail "backup bundle not found: $SRC"
[[ -f "$SRC/baodata.tgz" ]] || fail "$SRC/baodata.tgz missing — not a valid bundle"
command -v docker >/dev/null || fail "docker not found"
[[ -f "$COMPOSE_FILE" ]] || fail "compose file not found: $COMPOSE_FILE"

# Verify the archive is readable BEFORE wiping the live volume — a truncated
# or corrupt bundle must fail here, not after the current secrets are gone.
docker run --rm -v "$SRC":/b:ro alpine tar tzf /b/baodata.tgz >/dev/null || \
  fail "$SRC/baodata.tgz is not a valid gzip/tar archive — refusing to touch the live volume"

echo "About to REPLACE the OpenBao volume '$PROTEOS_BAODATA_VOLUME' with:"
echo "  $SRC/baodata.tgz"
if [[ "$FORCE" -ne 1 ]]; then
  read -r -p "This destroys ALL current secrets in OpenBao. Type 'restore' to confirm: " CONFIRM
  [[ "$CONFIRM" == "restore" ]] || fail "confirmation did not match; aborting"
fi

echo "==> Stopping the stack's openbao-dependent services"
docker compose -f "$COMPOSE_FILE" stop controlplane bao-unsealer openbao

echo "==> Wiping and restoring $PROTEOS_BAODATA_VOLUME"
docker run --rm -v "$PROTEOS_BAODATA_VOLUME":/d alpine sh -c 'find /d -mindepth 1 -delete'
docker run --rm -v "$PROTEOS_BAODATA_VOLUME":/d -v "$SRC":/b:ro alpine tar xzf /b/baodata.tgz -C /d

for f in openbao-init.json bao-unseal-key openbao-secret-id; do
  if [[ -f "$SRC/$f" ]]; then
    cp "$SRC/$f" "$SCRIPT_DIR/$f"
    chmod 600 "$SCRIPT_DIR/$f"
  else
    echo "  (warning: $f missing from bundle — restore it manually or re-run openbao-init.sh)" >&2
  fi
done

echo "==> Starting openbao (bao-unsealer will auto-unseal within ~10s)"
docker compose -f "$COMPOSE_FILE" up -d openbao bao-unsealer
sleep 12
docker compose -f "$COMPOSE_FILE" exec openbao bao status || true
docker compose -f "$COMPOSE_FILE" up -d controlplane

ok "restore complete — verify: docker compose -f $COMPOSE_FILE logs bao-unsealer | grep unsealed"
