package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/tavon-ai/proteos/controlplane/internal/audit"
	"github.com/tavon-ai/proteos/controlplane/internal/machine"
)

// networkPolicyView is the API shape of a machine's network policy (TAV-116):
// the body of GET/PUT /api/machines/{id}/network-policy.
type networkPolicyView struct {
	Mode    string   `json:"mode"`
	Domains []string `json:"domains"`
}

func toNetworkPolicyView(p machine.NetworkPolicy) networkPolicyView {
	return networkPolicyView{Mode: p.Mode, Domains: p.Domains}
}

// handleGetNetworkPolicy returns the machine's configured network policy, or
// the allow_all default if none has been set. 404 no_machine for a missing or
// foreign machine.
func (s *Server) handleGetNetworkPolicy(w http.ResponseWriter, r *http.Request) {
	user, ok := userFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	id, ok := parseMachineID(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "no_machine")
		return
	}
	policy, err := s.Machines.NetworkPolicyFor(r.Context(), user.ID, id)
	switch {
	case errors.Is(err, machine.ErrNoMachine):
		writeError(w, http.StatusNotFound, "no_machine")
	case err != nil:
		writeError(w, http.StatusInternalServerError, "internal")
	default:
		writeJSON(w, http.StatusOK, toNetworkPolicyView(policy))
	}
}

// setNetworkPolicyRequest is the body of PUT /api/machines/{id}/network-policy.
type setNetworkPolicyRequest struct {
	Mode    string   `json:"mode"`
	Domains []string `json:"domains"`
}

// handleSetNetworkPolicy validates and saves a machine's network policy. 200
// with the saved policy, 404 no_machine, 400 invalid_network_policy for an
// unknown mode or malformed domain. It takes effect on the machine's next
// (re)boot, not immediately on a running machine (see machine.SetNetworkPolicy).
func (s *Server) handleSetNetworkPolicy(w http.ResponseWriter, r *http.Request) {
	user, ok := userFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	id, ok := parseMachineID(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "no_machine")
		return
	}
	var req setNetworkPolicyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request")
		return
	}
	policy, err := s.Machines.SetNetworkPolicy(r.Context(), user.ID, id, req.Mode, req.Domains)
	switch {
	case errors.Is(err, machine.ErrNoMachine):
		writeError(w, http.StatusNotFound, "no_machine")
	case errors.Is(err, machine.ErrInvalidNetworkPolicy):
		writeError(w, http.StatusBadRequest, "invalid_network_policy")
	case err != nil:
		writeError(w, http.StatusInternalServerError, "internal")
	default:
		uid := uuidString(user.ID)
		s.Audit.Record(r.Context(), audit.Entry{
			UserID:   uid,
			Actor:    audit.UserActor(uid),
			Action:   audit.ActionNetworkPolicySet,
			Target:   machine.UUIDString(id),
			Metadata: map[string]any{"mode": policy.Mode},
		})
		writeJSON(w, http.StatusOK, toNetworkPolicyView(policy))
	}
}

// handleDeleteNetworkPolicy resets a machine to the default policy (allow_all).
// 204 on success (idempotent), 404 no_machine.
func (s *Server) handleDeleteNetworkPolicy(w http.ResponseWriter, r *http.Request) {
	user, ok := userFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	id, ok := parseMachineID(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "no_machine")
		return
	}
	err := s.Machines.DeleteNetworkPolicy(r.Context(), user.ID, id)
	switch {
	case errors.Is(err, machine.ErrNoMachine):
		writeError(w, http.StatusNotFound, "no_machine")
	case err != nil:
		writeError(w, http.StatusInternalServerError, "internal")
	default:
		uid := uuidString(user.ID)
		s.Audit.Record(r.Context(), audit.Entry{
			UserID: uid,
			Actor:  audit.UserActor(uid),
			Action: audit.ActionNetworkPolicyDelete,
			Target: machine.UUIDString(id),
		})
		w.WriteHeader(http.StatusNoContent)
	}
}
