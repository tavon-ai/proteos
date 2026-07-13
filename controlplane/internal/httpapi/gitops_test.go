package httpapi_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tavon-ai/proteos/controlplane/internal/audit"
	"github.com/tavon-ai/proteos/controlplane/internal/auth"
	"github.com/tavon-ai/proteos/controlplane/internal/github"
	"github.com/tavon-ai/proteos/controlplane/internal/guestctl"
	"github.com/tavon-ai/proteos/controlplane/internal/httpapi"
	"github.com/tavon-ai/proteos/controlplane/internal/machine"
	"github.com/tavon-ai/proteos/controlplane/internal/secrets"
	"github.com/tavon-ai/proteos/controlplane/internal/session"
	"github.com/tavon-ai/proteos/controlplane/internal/store"
	"github.com/tavon-ai/proteos/controlplane/internal/testutil"
	guestwire "github.com/tavon-ai/proteos/guestagent/api"
)

// fakeWorktree implements httpapi.GitWorktree with canned projects/status/diff
// and records the path/staged it was asked for.
type fakeWorktree struct {
	projects  []guestwire.Project
	noChan    bool
	status    guestwire.GitStatusResponse
	diff      guestwire.GitDiffResponse
	branch    guestwire.GitBranchResponse
	commit    guestwire.GitCommitResponse
	statusErr error
	diffErr   error
	branchErr error
	commitErr error
	pushErr   error
	runErr    error

	lastStatusPath  string
	lastDiffPath    string
	lastStaged      bool
	lastBranchPath  string
	lastBranchName  string
	lastCheckout    bool
	lastFrom        string
	lastCommitPath  string
	lastMessage     string
	lastPaths       []string
	lastPushPath    string
	lastPushBranch  string
	lastSetUp       bool
	lastPushOpID    string
	lastRunTaskID   string
	lastRunPath     string
	lastRunPrompt   string
	lastRunProvider string
	lastRunSession  string
	lastCancelTask  string
	cancelErr       error
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

func (f *fakeWorktree) GitBranch(_ context.Context, _, repoPath, name string, checkout bool, from string) (guestwire.GitBranchResponse, error) {
	f.lastBranchPath, f.lastBranchName, f.lastCheckout, f.lastFrom = repoPath, name, checkout, from
	if f.branchErr != nil {
		return guestwire.GitBranchResponse{}, f.branchErr
	}
	return f.branch, nil
}

func (f *fakeWorktree) GitCommit(_ context.Context, _, repoPath, message string, paths []string) (guestwire.GitCommitResponse, error) {
	f.lastCommitPath, f.lastMessage, f.lastPaths = repoPath, message, paths
	if f.commitErr != nil {
		return guestwire.GitCommitResponse{}, f.commitErr
	}
	return f.commit, nil
}

func (f *fakeWorktree) Push(_ context.Context, _, repoPath, branch string, setUpstream bool, opID string) error {
	f.lastPushPath, f.lastPushBranch, f.lastSetUp, f.lastPushOpID = repoPath, branch, setUpstream, opID
	return f.pushErr
}

// RunAgent records a headless-run dispatch (AT1). The fake satisfies both
// GitWorktree and TaskChannel so a test can wire one object to both fields.
func (f *fakeWorktree) RunAgent(_ context.Context, _, taskID, repoPath, prompt, provider, sessionID string) error {
	f.lastRunTaskID, f.lastRunPath, f.lastRunPrompt, f.lastRunProvider = taskID, repoPath, prompt, provider
	f.lastRunSession = sessionID
	return f.runErr
}

// CancelAgent records a cancel dispatch (AT3).
func (f *fakeWorktree) CancelAgent(_ context.Context, _, taskID string) error {
	f.lastCancelTask = taskID
	return f.cancelErr
}

type wtFixture struct {
	url   string
	token string
	mid   string
	ch    *fakeWorktree
	q     *store.Queries // set by setupTasks (AT1/AT3 task tests); nil elsewhere
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
		Machines:    machine.NewService(pool, nil, machine.NewBroker(), secrets.NewMemStore(), machine.Spec{}),
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

func (fx wtFixture) post(t *testing.T, path, body string, csrf bool) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, fx.url+path, strings.NewReader(body))
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: fx.token})
	req.Header.Set("Content-Type", "application/json")
	if csrf {
		req.Header.Set("X-Requested-By", "proteos")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
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

func alphaWorktree(branch guestwire.GitBranchResponse, branchErr error) *fakeWorktree {
	return &fakeWorktree{
		projects:  []guestwire.Project{{Name: "alpha", Path: "/workspace/alpha"}},
		branch:    branch,
		branchErr: branchErr,
	}
}

func TestGitBranch_200Checkout(t *testing.T) {
	ch := alphaWorktree(guestwire.GitBranchResponse{Branch: "feature/x"}, nil)
	fx := setupWorktree(t, string(machine.StateRunning), ch)
	resp := fx.post(t, "/api/machines/"+fx.mid+"/git/branch",
		`{"project":"alpha","name":"feature/x","checkout":true}`, true)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body guestwire.GitBranchResponse
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body.Branch != "feature/x" {
		t.Fatalf("branch = %q, want feature/x", body.Branch)
	}
	if ch.lastBranchName != "feature/x" || !ch.lastCheckout || ch.lastBranchPath != "/workspace/alpha" {
		t.Fatalf("guest call = name %q checkout %v path %q", ch.lastBranchName, ch.lastCheckout, ch.lastBranchPath)
	}
}

func TestGitBranch_200CreateOnlyWithFrom(t *testing.T) {
	ch := alphaWorktree(guestwire.GitBranchResponse{Branch: "main"}, nil)
	fx := setupWorktree(t, string(machine.StateRunning), ch)
	resp := fx.post(t, "/api/machines/"+fx.mid+"/git/branch",
		`{"project":"alpha","name":"feature/y","checkout":false,"from":"main"}`, true)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ch.lastCheckout {
		t.Errorf("checkout should be false")
	}
	if ch.lastFrom != "main" {
		t.Errorf("from = %q, want main", ch.lastFrom)
	}
}

func TestGitBranch_400InvalidName(t *testing.T) {
	ch := alphaWorktree(guestwire.GitBranchResponse{}, nil)
	fx := setupWorktree(t, string(machine.StateRunning), ch)
	// A leading '-' is rejected by ValidBranchName before any dispatch.
	resp := fx.post(t, "/api/machines/"+fx.mid+"/git/branch",
		`{"project":"alpha","name":"-bad","checkout":true}`, true)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if code := errorCode(t, resp); code != "invalid_branch_name" {
		t.Fatalf("error = %q, want invalid_branch_name", code)
	}
	if ch.lastBranchName != "" {
		t.Errorf("invalid name should not reach the guest, got %q", ch.lastBranchName)
	}
}

func TestGitBranch_409Exists(t *testing.T) {
	ch := alphaWorktree(guestwire.GitBranchResponse{}, &guestctl.ControlError{Code: guestwire.ErrCodeBranchExists})
	fx := setupWorktree(t, string(machine.StateRunning), ch)
	resp := fx.post(t, "/api/machines/"+fx.mid+"/git/branch",
		`{"project":"alpha","name":"feature/dup","checkout":true}`, true)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	if code := errorCode(t, resp); code != "branch_exists" {
		t.Fatalf("error = %q, want branch_exists", code)
	}
}

func TestGitBranch_RequiresCSRF(t *testing.T) {
	ch := alphaWorktree(guestwire.GitBranchResponse{Branch: "feature/x"}, nil)
	fx := setupWorktree(t, string(machine.StateRunning), ch)
	resp := fx.post(t, "/api/machines/"+fx.mid+"/git/branch",
		`{"project":"alpha","name":"feature/x","checkout":true}`, false)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (missing CSRF header)", resp.StatusCode)
	}
}

