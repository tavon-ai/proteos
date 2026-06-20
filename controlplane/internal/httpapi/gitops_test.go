package httpapi_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tavon/proteos/controlplane/internal/audit"
	"github.com/tavon/proteos/controlplane/internal/auth"
	"github.com/tavon/proteos/controlplane/internal/guestctl"
	"github.com/tavon/proteos/controlplane/internal/httpapi"
	"github.com/tavon/proteos/controlplane/internal/machine"
	"github.com/tavon/proteos/controlplane/internal/secrets"
	"github.com/tavon/proteos/controlplane/internal/session"
	"github.com/tavon/proteos/controlplane/internal/store"
	"github.com/tavon/proteos/controlplane/internal/testutil"
	guestwire "github.com/tavon/proteos/guestagent/api"
)

// fakeWorktree implements httpapi.GitWorktree with canned projects/status/diff
// and records the path/staged it was asked for.
type fakeWorktree struct {
	projects  []guestwire.Project
	noChan    bool
	status    guestwire.GitStatusResponse
	diff      guestwire.GitDiffResponse
	statusErr error
	diffErr   error

	lastStatusPath string
	lastDiffPath   string
	lastStaged     bool
}

func (f *fakeWorktree) HasChannel(string) bool { return !f.noChan }

func (f *fakeWorktree) ListProjects(context.Context, string) ([]guestwire.Project, error) {
	if f.noChan {
		return nil, guestctl.ErrNoChannel
	}
	return f.projects, nil
}

func (f *fakeWorktree) GitStatus(_ context.Context, _, repoPath string) (guestwire.GitStatusResponse, error) {
	f.lastStatusPath = repoPath
	if f.statusErr != nil {
		return guestwire.GitStatusResponse{}, f.statusErr
	}
	return f.status, nil
}

func (f *fakeWorktree) GitDiff(_ context.Context, _, repoPath string, staged bool) (guestwire.GitDiffResponse, error) {
	f.lastDiffPath, f.lastStaged = repoPath, staged
	if f.diffErr != nil {
		return guestwire.GitDiffResponse{}, f.diffErr
	}
	return f.diff, nil
}

type wtFixture struct {
	url   string
	token string
	mid   string
	ch    *fakeWorktree
}

func setupWorktree(t *testing.T, machineState string, ch *fakeWorktree) wtFixture {
	t.Helper()
	ctx := context.Background()
	pool, q := testutil.Postgres(t)

	host, err := q.UpsertHostByName(ctx, store.UpsertHostByNameParams{Name: "local", AgentUrl: "http://127.0.0.1:9090"})
	if err != nil {
		t.Fatalf("host: %v", err)
	}
	user, err := q.UpsertUser(ctx, store.UpsertUserParams{GithubUserID: 9, Login: "octocat", Email: "o@example.com"})
	if err != nil {
		t.Fatalf("user: %v", err)
	}
	mc, err := q.CreateMachine(ctx, store.CreateMachineParams{UserID: user.ID, HostID: host.ID, KernelRef: "k", RootfsRef: "r", ResourceSpec: []byte(`{}`)})
	if err != nil {
		t.Fatalf("machine: %v", err)
	}
	if _, err := pool.Exec(ctx, "UPDATE machines SET state=$1 WHERE id=$2", machineState, mc.ID); err != nil {
		t.Fatalf("set state: %v", err)
	}

	sessions := session.NewManager(q, time.Hour)
	token, err := sessions.Create(ctx, user.ID)
	if err != nil {
		t.Fatalf("session: %v", err)
	}

	srv := &httpapi.Server{
		Sessions:    sessions,
		Queries:     q,
		Audit:       audit.NewRecorder(q),
		Machines:    machine.NewService(pool, nil, machine.NewBroker(), secrets.NewMemStore(), host.ID, machine.Spec{}),
		GitWorktree: ch,
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return wtFixture{url: ts.URL, token: token, mid: machine.UUIDString(mc.ID), ch: ch}
}

func (fx wtFixture) get(t *testing.T, path string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, fx.url+path, nil)
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: fx.token})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	return resp
}

