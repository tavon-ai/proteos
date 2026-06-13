# shellcheck shell=sh
# Installed as /etc/profile.d/proteos-providers.sh — makes injected provider
# secrets available to login shells (Phase 5 decision #9a).
#
# The control plane pushes per-provider env files to /run/proteos/env/<key>.env
# (tmpfs, 0600) at machine start/resume. Sourcing them here means typing `claude`
# (or any future provider CLI) in a fresh terminal just works — ANTHROPIC_API_KEY
# and friends are already in the environment.
#
# Sessions run `<shell> -l`, so /etc/profile (and thus this snippet) is sourced.
# Kept POSIX-sh safe: if no env files exist the glob stays literal and the
# readability test skips it, so this is a no-op on a machine with nothing injected.
if [ -d /run/proteos/env ]; then
  for _proteos_env in /run/proteos/env/*.env; do
    # shellcheck disable=SC1090  # runtime-injected files; path is intentional
    [ -r "$_proteos_env" ] && . "$_proteos_env"
  done
  unset _proteos_env
fi
