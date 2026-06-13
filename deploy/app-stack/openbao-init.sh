#!/usr/bin/env bash
# openbao-init.sh — one-time (idempotent) OpenBao setup for the ProteOS app stack
# (Phase 5 decision #3). Run it from the app VM after `docker compose up`, with
# the `bao` CLI installed and BAO_ADDR pointing at the openbao service.
#
#   export BAO_ADDR=http://127.0.0.1:8200      # the published openbao port
#   ./openbao-init.sh
#
# What it does, each step guarded so re-running is safe:
#   1. operator init (1 unseal key / threshold 1) — writes openbao-init.json
#   2. unseal + login with the recorded root token
#   3. enable KV v2 at secret/, a file audit device, and AppRole auth
#   4. write policy cp-base, the proteos-user token role, and the proteos-cp
#      AppRole role
#   5. emit role_id into .env (PROTEOS_OPENBAO_ROLE_ID) and a fresh secret_id
#      into ./openbao-secret-id (mounted into the control plane)
#
# After it runs: set PROTEOS_SECRETS_BACKEND=openbao in .env and restart the
# control plane (`docker compose up -d controlplane`).
#
# NOTE: openbao boots SEALED and seals again on every restart. After a restart,
# re-run just the unseal:  bao operator unseal "$(jq -r .unseal_key openbao-init.json)"
set -euo pipefail

: "${BAO_ADDR:=http://127.0.0.1:8200}"
export BAO_ADDR
HERE="$(cd "$(dirname "$0")" && pwd)"
INIT_JSON="$HERE/openbao-init.json"
SECRET_ID_FILE="$HERE/openbao-secret-id"
ENV_FILE="$HERE/.env"
MOUNT="${PROTEOS_OPENBAO_MOUNT:-secret}"

need() { command -v "$1" >/dev/null 2>&1 || { echo "error: '$1' not found in PATH" >&2; exit 1; }; }
need bao
need jq

echo "==> Using BAO_ADDR=$BAO_ADDR"

# 1. Initialize (only if not already initialized).
if [ "$(bao status -format=json 2>/dev/null | jq -r .initialized)" = "true" ]; then
  echo "==> Already initialized; reusing $INIT_JSON"
  [ -f "$INIT_JSON" ] || { echo "error: initialized but $INIT_JSON missing — cannot unseal/login" >&2; exit 1; }
else
  echo "==> Initializing (1 key share, threshold 1)"
  bao operator init -key-shares=1 -key-threshold=1 -format=json > "$INIT_JSON"
  chmod 600 "$INIT_JSON"
  echo "    wrote $INIT_JSON (KEEP IT SAFE — unseal key + root token)"
fi

UNSEAL_KEY="$(jq -r '.unseal_keys_b64[0]' "$INIT_JSON")"
ROOT_TOKEN="$(jq -r '.root_token' "$INIT_JSON")"

# 2. Unseal (no-op if already unsealed) + login.
if [ "$(bao status -format=json 2>/dev/null | jq -r .sealed)" = "true" ]; then
  echo "==> Unsealing"
  bao operator unseal "$UNSEAL_KEY" >/dev/null
fi
export BAO_TOKEN="$ROOT_TOKEN"

# 3. Enable KV v2 at secret/ (idempotent).
if bao secrets list -format=json | jq -e '."'"$MOUNT"'/"' >/dev/null 2>&1; then
  echo "==> KV mount $MOUNT/ already enabled"
else
  echo "==> Enabling KV v2 at $MOUNT/"
  bao secrets enable -path="$MOUNT" -version=2 kv
fi

# File audit device (idempotent).
if bao audit list -format=json 2>/dev/null | jq -e '."file/"' >/dev/null 2>&1; then
  echo "==> Audit device already enabled"
else
  echo "==> Enabling file audit device at /openbao/logs/audit.log"
  bao audit enable file file_path=/openbao/logs/audit.log
fi

# AppRole auth (idempotent).
if bao auth list -format=json | jq -e '."approle/"' >/dev/null 2>&1; then
  echo "==> AppRole auth already enabled"
else
  echo "==> Enabling AppRole auth"
  bao auth enable approle
fi

# 4. Policy cp-base: machine secrets + the machinery to mint per-user tokens.
echo "==> Writing policy cp-base"
bao policy write cp-base - <<EOF
path "auth/token/create/proteos-user" {
  capabilities = ["update"]
}
path "sys/policies/acl/user-*" {
  capabilities = ["create", "update", "read"]
}
path "$MOUNT/data/machines/*" {
  capabilities = ["create", "update", "read", "delete"]
}
path "$MOUNT/metadata/machines/*" {
  capabilities = ["read", "delete", "list"]
}
EOF

# proteos-user token role: orphan, short-lived, glob-scoped to user-* policies.
echo "==> Writing token role proteos-user"
bao write auth/token/roles/proteos-user \
  allowed_policies_glob="user-*" \
  orphan=true \
  renewable=false \
  token_ttl=90s \
  token_type=service >/dev/null

# proteos-cp AppRole: the control plane's identity. Reusable secret_id (the
# BaoStore re-logs-in on token expiry), bounded token TTLs.
echo "==> Writing AppRole role proteos-cp"
bao write auth/approle/role/proteos-cp \
  token_policies=cp-base \
  token_ttl=1h \
  token_max_ttl=4h \
  secret_id_ttl=0 \
  secret_id_num_uses=0 >/dev/null

# 5. Emit role_id (.env) + a fresh secret_id (file).
ROLE_ID="$(bao read -field=role_id auth/approle/role/proteos-cp/role-id)"
SECRET_ID="$(bao write -f -field=secret_id auth/approle/role/proteos-cp/secret-id)"

umask 077
printf '%s' "$SECRET_ID" > "$SECRET_ID_FILE"
echo "==> Wrote secret_id to $SECRET_ID_FILE"

# Update PROTEOS_OPENBAO_ROLE_ID in .env (append or replace).
if [ -f "$ENV_FILE" ] && grep -q '^PROTEOS_OPENBAO_ROLE_ID=' "$ENV_FILE"; then
  tmp="$(mktemp)"
  sed "s|^PROTEOS_OPENBAO_ROLE_ID=.*|PROTEOS_OPENBAO_ROLE_ID=$ROLE_ID|" "$ENV_FILE" > "$tmp"
  mv "$tmp" "$ENV_FILE"
else
  echo "PROTEOS_OPENBAO_ROLE_ID=$ROLE_ID" >> "$ENV_FILE"
fi
echo "==> Set PROTEOS_OPENBAO_ROLE_ID in $ENV_FILE"

cat <<DONE

OpenBao is initialized and unsealed.

Next:
  1. In $ENV_FILE set:  PROTEOS_SECRETS_BACKEND=openbao
  2. Restart the control plane:  docker compose up -d controlplane
  3. (Migrating an existing dev FileStore? See RUNBOOK Part D — controlplane -migrate-secrets)

After ANY openbao restart it boots sealed; unseal with:
  BAO_ADDR=$BAO_ADDR bao operator unseal "\$(jq -r '.unseal_keys_b64[0]' $INIT_JSON)"
DONE