func TestGitBranch_400BadProject(t *testing.T) {
	ch := alphaWorktree(guestwire.GitBranchResponse{}, nil)
	fx := setupWorktree(t, string(machine.StateRunning), ch)
	resp := fx.post(t, "/api/machines/"+fx.mid+"/git/branch",
		`{"project":"ghost","name":"feature/x","checkout":true}`, true)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if code := errorCode(t, resp); code != "bad_project" {
		t.Fatalf("error = %q, want bad_project", code)
	}
}

func TestGitBranch_409NotRunning(t *testing.T) {
	ch := alphaWorktree(guestwire.GitBranchResponse{}, nil)
	fx := setupWorktree(t, string(machine.StateStopped), ch)
	resp := fx.post(t, "/api/machines/"+fx.mid+"/git/branch",
		`{"project":"alpha","name":"feature/x","checkout":true}`, true)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
}

func alphaCommitWorktree(commit guestwire.GitCommitResponse, commitErr error) *fakeWorktree {
	return &fakeWorktree{
		projects:  []guestwire.Project{{Name: "alpha", Path: "/workspace/alpha"}},
		commit:    commit,
		commitErr: commitErr,
	}
}

func TestGitCommit_200All(t *testing.T) {
	ch := alphaCommitWorktree(guestwire.GitCommitResponse{Sha: "abc1234", Subject: "my commit"}, nil)
	fx := setupWorktree(t, string(machine.StateRunning), ch)
	resp := fx.post(t, "/api/machines/"+fx.mid+"/git/commit",
		`{"project":"alpha","message":"my commit"}`, true)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body guestwire.GitCommitResponse
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body.Sha != "abc1234" || body.Subject != "my commit" {
		t.Fatalf("unexpected commit body: %+v", body)
	}
	if ch.lastMessage != "my commit" || ch.lastCommitPath != "/workspace/alpha" || len(ch.lastPaths) != 0 {
		t.Fatalf("guest call = msg %q path %q paths %v", ch.lastMessage, ch.lastCommitPath, ch.lastPaths)
	}
}

