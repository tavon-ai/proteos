package httpapi_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tavon/proteos/controlplane/internal/auth"
	"github.com/tavon/proteos/controlplane/internal/httpapi"
	"github.com/tavon/proteos/controlplane/internal/machine"
	"github.com/tavon/proteos/controlplane/internal/secrets"
	"github.com/tavon/proteos/controlplane/internal/session"
	"github.com/tavon/proteos/controlplane/internal/store"
	"github.com/tavon/proteos/controlplane/internal/testutil"
)

// setupTemplatesCP wires a minimal authenticated control plane with a two-entry
// template catalog + resource caps. The node-agent is nil: the only create paths
// exercised here (unknown template, out-of-range resources) fail in resolution
// before any agent call, so no agent is needed.
func setupTemplatesCP(t *testing.T) (url, token string) {
	t.Helper()
	ctx := context.Background()
	pool, q := testutil.Postgres(t)

	user, err := q.UpsertUser(ctx, store.UpsertUserParams{GithubUserID: 55, Login: "tpl"})
	if err != nil {
		t.Fatal(err)
	}
	host, err := q.UpsertHostByName(ctx, store.UpsertHostByNameParams{Name: "local", AgentUrl: "http://x"})
	if err != nil {
		t.Fatal(err)
	}
	sessions := session.NewManager(q, time.Hour)
	tok, err := sessions.Create(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}

	catalog, err := machine.NewCatalog([]machine.Template{
		{ID: "base", Label: "Base", Description: "Platform baseline.", RootfsRef: "rootfs-base.ext4", Defaults: machine.Resources{Vcpus: 2, MemMiB: 2048, DiskMiB: 10240}},
		{ID: "full", Label: "Full stack", RootfsRef: "rootfs-full.ext4", Defaults: machine.Resources{Vcpus: 4, MemMiB: 4096, DiskMiB: 20480}},
	}, "vmlinux-6.1")
	if err != nil {
		t.Fatal(err)
	}
	svc := machine.NewService(pool, nil, machine.NewBroker(), secrets.NewMemStore(), host.ID, machine.Spec{
		Vcpus: 2, MemMiB: 2048, DiskMiB: 10240, KernelRef: "k", RootfsRef: "r",
		Catalog: catalog, Limits: machine.NewResourceLimits(8, 16384, 51200),
	})

	srv := &httpapi.Server{Sessions: sessions, Machines: svc, Broker: machine.NewBroker(), Queries: q}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts.URL, tok
}

func doReq(t *testing.T, method, url, token, body string) *http.Response {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: token})
	if method != http.MethodGet {
		req.Header.Set("X-Requested-By", "proteos")
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestListTemplatesEndpoint(t *testing.T) {
	url, token := setupTemplatesCP(t)
	resp := doReq(t, http.MethodGet, url+"/api/templates", token, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want 200", resp.StatusCode)
	}
	var views []httpapi.TemplateView
	if err := json.NewDecoder(resp.Body).Decode(&views); err != nil {
		t.Fatal(err)
	}
	if len(views) != 2 || views[0].ID != "base" || views[1].ID != "full" {
		t.Fatalf("templates=%+v, want [base full]", views)
	}
	if views[0].Defaults.Vcpus != 2 || views[1].Defaults.DiskMiB != 20480 {
		t.Fatalf("unexpected defaults: %+v", views)
	}
	// Limits are repeated on each entry (global), with the fixed floors + caps.
	if views[0].Limits.Vcpus.Min != 1 || views[0].Limits.Vcpus.Max != 8 || views[0].Limits.MemMiB.Max != 16384 {
		t.Fatalf("unexpected limits: %+v", views[0].Limits)
	}
	// Internal refs must never be exposed.
	if strings.Contains(rawBody(t, url, token), "rootfs") {
		t.Fatal("templates response leaked rootfs/kernel refs")
	}
}

func rawBody(t *testing.T, url, token string) string {
	t.Helper()
	resp := doReq(t, http.MethodGet, url+"/api/templates", token, "")
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

func TestCreateMachine_InvalidResources(t *testing.T) {
	url, token := setupTemplatesCP(t)
	resp := doReq(t, http.MethodPost, url+"/api/machines", token, `{"template_id":"base","vcpus":99}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400", resp.StatusCode)
	}
	var env struct {
		Error  string `json:"error"`
		Detail string `json:"detail"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatal(err)
	}
	if env.Error != "invalid_resources" || env.Detail != "vcpus must be 1..8" {
		t.Fatalf("env=%+v, want invalid_resources / vcpus must be 1..8", env)
	}
}

func TestCreateMachine_UnknownTemplate(t *testing.T) {
	url, token := setupTemplatesCP(t)
	resp := doReq(t, http.MethodPost, url+"/api/machines", token, `{"template_id":"nope"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400", resp.StatusCode)
	}
	var env struct {
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatal(err)
	}
	if env.Error != "unknown_template" {
		t.Fatalf("error=%q, want unknown_template", env.Error)
	}
}
