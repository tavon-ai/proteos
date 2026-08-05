package auth

// RoleKeyAdmin is the Zitadel project role that grants ProteOS administrator
// access — the fleet-wide admin console (GET /api/admin/*), and nothing else.
//
// The key is namespaced because the Zitadel project is SHARED with the suite's
// other applications, whose own roles (databox.admin, databox.manager, internal,
// beta, sessions.read_all) arrive in the very same claim. An unnamespaced
// "admin" would collide; a prefix or substring test against the claim would let
// a future role belonging to another app quietly confer ProteOS privilege.
// Membership is therefore an exact lookup of this one key (oidc.UserInfo.HasRole).
//
// ProteOS deliberately has a single elevated level. Databox needs the
// user < manager < admin hierarchy and models it as such; here, everyone else is
// an ordinary user, so a user carrying no ProteOS role is the normal, expected
// case and needs no provisioning in Zitadel at all.
const RoleKeyAdmin = "proteos.admin"