func TestGitCommit_200Partial(t *testing.T) {
	ch := alphaCommitWorktree(guestwire.GitCommitResponse{Sha: "def5678", Subject: "add a"}, nil)
	fx := setupWorktree(t, string(machine.StateRunning), ch)
	resp := fx.post(t, "/api/machines/"+fx.mid+"/git/commit",
		`{"project":"alpha","message":"add a","paths":["a.txt"]}`, true)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if len(ch.lastPaths) != 1 || ch.lastPaths[0] != "a.txt" {
		t.Fatalf("paths forwarded = %v, want [a.txt]", ch.lastPaths)
	}
}

func TestGitCommit_400EmptyMessage(t *testing.T) {
	ch := alphaCommitWorktree(guestwire.GitCommitResponse{}, nil)
	fx := setupWorktree(t, string(machine.StateRunning), ch)
	resp := fx.post(t, "/api/machines/"+fx.mid+"/git/commit",
		`{"project":"alpha","message":"   "}`, true)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if code := errorCode(t, resp); code != "empty_message" {
		t.Fatalf("error = %q, want empty_message", code)
	}
	if ch.lastMessage != "" {
		t.Errorf("empty message should not reach the guest")
	}
}

func TestGitCommit_409NothingToCommit(t *testing.T) {
	ch := alphaCommitWorktree(guestwire.GitCommitResponse{}, &guestctl.ControlError{Code: guestwire.ErrCodeNothingToCommit})
	fx := setupWorktree(t, string(machine.StateRunning), ch)
	resp := fx.post(t, "/api/machines/"+fx.mid+"/git/commit",
		`{"project":"alpha","message":"noop"}`, true)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	if code := errorCode(t, resp); code != "nothing_to_commit" {
		t.Fatalf("error = %q, want nothing_to_commit", code)
	}
}

func TestGitCommit_RequiresCSRF(t *testing.T) {
	ch := alphaCommitWorktree(guestwire.GitCommitResponse{Sha: "x"}, nil)
	fx := setupWorktree(t, string(machine.StateRunning), ch)
	resp := fx.post(t, "/api/machines/"+fx.mid+"/git/commit",
		`{"project":"alpha","message":"m"}`, false)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (missing CSRF header)", resp.StatusCode)
	}
}

func TestGitCommit_409NotRunning(t *testing.T) {
	ch := alphaCommitWorktree(guestwire.GitCommitResponse{}, nil)
	fx := setupWorktree(t, string(machine.StateStopped), ch)
	resp := fx.post(t, "/api/machines/"+fx.mid+"/git/commit",
		`{"project":"alpha","message":"m"}`, true)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
}

func TestGitPush_202(t *testing.T) {
	ch := &fakeWorktree{projects: []guestwire.Project{{Name: "alpha", Path: "/workspace/alpha"}}}
	fx := setupWorktree(t, string(machine.StateRunning), ch)
	resp := fx.post(t, "/api/machines/"+fx.mid+"/git/push",
		`{"project":"alpha","branch":"feature/x","set_upstream":true}`, true)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}
	var body pushBody
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body.OpID == "" {
		t.Fatal("missing op_id")
	}
	if ch.lastPushBranch != "feature/x" || !ch.lastSetUp || ch.lastPushPath != "/workspace/alpha" {
		t.Fatalf("guest call = branch %q setUpstream %v path %q", ch.lastPushBranch, ch.lastSetUp, ch.lastPushPath)
	}
	if ch.lastPushOpID != body.OpID {
		t.Fatalf("op_id mismatch: dispatched %q, returned %q", ch.lastPushOpID, body.OpID)
	}
}

