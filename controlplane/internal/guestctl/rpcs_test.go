package guestctl_test

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	guestwire "github.com/tavon-ai/proteos/guestagent/api"

	"github.com/tavon-ai/proteos/controlplane/internal/audit"
	"github.com/tavon-ai/proteos/controlplane/internal/github"
	"github.com/tavon-ai/proteos/controlplane/internal/guestctl"
	"github.com/tavon-ai/proteos/controlplane/internal/machine"
	"github.com/tavon-ai/proteos/controlplane/internal/secrets"
	"github.com/tavon-ai/proteos/controlplane/internal/store"
	"github.com/tavon-ai/proteos/controlplane/internal/taskevents"
	"github.com/tavon-ai/proteos/controlplane/internal/testutil"
)

// ─── ErrNoChannel coverage ───────────────────────────────────────────────────

// unknownMachineID is a well-formed UUID that is never registered in the manager.
const unknownMachineID = "00000000-0000-0000-0000-000000000000"

// TestRPC_ErrNoChannel_Methods proves every RPC surface returns ErrNoChannel
// when there is no live channel for the requested machine.
func TestRPC_ErrNoChannel_Methods(t *testing.T) {
	mgr, _, _, _, _, _ := setupManager(t)
	ctx := context.Background()
	mid := unknownMachineID

	if _, err := mgr.ListProjects(ctx, mid); err != guestctl.ErrNoChannel {
		t.Errorf("ListProjects: want ErrNoChannel, got %v", err)
	}
	if _, err := mgr.KVGet(ctx, mid, "k"); err != guestctl.ErrNoChannel {
		t.Errorf("KVGet: want ErrNoChannel, got %v", err)
	}
	if err := mgr.KVSet(ctx, mid, "k", "v"); err != guestctl.ErrNoChannel {
		t.Errorf("KVSet: want ErrNoChannel, got %v", err)
	}
	if err := mgr.Push(ctx, mid, "/workspace/repo", "main", false, "op-1"); err != guestctl.ErrNoChannel {
		t.Errorf("Push: want ErrNoChannel, got %v", err)
	}
	if err := mgr.RunAgent(ctx, mid, "task-1", "/workspace/repo", "prompt", "claude", ""); err != guestctl.ErrNoChannel {
		t.Errorf("RunAgent: want ErrNoChannel, got %v", err)
	}
	if _, err := mgr.GitStatus(ctx, mid, "/workspace/repo"); err != guestctl.ErrNoChannel {
		t.Errorf("GitStatus: want ErrNoChannel, got %v", err)
	}
	if _, err := mgr.GitDiff(ctx, mid, "/workspace/repo", false); err != guestctl.ErrNoChannel {
		t.Errorf("GitDiff: want ErrNoChannel, got %v", err)
	}
	if _, err := mgr.GitBranch(ctx, mid, "/workspace/repo", "feat", true, ""); err != guestctl.ErrNoChannel {
		t.Errorf("GitBranch: want ErrNoChannel, got %v", err)
	}
	if _, err := mgr.GitCommit(ctx, mid, "/workspace/repo", "msg", nil); err != guestctl.ErrNoChannel {
		t.Errorf("GitCommit: want ErrNoChannel, got %v", err)
	}
}

// ─── rpcFakeGuest – extended fake for live-channel RPC tests ─────────────────

// rpcFakeGuest is a self-contained stand-in for the guest /control endpoint
// that handles the ops the existing fakeGuest (in manager_test.go) does not:
// projects.list, kv.get, kv.set, git.push, agent.run, git.status, git.diff.
// It also handles git.configure and claude.configure so the Manager's configure
// step succeeds and the channel is fully established.
type rpcFakeGuest struct {
	projects []guestwire.Project
	kvData   map[string]string // server-side kv store

	// recorded payloads written by the test handler
	lastPush   *guestwire.GitPushPayload
	lastRun    *guestwire.AgentRunPayload
	lastKVGet  *guestwire.KVGetPayload
	lastKVSet  *guestwire.KVSetPayload
	lastStatus *guestwire.GitStatusPayload
	lastDiff   *guestwire.GitDiffPayload
	lastBranch *guestwire.GitBranchPayload
	lastCommit *guestwire.GitCommitPayload

	conn *websocket.Conn
}

