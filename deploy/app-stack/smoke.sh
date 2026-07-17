#!/usr/bin/env bash
# E2E smoke probes (TAV-40): boot deploy/app-stack/compose.smoke.yaml, prove the
# production images wire together, tear down. Run via `task smoke` or CI.
#
# What a pass means:
#   1. the control plane came up and applied its migrations against Postgres
#      (GET /healthz on the API container answers ok);
#   2. nginx serves the built SPA (GET / returns the index page);
#   3. the /api reverse proxy reaches the control plane through the compose
#      network (an unauthenticated GET /api/me comes back 401 FROM THE API —
#      a misrouted proxy would return the SPA's 200/index.html instead).
set -euo pipefail

cd "$(dirname "$0")"
compose() { docker compose -f compose.smoke.yaml "$@"; }

CP=http://127.0.0.1:18080 # controlplane, direct
WEB=http://127.0.0.1:18081 # SPA + reverse proxy

cleanup() { compose down -v --remove-orphans >/dev/null 2>&1 || true; }
trap cleanup EXIT

echo "== smoke: building + booting the app stack"
compose up --build -d

echo "== smoke: waiting for the control plane to become healthy"
for i in $(seq 1 60); do
  if curl -fsS "$CP/healthz" | grep -q '"ok"'; then
    break
  fi
  if [ "$i" = 60 ]; then
    echo "FAIL: control plane never became healthy" >&2
    compose logs controlplane >&2
    exit 1
  fi
  sleep 2
done
echo "ok: /healthz answers on the control plane"

echo "== smoke: SPA served by nginx"
# nginx starts alongside the control plane, so it may not be listening yet when
# /healthz answers on the first try — retry like the healthz probe (a not-yet-
# listening published port shows up as connection reset, hence the retries).
for i in $(seq 1 30); do
  if index=$(curl -fsS "$WEB/" 2>/dev/null); then
    break
  fi
  if [ "$i" = 30 ]; then
    echo "FAIL: nginx never answered on $WEB" >&2
    compose logs web >&2
    exit 1
  fi
  sleep 2
done
if ! echo "$index" | grep -qi '<div id="root">'; then
  echo "FAIL: GET / did not return the SPA index (got: ${index:0:200})" >&2
  exit 1
fi
echo "ok: nginx serves the built SPA"

echo "== smoke: /api reverse proxy reaches the control plane"
status=$(curl -s -o /dev/null -w '%{http_code}' "$WEB/api/me")
if [ "$status" != "401" ]; then
  echo "FAIL: GET /api/me via nginx returned $status, want 401 from the API" >&2
  echo "      (200 means the proxy fell through to the SPA — the /gw/ gotcha)" >&2
  exit 1
fi
echo "ok: unauthenticated /api/me is 401 from the control plane"

echo "== smoke: PASS"
