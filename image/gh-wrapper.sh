#!/bin/sh
# ProteOS gh wrapper — baked at /usr/local/bin/gh by build-rootfs.sh; the real
# GitHub CLI binary lives at /usr/local/libexec/proteos/gh.
#
# gh does not use git's credential.helper: it authenticates from GH_TOKEN /
# GITHUB_TOKEN or ~/.config/gh/hosts.yml. ProteOS never writes tokens to disk
# (Phase 7 decision #5), so this wrapper mints a fresh short-lived token per
# invocation through the same plumbing git already uses (guestagent
# git-credential → /run/proteos/agent.sock → control channel → control-plane
# TokenSource) and hands it to the real gh via the environment. The token only
# transits memory and can never go stale mid-session.
#
# A caller-provided GH_TOKEN/GITHUB_TOKEN wins, matching gh's own precedence.
# On lookup failure the helper prints an actionable message on stderr (e.g.
# "reconnect GitHub in the ProteOS dashboard") and gh runs unauthenticated.
if [ -z "${GH_TOKEN:-}" ] && [ -z "${GITHUB_TOKEN:-}" ]; then
  tok="$(printf 'host=github.com\nprotocol=https\n\n' \
    | /usr/local/bin/guestagent git-credential get \
    | sed -n 's/^password=//p')"
  if [ -n "$tok" ]; then
    GH_TOKEN="$tok"
    export GH_TOKEN
  fi
  unset tok
fi
exec /usr/local/libexec/proteos/gh "$@"