func newRPCFakeGuest() *rpcFakeGuest {
	return &rpcFakeGuest{kvData: map[string]string{}}
}

func (g *rpcFakeGuest) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(guestwire.RouteControl, func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		c.SetReadLimit(1 << 20)
		g.conn = c
		for {
			typ, data, err := c.Read(r.Context())
			if err != nil {
				return
			}
			if typ != websocket.MessageText {
				continue
			}
			var f guestwire.ControlFrame
			if json.Unmarshal(data, &f) != nil || f.Kind != guestwire.ControlReq {
				continue
			}
			g.dispatch(r.Context(), c, f)
		}
	})
	return mux
}

func (g *rpcFakeGuest) dispatch(ctx context.Context, c *websocket.Conn, f guestwire.ControlFrame) {
	reply := func(payload any) {
		var raw json.RawMessage
		if payload != nil {
			raw, _ = json.Marshal(payload)
		}
		b, _ := json.Marshal(guestwire.ControlFrame{ID: f.ID, Kind: guestwire.ControlResp, Payload: raw})
		_ = c.Write(ctx, websocket.MessageText, b)
	}

	switch f.Op {
	// Required for the Manager's configure step on every connect.
	case guestwire.OpGitConfigure:
		reply(nil)
	case guestwire.OpClaudeConfigure:
		reply(nil)

	case guestwire.OpProjectsList:
		reply(guestwire.ProjectsListResponse{Projects: g.projects})

	case guestwire.OpKVGet:
		var p guestwire.KVGetPayload
		_ = json.Unmarshal(f.Payload, &p)
		g.lastKVGet = &p
		var v *string
		if val, ok := g.kvData[p.Key]; ok {
			v = &val
		}
		reply(guestwire.KVGetResponse{Value: v})

	case guestwire.OpKVSet:
		var p guestwire.KVSetPayload
		_ = json.Unmarshal(f.Payload, &p)
		g.lastKVSet = &p
		g.kvData[p.Key] = p.Value
		reply(guestwire.KVSetResponse{})

	case guestwire.OpGitPush:
		var p guestwire.GitPushPayload
		_ = json.Unmarshal(f.Payload, &p)
		g.lastPush = &p
		reply(nil)

	case guestwire.OpAgentRun:
		var p guestwire.AgentRunPayload
		_ = json.Unmarshal(f.Payload, &p)
		g.lastRun = &p
		reply(nil)

	case guestwire.OpGitStatus:
		var p guestwire.GitStatusPayload
		_ = json.Unmarshal(f.Payload, &p)
		g.lastStatus = &p
		reply(guestwire.GitStatusResponse{Branch: "main"})

	case guestwire.OpGitDiff:
		var p guestwire.GitDiffPayload
		_ = json.Unmarshal(f.Payload, &p)
		g.lastDiff = &p
		reply(guestwire.GitDiffResponse{Diff: "--- a/x\n+++ b/x\n"})

	case guestwire.OpGitBranch:
		var p guestwire.GitBranchPayload
		_ = json.Unmarshal(f.Payload, &p)
		g.lastBranch = &p
		reply(guestwire.GitBranchResponse{Branch: p.Name})

	case guestwire.OpGitCommit:
		var p guestwire.GitCommitPayload
		_ = json.Unmarshal(f.Payload, &p)
		g.lastCommit = &p
		reply(guestwire.GitCommitResponse{Sha: "abc1234", Subject: "fix: typo"})

	default:
		b, _ := json.Marshal(guestwire.ControlFrame{
			ID: f.ID, Kind: guestwire.ControlErr,
			Payload: mustMarshal(guestwire.ControlErrorPayload{Code: guestwire.ErrCodeUnavailable, Message: "unknown op"}),
		})
		_ = c.Write(ctx, websocket.MessageText, b)
	}
}

// rpcDialer dials the rpcFakeGuest's TCP listener.
type rpcDialer struct{ addr string }

func (d rpcDialer) DialGuest(ctx context.Context, _ string, _ uint32) (net.Conn, error) {
	var nd net.Dialer
	return nd.DialContext(ctx, "tcp", d.addr)
}