func TestGitStatus_200(t *testing.T) {
	ch := &fakeWorktree{
		projects: []guestwire.Project{{Name: "alpha", Path: "/workspace/alpha"}},
		status: guestwire.GitStatusResponse{Branch: "main", Files: []guestwire.GitFileStatus{
			{Path: "README.md", Index: " ", Worktree: "M"},
		}},
	}
	fx := setupWorktree(t, string(machine.StateRunning), ch)
	resp := fx.get(t, "/api/machines/"+fx.mid+"/git/status?project=alpha")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body guestwire.GitStatusResponse
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body.Branch != "main" || len(body.Files) != 1 || body.Files[0].Path != "README.md" {
		t.Fatalf("unexpected status body: %+v", body)
	}
	// The CP resolved the project name to its absolute path for the guest.
	if ch.lastStatusPath != "/workspace/alpha" {
		t.Errorf("guest asked for path %q, want /workspace/alpha", ch.lastStatusPath)
	}
}

func TestGitStatus_400BadProject(t *testing.T) {
	ch := &fakeWorktree{projects: []guestwire.Project{{Name: "alpha", Path: "/workspace/alpha"}}}
	fx := setupWorktree(t, string(machine.StateRunning), ch)
	resp := fx.get(t, "/api/machines/"+fx.mid+"/git/status?project=ghost")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if code := errorCode(t, resp); code != "bad_project" {
		t.Fatalf("error = %q, want bad_project", code)
	}
}

func TestGitStatus_400MissingProject(t *testing.T) {
	ch := &fakeWorktree{projects: []guestwire.Project{{Name: "alpha", Path: "/workspace/alpha"}}}
	fx := setupWorktree(t, string(machine.StateRunning), ch)
	resp := fx.get(t, "/api/machines/"+fx.mid+"/git/status")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestGitStatus_409NotRunning(t *testing.T) {
	ch := &fakeWorktree{projects: []guestwire.Project{{Name: "alpha", Path: "/workspace/alpha"}}}
	fx := setupWorktree(t, string(machine.StateStopped), ch)
	resp := fx.get(t, "/api/machines/"+fx.mid+"/git/status?project=alpha")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	if code := errorCode(t, resp); code != "machine_not_running" {
		t.Fatalf("error = %q, want machine_not_running", code)
	}
}

func TestGitStatus_404UnknownMachine(t *testing.T) {
	ch := &fakeWorktree{}
	fx := setupWorktree(t, string(machine.StateRunning), ch)
	resp := fx.get(t, "/api/machines/11111111-1111-1111-1111-111111111111/git/status?project=alpha")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestGitStatus_502GuestUnreachable(t *testing.T) {
	ch := &fakeWorktree{
		projects:  []guestwire.Project{{Name: "alpha", Path: "/workspace/alpha"}},
		statusErr: fmt.Errorf("guest read failed"),
	}
	fx := setupWorktree(t, string(machine.StateRunning), ch)
	resp := fx.get(t, "/api/machines/"+fx.mid+"/git/status?project=alpha")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
}

func TestGitDiff_200Staged(t *testing.T) {
	ch := &fakeWorktree{
		projects: []guestwire.Project{{Name: "alpha", Path: "/workspace/alpha"}},
		diff:     guestwire.GitDiffResponse{Diff: "diff --git a/README.md b/README.md\n+changed\n"},
	}
	fx := setupWorktree(t, string(machine.StateRunning), ch)
	resp := fx.get(t, "/api/machines/"+fx.mid+"/git/diff?project=alpha&staged=true")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body guestwire.GitDiffResponse
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body.Diff == "" {
		t.Fatalf("empty diff body")
	}
	if !ch.lastStaged {
		t.Errorf("staged=true was not forwarded to the guest")
	}
	if ch.lastDiffPath != "/workspace/alpha" {
		t.Errorf("guest asked for diff path %q, want /workspace/alpha", ch.lastDiffPath)
	}
}

func TestGitDiff_200WorktreeDefault(t *testing.T) {
	ch := &fakeWorktree{
		projects: []guestwire.Project{{Name: "alpha", Path: "/workspace/alpha"}},
		diff:     guestwire.GitDiffResponse{Diff: "x"},
	}
	fx := setupWorktree(t, string(machine.StateRunning), ch)
	resp := fx.get(t, "/api/machines/"+fx.mid+"/git/diff?project=alpha")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ch.lastStaged {
		t.Errorf("staged should default to false")
	}
}
