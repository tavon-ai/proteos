#!/bin/sh
# Polls OpenBao every 10 s and unseals it whenever it is found sealed.
# The unseal key (base64, written by openbao-init.sh) is read from
# /run/secrets/bao-unseal-key.  If that file is empty the script waits
# silently — this is normal before openbao-init.sh has run.
#
# Exit codes from `bao status`:
#   0 = initialized, unsealed, active
#   1 = error / unreachable
#   2 = sealed
set -u

BAO_ADDR="${BAO_ADDR:-http://openbao:8200}"
KEY_FILE="/run/secrets/bao-unseal-key"
export BAO_ADDR

echo "unsealer: starting (addr=$BAO_ADDR)"

while true; do
  bao status >/dev/null 2>&1
  rc=$?

  case $rc in
    0)
      # Unsealed and active — nothing to do.
      ;;
    2)
      # Sealed.  Unseal if the key has been written by openbao-init.sh.
      if [ ! -s "$KEY_FILE" ]; then
        echo "unsealer: OpenBao is sealed but key not yet written — waiting for openbao-init.sh"
      else
        echo "unsealer: OpenBao is sealed, unsealing…"
        bao operator unseal "$(cat "$KEY_FILE")" >/dev/null 2>&1 \
          && echo "unsealer: unsealed successfully" \
          || echo "unsealer: unseal attempt failed (will retry)"
      fi
      ;;
    *)
      # Server not reachable yet — log nothing, just wait.
      ;;
  esac

  sleep 10
done