func TestGitPush_400InvalidBranch(t *testing.T) {
	ch := &fakeWorktree{projects: []guestwire.Project{{Name: "alpha", Path: "/workspace/alpha"}}}
	fx := setupWorktree(t, string(machine.StateRunning), ch)
	resp := fx.post(t, "/api/machines/"+fx.mid+"/git/push",
		`{"project":"alpha","branch":"-bad"}`, true)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if ch.lastPushBranch != "" {
		t.Errorf("invalid branch should not be dispatched")
	}
}

func TestGitPush_RequiresCSRF(t *testing.T) {
	ch := &fakeWorktree{projects: []guestwire.Project{{Name: "alpha", Path: "/workspace/alpha"}}}
	fx := setupWorktree(t, string(machine.StateRunning), ch)
	resp := fx.post(t, "/api/machines/"+fx.mid+"/git/push",
		`{"project":"alpha","branch":"main"}`, false)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (missing CSRF header)", resp.StatusCode)
	}
}

func TestGitPush_409NotRunning(t *testing.T) {
	ch := &fakeWorktree{projects: []guestwire.Project{{Name: "alpha", Path: "/workspace/alpha"}}}
	fx := setupWorktree(t, string(machine.StateStopped), ch)
	resp := fx.post(t, "/api/machines/"+fx.mid+"/git/push",
		`{"project":"alpha","branch":"main"}`, true)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
}

type pushBody struct {
	OpID string `json:"op_id"`
}

// --- GR5: open PR -----------------------------------------------------------

// fakePRServer serves the two GitHub endpoints handleGitPR touches: the repo
// lookup (default branch) and the PR creation (with a caller-controlled outcome).
func fakePRServer(t *testing.T, pullStatus int, pullBody string) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/octocat/hello", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"full_name":"octocat/hello","default_branch":"main"}`))
	})
	mux.HandleFunc("POST /repos/octocat/hello/pulls", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(pullStatus)
		_, _ = w.Write([]byte(pullBody))
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts.URL
}

