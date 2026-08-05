#!/usr/bin/env bash
#
# register-proteos-admin-role.sh — register the ProteOS admin role in Zitadel.
#
# Creates (idempotently):
#
#   role  proteos.admin   on the SHARED Tavon.io project
#
# and, with --grant, assigns it to a named user.
#
# Role model: ProteOS has exactly ONE elevated level. A user with no grant is an
# ordinary user, which is the overwhelmingly common case — an ordinary signup
# needs no provisioning here at all. Holding proteos.admin unlocks the read-only
# fleet console (GET /api/admin/*) and nothing else; it confers no ability to
# touch another user's machines.
#
# The key is namespaced because the Tavon.io project is SHARED with databox,
# chat and harness, whose roles (databox.admin, databox.manager, internal, beta,
# sessions.read_all) land in the very same token claim. The control plane
# matches this key exactly — never by prefix — so another app's role can never
# confer ProteOS privilege. See controlplane/internal/auth/roles.go.
#
# projectRoleAssertion is load-bearing: without it Zitadel omits
# urn:zitadel:iam:org:project:roles from the userinfo response, and the control
# plane has no role to read. It is ADDITIVE — it only adds a claim — so enabling
# it is safe for the other apps on this project. The script preserves every
# other project setting exactly as an operator left it, and never touches
# projectRoleCheck or hasProjectCheck: both REFUSE tokens for users without a
# grant, which would lock out every ordinary ProteOS user and every chat and
# harness user besides.
#
# Requirements: curl, jq.
#
# Auth: a personal access token of a Zitadel service user with ORG_OWNER.
#
# Usage:
#   ZITADEL_PAT=xxx ./scripts/zitadel/register-proteos-admin-role.sh
#   ZITADEL_PAT=xxx ./scripts/zitadel/register-proteos-admin-role.sh --grant ivan@example.com
#   ZITADEL_PAT=xxx ./scripts/zitadel/register-proteos-admin-role.sh --revoke ivan@example.com
#   ZITADEL_PAT=xxx ./scripts/zitadel/register-proteos-admin-role.sh --list
#
# Overridable via environment:
#   ZITADEL_DOMAIN   Zitadel instance domain   (default: auth.tavon.io)
#   PROJECT_NAME     Zitadel project           (default: Tavon.io)
#   ROLE_KEY         role to manage            (default: proteos.admin)
#
# NOTE: a grant takes effect on the user's NEXT ProteOS sign-in. ProteOS
# sessions are opaque and server-side, so the role is read from the IdP once, at
# login, and mirrored onto the user row. Tell the person to sign out and back in.
set -euo pipefail

ZITADEL_DOMAIN="${ZITADEL_DOMAIN:-auth.tavon.io}"
ZITADEL_PAT="${ZITADEL_PAT:?set ZITADEL_PAT to a service-user personal access token}"
PROJECT_NAME="${PROJECT_NAME:-Tavon.io}"
ROLE_KEY="${ROLE_KEY:-proteos.admin}"
ROLE_DISPLAY="ProteOS Admin"

ACTION=""
TARGET=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --grant)  ACTION=grant;  TARGET="${2:?--grant needs a username or email}"; shift 2 ;;
    --revoke) ACTION=revoke; TARGET="${2:?--revoke needs a username or email}"; shift 2 ;;
    --list)   ACTION=list;   shift ;;
    -h|--help) sed -n '2,50p' "$0"; exit 0 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