// setupRPCChannel provisions a DB, wires a Manager to a rpcFakeGuest, and
// waits until the control channel is established. Returns the running manager,
// the fake guest, and the machine UUID.
func setupRPCChannel(t *testing.T) (*guestctl.Manager, *rpcFakeGuest, string) {
	t.Helper()
	fg := newRPCFakeGuest()
	ts := httptest.NewServer(fg.handler())
	t.Cleanup(ts.Close)
	addr := strings.TrimPrefix(ts.URL, "http://")

	_, q := testutil.Postgres(t)
	ctx := context.Background()

	host, err := q.UpsertHostByName(ctx, store.UpsertHostByNameParams{Name: "local", AgentUrl: "http://127.0.0.1:9090"})
	if err != nil {
		t.Fatalf("host: %v", err)
	}
	user, err := q.UpsertUser(ctx, store.UpsertUserParams{GithubUserID: 55, Login: "rpc-user", Email: "rpc@example.com"})
	if err != nil {
		t.Fatalf("user: %v", err)
	}
	mc, err := q.CreateMachine(ctx, store.CreateMachineParams{
		UserID: user.ID, HostID: host.ID, KernelRef: "k", RootfsRef: "r", ResourceSpec: []byte(`{}`),
	})
	if err != nil {
		t.Fatalf("machine: %v", err)
	}
	mc.State = string(machine.StateRunning)

	sec, err := secrets.NewFileStore(t.TempDir() + "/s.json")
	if err != nil {
		t.Fatalf("filestore: %v", err)
	}
	uid := uuidStrT(user.ID)
	if err := sec.Put(secrets.UserGitHubPath(uid), map[string]string{
		"access_token":            "gho_rpc",
		"refresh_token":           "ghr_rpc",
		"access_token_expires_at": time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("seed secret: %v", err)
	}
	meta, _ := json.Marshal(map[string]any{"revoked": false})
	if _, err := q.UpsertGitHubLink(ctx, store.UpsertGitHubLinkParams{
		UserID: user.ID, Metadata: meta, SecretRef: secrets.UserGitHubPath(uid),
	}); err != nil {
		t.Fatalf("seed link: %v", err)
	}

	gh := github.NewClient(github.Config{ClientID: "id", ClientSecret: "s"})
	tokens := github.NewTokenSource(gh, q, sec)
	broker := machine.NewBroker()
	hub := taskevents.New(taskevents.DefaultBufferSize, taskevents.DefaultRetention)
	mgr := guestctl.New(rpcDialer{addr: addr}, broker, q, tokens, audit.NewRecorder(q), hub, "github.com", nil, nil)

	runCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go mgr.Run(runCtx)

	machineID := machine.UUIDString(mc.ID)
	go func() {
		for range 60 {
			if mgr.HasChannel(machineID) {
				return
			}
			broker.Publish(machine.Update{Machine: mc})
			time.Sleep(50 * time.Millisecond)
		}
	}()
	waitChannel(t, mgr, machineID)

	return mgr, fg, machineID
}

// ─── Live-channel RPC tests ───────────────────────────────────────────────────

func TestListProjects_LiveChannel(t *testing.T) {
	mgr, fg, machineID := setupRPCChannel(t)
	fg.projects = []guestwire.Project{
		{Name: "alpha", Path: "/workspace/alpha", Branch: "main"},
		{Name: "beta", Path: "/workspace/beta", Branch: "dev", Dirty: true},
	}

	projects, err := mgr.ListProjects(context.Background(), machineID)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	if len(projects) != 2 || projects[0].Name != "alpha" || !projects[1].Dirty {
		t.Fatalf("unexpected projects: %+v", projects)
	}
}

func TestKVGetSet_LiveChannel(t *testing.T) {
	mgr, fg, machineID := setupRPCChannel(t)
	ctx := context.Background()

	// Unset key → nil value, no error.
	v, err := mgr.KVGet(ctx, machineID, "layout")
	if err != nil {
		t.Fatalf("KVGet (unset): %v", err)
	}
	if v != nil {
		t.Fatalf("KVGet unset: want nil, got %q", *v)
	}
	if fg.lastKVGet == nil || fg.lastKVGet.Key != "layout" {
		t.Fatalf("KVGet payload not recorded: %+v", fg.lastKVGet)
	}

	// KVSet → guest stores it in its in-memory map.
	if err := mgr.KVSet(ctx, machineID, "layout", `{"cols":3}`); err != nil {
		t.Fatalf("KVSet: %v", err)
	}
	if fg.lastKVSet == nil || fg.lastKVSet.Key != "layout" || fg.lastKVSet.Value != `{"cols":3}` {
		t.Fatalf("KVSet payload not recorded: %+v", fg.lastKVSet)
	}

	// The guest stores the value; KVGet now returns it.
	v, err = mgr.KVGet(ctx, machineID, "layout")
	if err != nil {
		t.Fatalf("KVGet (after set): %v", err)
	}
	if v == nil || *v != `{"cols":3}` {
		t.Fatalf("KVGet round-trip: got %v", v)
	}
}

func TestPush_LiveChannel(t *testing.T) {
	mgr, fg, machineID := setupRPCChannel(t)
	err := mgr.Push(context.Background(), machineID, "/workspace/repo", "feature", true, "push-op-1")
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if fg.lastPush == nil {
		t.Fatal("guest did not receive git.push")
	}
	p := fg.lastPush
	if p.Path != "/workspace/repo" || p.Branch != "feature" || !p.SetUpstream || p.OpID != "push-op-1" {
		t.Fatalf("unexpected push payload: %+v", p)
	}
}

func TestRunAgent_LiveChannel(t *testing.T) {
	mgr, fg, machineID := setupRPCChannel(t)
	err := mgr.RunAgent(context.Background(), machineID, "task-42", "/workspace/repo", "fix it", "claude", "sess-prev")
	if err != nil {
		t.Fatalf("RunAgent: %v", err)
	}
	if fg.lastRun == nil {
		t.Fatal("guest did not receive agent.run")
	}
	p := fg.lastRun
	if p.TaskID != "task-42" || p.Path != "/workspace/repo" || p.Prompt != "fix it" || p.Provider != "claude" || p.SessionID != "sess-prev" {
		t.Fatalf("unexpected agent.run payload: %+v", p)
	}
}

func TestGitStatus_LiveChannel(t *testing.T) {
	mgr, fg, machineID := setupRPCChannel(t)
	resp, err := mgr.GitStatus(context.Background(), machineID, "/workspace/repo")
	if err != nil {
		t.Fatalf("GitStatus: %v", err)
	}
	if resp.Branch != "main" {
		t.Fatalf("branch = %q, want main", resp.Branch)
	}
	if fg.lastStatus == nil || fg.lastStatus.Path != "/workspace/repo" {
		t.Fatalf("status payload not recorded: %+v", fg.lastStatus)
	}
}

func TestGitDiff_LiveChannel(t *testing.T) {
	mgr, fg, machineID := setupRPCChannel(t)
	resp, err := mgr.GitDiff(context.Background(), machineID, "/workspace/repo", true)
	if err != nil {
		t.Fatalf("GitDiff: %v", err)
	}
	if !strings.Contains(resp.Diff, "---") {
		t.Fatalf("diff body unexpected: %q", resp.Diff)
	}
	if fg.lastDiff == nil || fg.lastDiff.Path != "/workspace/repo" || !fg.lastDiff.Staged {
		t.Fatalf("diff payload not recorded: %+v", fg.lastDiff)
	}
}

func TestGitBranch_LiveChannel(t *testing.T) {
	mgr, fg, machineID := setupRPCChannel(t)
	resp, err := mgr.GitBranch(context.Background(), machineID, "/workspace/repo", "feat/new", true, "main")
	if err != nil {
		t.Fatalf("GitBranch: %v", err)
	}
	if resp.Branch != "feat/new" {
		t.Fatalf("branch = %q, want feat/new", resp.Branch)
	}
	if fg.lastBranch == nil || fg.lastBranch.Name != "feat/new" || fg.lastBranch.From != "main" {
		t.Fatalf("branch payload not recorded: %+v", fg.lastBranch)
	}
}

func TestGitCommit_LiveChannel(t *testing.T) {
	mgr, fg, machineID := setupRPCChannel(t)
	resp, err := mgr.GitCommit(context.Background(), machineID, "/workspace/repo", "fix: typo", []string{"README.md"})
	if err != nil {
		t.Fatalf("GitCommit: %v", err)
	}
	if resp.Sha == "" {
		t.Fatal("expected a commit sha in the response")
	}
	if fg.lastCommit == nil || fg.lastCommit.Message != "fix: typo" || len(fg.lastCommit.Paths) != 1 {
		t.Fatalf("commit payload not recorded: %+v", fg.lastCommit)
	}
}