// setupPR wires a full server for the PR endpoint: a worktree fake with a remote,
// a real GitHub client pointed at ghURL, and a seeded (optionally revoked) token.
func setupPR(t *testing.T, machineState string, revoked bool, ghURL string) wtFixture {
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
	uid := machine.UUIDString(user.ID)

	sec := secrets.NewMemStore()
	if err := sec.Put(secrets.UserGitHubPath(uid), map[string]string{
		"access_token":            "gho_valid",
		"refresh_token":           "ghr_valid",
		"access_token_expires_at": time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("seed secret: %v", err)
	}
	meta, _ := json.Marshal(map[string]any{"revoked": revoked})
	if _, err := q.UpsertGitHubLink(ctx, store.UpsertGitHubLinkParams{UserID: user.ID, Metadata: meta, SecretRef: secrets.UserGitHubPath(uid)}); err != nil {
		t.Fatalf("seed link: %v", err)
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

	gh := github.NewClient(github.Config{ClientID: "id", ClientSecret: "s", APIBaseURL: ghURL})
	ch := &fakeWorktree{projects: []guestwire.Project{
		{Name: "alpha", Path: "/workspace/alpha", Remote: "https://github.com/octocat/hello.git"},
	}}

	srv := &httpapi.Server{
		Sessions:    sessions,
		Queries:     q,
		Broker:      machine.NewBroker(),
		Audit:       audit.NewRecorder(q),
		Machines:    machine.NewService(pool, nil, machine.NewBroker(), sec, machine.Spec{}),
		GitWorktree: ch,
		GitHub:      gh,
		GitHost:     "github.com",
		Tokens:      github.NewTokenSource(gh, q, sec),
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return wtFixture{url: ts.URL, token: token, mid: machine.UUIDString(mc.ID), ch: ch}
}

func TestGitPR_200(t *testing.T) {
	gh := fakePRServer(t, http.StatusCreated, `{"number":7,"html_url":"https://github.com/octocat/hello/pull/7"}`)
	fx := setupPR(t, string(machine.StateRunning), false, gh)
	resp := fx.post(t, "/api/machines/"+fx.mid+"/git/pr",
		`{"project":"alpha","title":"My change","head":"feature/x"}`, true)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body struct {
		PRURL  string `json:"pr_url"`
		Number int    `json:"number"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body.PRURL != "https://github.com/octocat/hello/pull/7" || body.Number != 7 {
		t.Fatalf("unexpected pr body: %+v", body)
	}
}

// A project cloned from a public (non-GitHub) host must be refused before any
// GitHub API call — owner/repo alone would address the wrong repository. The
// fake GitHub server would happily create the PR (returning 200), so a missing
// guard fails this test on status.
func TestGitPR_422UnsupportedHost(t *testing.T) {
	gh := fakePRServer(t, http.StatusCreated, `{"number":7,"html_url":"x"}`)
	fx := setupPR(t, string(machine.StateRunning), false, gh)
	fx.ch.projects = []guestwire.Project{
		{Name: "alpha", Path: "/workspace/alpha", Remote: "https://codeberg.org/octocat/hello.git"},
	}
	resp := fx.post(t, "/api/machines/"+fx.mid+"/git/pr",
		`{"project":"alpha","title":"T","head":"feature/x"}`, true)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", resp.StatusCode)
	}
	if code := errorCode(t, resp); code != "unsupported_host" {
		t.Fatalf("error = %q, want unsupported_host", code)
	}
}

func TestGitPR_422NoCommits(t *testing.T) {
	gh := fakePRServer(t, http.StatusUnprocessableEntity,
		`{"message":"Validation Failed","errors":[{"message":"No commits between main and feature/x"}]}`)
	fx := setupPR(t, string(machine.StateRunning), false, gh)
	resp := fx.post(t, "/api/machines/"+fx.mid+"/git/pr",
		`{"project":"alpha","title":"T","head":"feature/x"}`, true)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", resp.StatusCode)
	}
	if code := errorCode(t, resp); code != "no_commits" {
		t.Fatalf("error = %q, want no_commits", code)
	}
}

func TestGitPR_409Exists(t *testing.T) {
	gh := fakePRServer(t, http.StatusUnprocessableEntity,
		`{"message":"Validation Failed","errors":[{"message":"A pull request already exists for octocat:feature/x."}]}`)
	fx := setupPR(t, string(machine.StateRunning), false, gh)
	resp := fx.post(t, "/api/machines/"+fx.mid+"/git/pr",
		`{"project":"alpha","title":"T","head":"feature/x"}`, true)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	if code := errorCode(t, resp); code != "pr_exists" {
		t.Fatalf("error = %q, want pr_exists", code)
	}
}

func TestGitPR_409Reconnect(t *testing.T) {
	gh := fakePRServer(t, http.StatusCreated, `{"number":1,"html_url":"x"}`)
	fx := setupPR(t, string(machine.StateRunning), true, gh) // revoked grant
	resp := fx.post(t, "/api/machines/"+fx.mid+"/git/pr",
		`{"project":"alpha","title":"T","head":"feature/x"}`, true)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	if code := errorCode(t, resp); code != "reconnect_github" {
		t.Fatalf("error = %q, want reconnect_github", code)
	}
}

func TestGitPR_400MissingTitle(t *testing.T) {
	gh := fakePRServer(t, http.StatusCreated, `{}`)
	fx := setupPR(t, string(machine.StateRunning), false, gh)
	resp := fx.post(t, "/api/machines/"+fx.mid+"/git/pr",
		`{"project":"alpha","head":"feature/x"}`, true)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestGitPR_RequiresCSRF(t *testing.T) {
	gh := fakePRServer(t, http.StatusCreated, `{"number":1,"html_url":"x"}`)
	fx := setupPR(t, string(machine.StateRunning), false, gh)
	resp := fx.post(t, "/api/machines/"+fx.mid+"/git/pr",
		`{"project":"alpha","title":"T","head":"feature/x"}`, false)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (missing CSRF)", resp.StatusCode)
	}
}

func TestGitPR_409NotRunning(t *testing.T) {
	gh := fakePRServer(t, http.StatusCreated, `{"number":1,"html_url":"x"}`)
	fx := setupPR(t, string(machine.StateStopped), false, gh)
	resp := fx.post(t, "/api/machines/"+fx.mid+"/git/pr",
		`{"project":"alpha","title":"T","head":"feature/x"}`, true)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
}
