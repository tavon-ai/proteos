package httpapi

import (
	"net/http"

	"github.com/tavon/proteos/controlplane/internal/machine"
)

// TemplateView is one machine-template as exposed to the SPA. It deliberately
// omits the rootfs/kernel refs (internal build detail that changes on every
// rebake); the stable contract is the id + label + default resources + the
// per-resource override bounds (global, repeated on each entry so the create
// dialog can bound its inputs without a second request).
type TemplateView struct {
	ID          string                 `json:"id"`
	Label       string                 `json:"label"`
	Description string                 `json:"description"`
	Defaults    machine.Resources      `json:"defaults"`
	Limits      machine.ResourceLimits `json:"limits"`
}

// handleListTemplates returns the machine-template catalog for the create-machine
// picker. Auth-only (read). An empty catalog (legacy single-image deployment)
// yields an empty array.
func (s *Server) handleListTemplates(w http.ResponseWriter, r *http.Request) {
	if _, ok := userFromContext(r.Context()); !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	limits := s.Machines.Limits()
	ts := s.Machines.Templates()
	out := make([]TemplateView, 0, len(ts))
	for _, t := range ts {
		out = append(out, TemplateView{
			ID:          t.ID,
			Label:       t.Label,
			Description: t.Description,
			Defaults:    t.Defaults,
			Limits:      limits,
		})
	}
	writeJSON(w, http.StatusOK, out)
}