api() {
  local method=$1 path=$2 body=${3:-}
  local -a args=(-H "Content-Type: application/json")
  [[ -n "$body" ]] && args+=(--data "$body")
  local out status resp
  # -w appends the status on its own line. `curl -f` would be shorter but throws
  # away the response body — and Zitadel puts the reason a call was refused
  # (which permission was missing) in exactly that body.
  # The Authorization header is fed via stdin (-H @-) so the PAT never appears
  # in the process list on shared hosts.
  out=$(curl -sS -w $'\n%{http_code}' -X "$method" "https://${ZITADEL_DOMAIN}${path}" \
    -H @- "${args[@]}" <<< "Authorization: Bearer ${ZITADEL_PAT}") || return 1
  status=${out##*$'\n'}
  resp=${out%$'\n'*}
  if [[ $status != 2* ]]; then
    printf '\n%s %s -> HTTP %s\n%s\n\n' "$method" "$path" "$status" "$resp" >&2
    return 1
  fi
  printf '%s' "$resp"
}

# --- project -----------------------------------------------------------------

project_query=$(jq -n --arg name "$PROJECT_NAME" \
  '{queries: [{nameQuery: {name: $name, method: "TEXT_QUERY_METHOD_EQUALS"}}]}')
project_id=$(api POST /management/v1/projects/_search "$project_query" \
  | jq -r '.result[0].id // empty')

if [[ -z "$project_id" ]]; then
  echo "project '${PROJECT_NAME}' not found." >&2
  echo "This script only ADDS a role to the existing shared project; it does not create it." >&2
  echo "Register the project and the ProteOS app first, then re-run." >&2
  exit 1
fi
echo "project '${PROJECT_NAME}' (id ${project_id})"

# Re-send the project's current settings with only projectRoleAssertion forced
# on, so the shared project keeps every other flag as the operator left it.
current=$(api GET "/management/v1/projects/${project_id}" | jq '.project')
if [[ "$(jq -r '.projectRoleAssertion // false' <<< "$current")" == "true" ]]; then
  echo "role assertion already on"
else
  api PUT "/management/v1/projects/${project_id}" \
    "$(jq '{name, projectRoleCheck, hasProjectCheck, privateLabelingSetting} + {projectRoleAssertion: true}' <<< "$current")" > /dev/null
  echo "enabled role assertion (other project settings preserved)"
fi

# --- role --------------------------------------------------------------------

existing_roles=$(api POST "/management/v1/projects/${project_id}/roles/_search" '{}' \
  | jq -r '.result[]?.key')

if grep -qx "$ROLE_KEY" <<< "$existing_roles"; then
  echo "role '${ROLE_KEY}' already exists"
else
  api POST "/management/v1/projects/${project_id}/roles" \
    "$(jq -n --arg k "$ROLE_KEY" --arg d "$ROLE_DISPLAY" \
      '{roleKey: $k, displayName: $d, group: "proteos"}')" > /dev/null
  echo "created role '${ROLE_KEY}'"
fi

[[ -z "$ACTION" ]] && exit 0

# --- grants ------------------------------------------------------------------

# find_user resolves a login name or email to a Zitadel user id. It refuses on
# ambiguity rather than picking one: granting fleet-wide visibility to the wrong
# account is not a mistake worth being convenient about.
find_user() {
  local ident=$1 result count
  result=$(api POST /management/v1/users/_search "$(jq -n --arg q "$ident" '{
    queries: [{orQuery: {queries: [
      {userNameQuery: {userName: $q, method: "TEXT_QUERY_METHOD_EQUALS_IGNORE_CASE"}},
      {emailQuery:    {emailAddress: $q, method: "TEXT_QUERY_METHOD_EQUALS_IGNORE_CASE"}}
    ]}}]
  }')")
  count=$(jq -r '.result | length' <<< "$result")
  if [[ "$count" == "0" ]]; then
    echo "no Zitadel user matches '${ident}'" >&2
    return 1
  fi
  if [[ "$count" != "1" ]]; then
    echo "'${ident}' matches ${count} users — refusing to guess. Use the exact username." >&2
    jq -r '.result[] | "  \(.userName)  <\(.human.email.email // "no email")>"' <<< "$result" >&2
    return 1
  fi
  jq -r '.result[0].id' <<< "$result"
}

# user_grant_id returns the id of the user's existing grant on this project, if
# any. Zitadel models a grant as one object per (user, project) carrying a LIST
# of role keys, so adding a role means updating that object, not creating a
# second one — and a blind create would clobber roles from the other apps.
user_grant_id() {
  local user_id=$1
  api POST /management/v1/users/grants/_search "$(jq -n --arg u "$user_id" --arg p "$project_id" '{
    queries: [{userIdQuery: {userId: $u}}, {projectIdQuery: {projectId: $p}}]
  }')" | jq -r '.result[0].id // empty'
}

user_grant_roles() {
  local user_id=$1
  api POST /management/v1/users/grants/_search "$(jq -n --arg u "$user_id" --arg p "$project_id" '{
    queries: [{userIdQuery: {userId: $u}}, {projectIdQuery: {projectId: $p}}]
  }')" | jq -r '.result[0].roleKeys // [] | .[]'
}

case "$ACTION" in
  list)
    echo
    echo "holders of ${ROLE_KEY}:"
    api POST /management/v1/users/grants/_search "$(jq -n --arg p "$project_id" --arg r "$ROLE_KEY" '{
      queries: [{projectIdQuery: {projectId: $p}}, {roleKeyQuery: {roleKey: $r}}]
    }')" | jq -r '.result[]? | "  \(.userName)  <\(.email // "no email")>"' || true
    ;;

  grant)
    user_id=$(find_user "$TARGET")
    grant_id=$(user_grant_id "$user_id")
    # Union with what they already hold: the grant object is shared with the
    # other apps on this project, so replacing the list instead of extending it
    # would silently revoke their databox/chat/harness roles.
    roles=$(user_grant_roles "$user_id" | { grep -v "^${ROLE_KEY}$" || true; })
    payload=$(jq -n --arg p "$project_id" --arg r "$ROLE_KEY" --arg existing "$roles" '{
      projectId: $p,
      roleKeys: (($existing | split("\n") | map(select(length > 0))) + [$r] | unique)
    }')
    if [[ -z "$grant_id" ]]; then
      api POST "/management/v1/users/${user_id}/grants" "$payload" > /dev/null
      echo "granted ${ROLE_KEY} to ${TARGET}"
    else
      api PUT "/management/v1/users/${user_id}/grants/${grant_id}" \
        "$(jq '{roleKeys}' <<< "$payload")" > /dev/null
      echo "added ${ROLE_KEY} to ${TARGET}'s existing grant"
    fi
    echo "NOTE: takes effect on their next ProteOS sign-in (sign out, sign back in)."
    ;;

  revoke)
    user_id=$(find_user "$TARGET")
    grant_id=$(user_grant_id "$user_id")
    if [[ -z "$grant_id" ]]; then
      echo "${TARGET} holds no grant on '${PROJECT_NAME}' — nothing to revoke"
      exit 0
    fi
    # Drop only our key. If nothing else remains the grant object goes too,
    # rather than being left as an empty husk.
    remaining=$(user_grant_roles "$user_id" | { grep -v "^${ROLE_KEY}$" || true; })
    if [[ -z "$remaining" ]]; then
      api DELETE "/management/v1/users/${user_id}/grants/${grant_id}" > /dev/null
      echo "revoked ${ROLE_KEY} from ${TARGET} (grant removed — no other roles held)"
    else
      api PUT "/management/v1/users/${user_id}/grants/${grant_id}" \
        "$(jq -n --arg existing "$remaining" \
          '{roleKeys: ($existing | split("\n") | map(select(length > 0)))}')" > /dev/null
      echo "revoked ${ROLE_KEY} from ${TARGET} (other roles preserved)"
    fi
    echo "NOTE: takes effect on their next ProteOS sign-in."
    ;;
esac
