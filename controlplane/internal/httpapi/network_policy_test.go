package httpapi_test

import (
	"encoding/json"
	"net/http"
	"testing"
)

type networkPolicyView struct {
	Mode    string   `json:"mode"`
	Domains []string `json:"domains"`
}

func decodeNetworkPolicy(t *testing.T, resp *http.Response) networkPolicyView {
	t.Helper()
	defer resp.Body.Close()
	var v networkPolicyView
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		t.Fatalf("decode network policy: %v", err)
	}
	return v
}

func TestGetNetworkPolicy_DefaultAllowAll(t *testing.T) {
	fx := setupMach(t, "running")

	resp := fx.doMach(t, http.MethodGet, "/api/machines/"+fx.mid+"/network-policy", "", true, false)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	v := decodeNetworkPolicy(t, resp)
	if v.Mode != "allow_all" {
		t.Fatalf("mode = %q, want allow_all", v.Mode)
	}
	if len(v.Domains) != 0 {
		t.Fatalf("domains = %v, want empty", v.Domains)
	}
}

func TestGetNetworkPolicy_Unauthenticated(t *testing.T) {
	fx := setupMach(t, "running")
	resp := fx.doMach(t, http.MethodGet, "/api/machines/"+fx.mid+"/network-policy", "", false, false)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

func TestGetNetworkPolicy_UnknownMachine(t *testing.T) {
	fx := setupMach(t, "running")
	resp := fx.doMach(t, http.MethodGet, "/api/machines/00000000-0000-0000-0000-0000000000ff/network-policy", "", true, false)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestSetNetworkPolicy_AllowDomainsRoundTrip(t *testing.T) {
	fx := setupMach(t, "running")

	body := `{"mode":"allow_domains","domains":["github.com","api.github.com"]}`
	resp := fx.doMach(t, http.MethodPut, "/api/machines/"+fx.mid+"/network-policy", body, true, true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200", resp.StatusCode)
	}
	v := decodeNetworkPolicy(t, resp)
	if v.Mode != "allow_domains" || len(v.Domains) != 2 {
		t.Fatalf("PUT response = %+v, want allow_domains with 2 domains", v)
	}

	getResp := fx.doMach(t, http.MethodGet, "/api/machines/"+fx.mid+"/network-policy", "", true, false)
	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("GET status = %d, want 200", getResp.StatusCode)
	}
	got := decodeNetworkPolicy(t, getResp)
	if got.Mode != "allow_domains" || len(got.Domains) != 2 {
		t.Fatalf("read-back policy = %+v, want allow_domains with 2 domains", got)
	}
}

func TestSetNetworkPolicy_InvalidMode(t *testing.T) {
	fx := setupMach(t, "running")
	body := `{"mode":"block_everything"}`
	resp := fx.doMach(t, http.MethodPut, "/api/machines/"+fx.mid+"/network-policy", body, true, true)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestSetNetworkPolicy_InvalidDomain(t *testing.T) {
	fx := setupMach(t, "running")
	body := `{"mode":"deny_domains","domains":["not a domain"]}`
	resp := fx.doMach(t, http.MethodPut, "/api/machines/"+fx.mid+"/network-policy", body, true, true)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestSetNetworkPolicy_RequiresCSRF(t *testing.T) {
	fx := setupMach(t, "running")
	body := `{"mode":"deny_all"}`
	resp := fx.doMach(t, http.MethodPut, "/api/machines/"+fx.mid+"/network-policy", body, true, false)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (missing CSRF header)", resp.StatusCode)
	}
}

func TestDeleteNetworkPolicy_ResetsToDefault(t *testing.T) {
	fx := setupMach(t, "running")

	setResp := fx.doMach(t, http.MethodPut, "/api/machines/"+fx.mid+"/network-policy", `{"mode":"deny_all"}`, true, true)
	setResp.Body.Close()
	if setResp.StatusCode != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200", setResp.StatusCode)
	}

	delResp := fx.doMach(t, http.MethodDelete, "/api/machines/"+fx.mid+"/network-policy", "", true, true)
	delResp.Body.Close()
	if delResp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE status = %d, want 204", delResp.StatusCode)
	}

	getResp := fx.doMach(t, http.MethodGet, "/api/machines/"+fx.mid+"/network-policy", "", true, false)
	got := decodeNetworkPolicy(t, getResp)
	if got.Mode != "allow_all" {
		t.Fatalf("mode after delete = %q, want allow_all", got.Mode)
	}
}

// TestNetworkPolicy_UnownedMachineNotFound checks the handler maps a
// well-formed but inaccessible machine id to 404 rather than leaking whether
// it exists. Cross-user ownership enforcement itself is exercised at the
// service layer (machine.TestNetworkPolicyOwnershipRejected) since
// handleGetNetworkPolicy delegates straight to Service.NetworkPolicyFor.
func TestNetworkPolicy_UnownedMachineNotFound(t *testing.T) {
	fxA := setupMach(t, "running")
	fxB := setupMach(t, "running")

	resp := fxB.doMach(t, http.MethodGet, "/api/machines/"+fxA.mid+"/network-policy", "", true, false)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}
