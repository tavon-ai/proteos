#!/usr/bin/env bash
# backup-all.sh — run every app-stack backup (Postgres + OpenBao) back to
# back. Convenience wrapper for cron/systemd — see proteos-backup.service and
# docs/disaster-recovery.md. The LUKS volume backup is separate: it runs on
# the KVM host, see deploy/node-agent/backup-volumes.sh.
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
"$SCRIPT_DIR/backup-postgres.sh"
"$SCRIPT_DIR/backup-openbao.sh"
