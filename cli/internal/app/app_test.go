package app_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/tavon-ai/proteos/cli/internal/app"
	"github.com/tavon-ai/proteos/cli/internal/client"
)

// fakeCP is a minimal in-memory control plane for CLI tests. It checks bearer
// auth, serves machines + a single task whose status it can advance, and an SSE
// event stream.
type fakeCP struct {
	mu          sync.Mutex
	token       string
	taskStatus  string // current status the GET endpoint reports
	getCount    int    // number of task GETs (to advance status over polls)
	advanceAt   int    // flip to taskStatus after this many GETs
	finalStatus string
	canceled    bool
	lastSend    string

	projectsPresent bool   // GET /api/projects returns hello-world when true
	cloned          bool   // set by POST /api/git/clone (then projects show up)
	lastCloneName   string // full_name from the last clone request
	lastCommitMsg   string // message from the last git commit request

	lastCreateName     string // name from the last POST /api/machines
	lastCreateTemplate string // template_id from the last POST /api/machines

	machinesFailN int // GET /api/machines returns 500 this many times before succeeding
	createFailN   int // POST /api/machines returns 500 this many times before succeeding

	providerKeySet map[string]bool   // provider key -> whether a key is currently stored
	lastSetKey     string            // provider key from the last PUT /api/secrets/providers/{key}
	lastSetFields  map[string]string // fields from the last PUT /api/secrets/providers/{key}
	lastDeletedKey string            // provider key from the last DELETE /api/secrets/providers/{key}
	lastMergeMethod string // method from the last PR merge request
	lastCommentBody string // body from the last PR comment request
}

func (f *fakeCP) handler() http.Handler {
	mux := http.NewServeMux()

	auth := func(w http.ResponseWriter, r *http.Request) bool {
		if r.Header.Get("Authorization") != "Bearer "+f.token {
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return false
		}
		return true
	}

	mux.HandleFunc("GET /api/me", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		fmt.Fprint(w, `{"user":{"login":"octocat","email":"o@x.com","avatar_url":""},"machines":[]}`)
	})
	mux.HandleFunc("GET /api/machines", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		f.mu.Lock()
		if f.machinesFailN > 0 {
			f.machinesFailN--
			f.mu.Unlock()
			writeErr(w, http.StatusInternalServerError, "boom")
			return
		}
		f.mu.Unlock()
		fmt.Fprint(w, `[{"id":"m1","name":"alpha","state":"running","guest_ip":"10.0.0.2","template_id":"go","created_at":"2026-06-20T00:00:00Z"},{"id":"m2","name":"beta","state":"stopped","guest_ip":null,"template_id":"go","created_at":"2026-06-21T00:00:00Z"}]`)
	})
	mux.HandleFunc("POST /api/machines", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		f.mu.Lock()
		if f.createFailN > 0 {
			f.createFailN--
			f.mu.Unlock()
			writeErr(w, http.StatusInternalServerError, "boom")
			return
		}
		f.mu.Unlock()
		var body struct {
			Name       string `json:"name"`
			TemplateID string `json:"template_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.lastCreateName = body.Name
		f.lastCreateTemplate = body.TemplateID
		f.mu.Unlock()
		name := body.Name
		if name == "" {
			name = "auto"
		}
		w.WriteHeader(http.StatusAccepted)
		fmt.Fprintf(w, `{"id":"m2","name":%q,"state":"provisioning","guest_ip":null,"template_id":%q,"created_at":"2026-06-21T00:00:00Z"}`, name, body.TemplateID)
	})
	mux.HandleFunc("GET /api/providers", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		f.mu.Lock()
		claudeSet := f.providerKeySet["claude"]
		f.mu.Unlock()
		fmt.Fprintf(w, `[`+
			`{"key":"claude","display_name":"Claude Code","enabled":true,"key_set":%t,"secret_fields":[{"name":"api_key","label":"Anthropic API key","env":"ANTHROPIC_API_KEY"}]},`+
			`{"key":"openai","display_name":"OpenAI Codex","enabled":true,"key_set":false,"secret_fields":[{"name":"api_key","label":"OpenAI API key","env":"OPENAI_API_KEY"}]},`+
			`{"key":"multi","display_name":"Multi-field Provider","enabled":true,"key_set":false,"secret_fields":[{"name":"a","label":"A","env":"A"},{"name":"b","label":"B","env":"B"}]}`+
			`]`, claudeSet)
	})
	mux.HandleFunc("PUT /api/secrets/providers/{key}", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		key := r.PathValue("key")
		if key != "claude" && key != "openai" {
			writeErr(w, http.StatusNotFound, "unknown_provider")
			return
		}
		var body struct {
			Fields map[string]string `json:"fields"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		if f.providerKeySet == nil {
			f.providerKeySet = map[string]bool{}
		}
		f.providerKeySet[key] = true
		f.lastSetKey = key
		f.lastSetFields = body.Fields
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("DELETE /api/secrets/providers/{key}", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		key := r.PathValue("key")
		f.mu.Lock()
		if f.providerKeySet == nil {
			f.providerKeySet = map[string]bool{}
		}
		f.providerKeySet[key] = false
		f.lastDeletedKey = key
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/machines/{id}/start", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		w.WriteHeader(http.StatusAccepted)
		fmt.Fprintf(w, `{"id":%q,"name":"alpha","state":"running","guest_ip":null,"template_id":"go","created_at":"2026-06-20T00:00:00Z"}`, r.PathValue("id"))
	})
	mux.HandleFunc("POST /api/machines/{id}/stop", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		w.WriteHeader(http.StatusAccepted)
		fmt.Fprintf(w, `{"id":%q,"name":"alpha","state":"stopped","guest_ip":null,"template_id":"go","created_at":"2026-06-20T00:00:00Z"}`, r.PathValue("id"))
	})
	mux.HandleFunc("GET /api/templates", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		fmt.Fprint(w, `[{"id":"go","label":"Go","description":"Go toolchain"},{"id":"full-stack","label":"Full Stack","description":"Node + Postgres"}]`)
	})
	mux.HandleFunc("GET /api/git/repos", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		fmt.Fprint(w, `{"repos":[{"full_name":"octocat/hello-world","private":false,"default_branch":"main","pushed_at":"2026-06-20T00:00:00Z"}],"grants_url":"https://github.com/apps/x/installations/new"}`)
	})
	mux.HandleFunc("GET /api/projects", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		f.mu.Lock()
		present := f.projectsPresent || f.cloned
		f.mu.Unlock()
		if !present {
			fmt.Fprint(w, `{"projects":[]}`)
			return
		}
		fmt.Fprint(w, `{"projects":[{"name":"hello-world","path":"/workspace/hello-world","remote":"https://github.com/octocat/hello-world.git","branch":"main","dirty":false}]}`)
	})
	mux.HandleFunc("POST /api/git/clone", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		var body struct {
			FullName string `json:"full_name"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.cloned = true
		f.lastCloneName = body.FullName
		f.mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
		fmt.Fprint(w, `{"op_id":"op1"}`)
	})
	mux.HandleFunc("GET /api/machines/{id}/git/status", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		fmt.Fprint(w, `{"branch":"main","files":[{"path":"main.go","index":" ","worktree":"M"}]}`)
	})
	mux.HandleFunc("POST /api/machines/{id}/git/commit", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		var body struct {
			Message string `json:"message"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.lastCommitMsg = body.Message
		f.mu.Unlock()
		fmt.Fprint(w, `{"sha":"abc1234","subject":"`+body.Message+`"}`)
	})
	mux.HandleFunc("GET /api/machines/{id}", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		if r.PathValue("id") != "m1" {
			writeErr(w, http.StatusNotFound, "no_machine")
			return
		}
		fmt.Fprint(w, `{"id":"m1","name":"alpha","state":"running","guest_ip":"10.0.0.2","template_id":"go","created_at":"2026-06-20T00:00:00Z"}`)
	})
	mux.HandleFunc("POST /api/machines/{id}/tasks", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		w.WriteHeader(http.StatusAccepted)
		fmt.Fprint(w, `{"task_id":"t1"}`)
	})
	mux.HandleFunc("GET /api/machines/{id}/tasks", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		f.mu.Lock()
		st := f.taskStatus
		f.mu.Unlock()
		fmt.Fprintf(w, `{"tasks":[{"id":"t1","status":"%s","provider":"claude","project":"alpha","created_at":"2026-06-20T00:00:00Z"}]}`, st)
	})
	mux.HandleFunc("GET /api/machines/{id}/tasks/{tid}", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		f.mu.Lock()
		f.getCount++
		if f.advanceAt > 0 && f.getCount >= f.advanceAt {
			f.taskStatus = f.finalStatus
		}
		st := f.taskStatus
		f.mu.Unlock()
		fmt.Fprintf(w, `{"id":"t1","status":"%s","provider":"claude","project":"alpha","agent_session_id":"sess-1","result_summary":"done it","created_at":"2026-06-20T00:00:00Z"}`, st)
	})
	mux.HandleFunc("POST /api/machines/{id}/tasks/{tid}/cancel", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		f.mu.Lock()
		f.canceled = true
		f.mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
		fmt.Fprint(w, `{"task_id":"t1"}`)
	})
	mux.HandleFunc("POST /api/machines/{id}/tasks/{tid}/messages", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		buf := new(bytes.Buffer)
		_, _ = buf.ReadFrom(r.Body)
		f.mu.Lock()
		f.lastSend = buf.String()
		f.mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
		fmt.Fprint(w, `{"task_id":"t1"}`)
	})
	mux.HandleFunc("GET /api/git/repos/{owner}/{repo}/pulls/{number}", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		fmt.Fprint(w, `{"number":42,"state":"open","title":"Add health check","body":"desc","html_url":"https://github.com/octocat/hello-world/pull/42","head":"fix/login","base":"main","head_sha":"abc123","author":{"login":"octocat","avatar_url":""},"additions":10,"deletions":2,"changed_files":3}`)
	})
	mux.HandleFunc("GET /api/git/repos/{owner}/{repo}/pulls/{number}/files", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		fmt.Fprint(w, `{"files":[{"path":"main.go","status":"M","additions":8,"deletions":2}]}`)
	})
	mux.HandleFunc("GET /api/git/repos/{owner}/{repo}/pulls/{number}/checks", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		fmt.Fprint(w, `{"total":2,"passed":1,"failed":0,"pending":1,"runs":[{"name":"build","status":"completed","conclusion":"success"},{"name":"test","status":"in_progress"}]}`)
	})
	mux.HandleFunc("POST /api/git/repos/{owner}/{repo}/pulls/{number}/merge", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		var body struct {
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.lastMergeMethod = body.Method
		f.mu.Unlock()
		fmt.Fprint(w, `{"merged":true,"sha":"deadbeef"}`)
	})
	mux.HandleFunc("POST /api/git/repos/{owner}/{repo}/pulls/{number}/comments", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		var body struct {
			Body string `json:"body"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.lastCommentBody = body.Body
		f.mu.Unlock()
		fmt.Fprint(w, `{"id":99,"html_url":"https://github.com/octocat/hello-world/pull/42#issuecomment-99"}`)
	})
	mux.HandleFunc("GET /api/machines/{id}/tasks/{tid}/events", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		writeSSE := func(id, data string) {
			fmt.Fprintf(w, "id: %s\nevent: agent\ndata: %s\n\n", id, data)
			if fl != nil {
				fl.Flush()
			}
		}
		writeSSE("1", `{"kind":"assistant_text","text":"hello"}`)
		writeSSE("2", `{"kind":"tool_use","tool":"Bash","tool_id":"x","input":{"cmd":"ls"}}`)
		writeSSE("3", `{"kind":"tool_result","tool_id":"x","output":"file.txt","is_error":false}`)
		writeSSE("4", `{"kind":"result","status":"done","is_error":false,"text":"ok","cost_usd":0.12,"num_turns":2,"duration_ms":1500}`)
	})
	return mux
}

func writeErr(w http.ResponseWriter, status int, code string) {
	w.WriteHeader(status)
	fmt.Fprintf(w, `{"error":%q}`, code)
}

// runCLI executes the CLI in-process with a temp config dir + env auth.
func runCLI(t *testing.T, url, token string, args ...string) (int, string, string) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("PROTEOS_URL", url)
	t.Setenv("PROTEOS_TOKEN", token)
	var out, errb bytes.Buffer
	code := app.Run(app.Env{Stdout: &out, Stderr: &errb, Version: "test"}, args)
	return code, out.String(), errb.String()
}

func newCP(t *testing.T) (*fakeCP, string) {
	t.Helper()
	cp := &fakeCP{token: "tok"}
	ts := httptest.NewServer(cp.handler())
	t.Cleanup(ts.Close)
	return cp, ts.URL
}

func TestMachinesList(t *testing.T) {
	_, url := newCP(t)
	code, out, _ := runCLI(t, url, "tok", "machines", "ls")
	if code != client.ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(out, "m1") || !strings.Contains(out, "alpha") || !strings.Contains(out, "running") {
		t.Fatalf("unexpected table:\n%s", out)
	}
}

func TestMachinesListJSON(t *testing.T) {
	_, url := newCP(t)
	code, out, _ := runCLI(t, url, "tok", "machines", "ls", "--json")
	if code != client.ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(out, `"id": "m1"`) {
		t.Fatalf("expected JSON:\n%s", out)
	}
}

func TestBadTokenExit3(t *testing.T) {
	_, url := newCP(t)
	code, _, errs := runCLI(t, url, "wrong", "machines", "ls")
	if code != client.ExitAuth {
		t.Fatalf("exit = %d, want 3", code)
	}
	if !strings.Contains(errs, "unauthorized") {
		t.Fatalf("expected unauthorized in stderr: %s", errs)
	}
}

func TestMachineNotFoundExit4(t *testing.T) {
	_, url := newCP(t)
	code, _, _ := runCLI(t, url, "tok", "machines", "get", "nope")
	if code != client.ExitNotFound {
		t.Fatalf("exit = %d, want 4", code)
	}
}

func TestMachinesCreate(t *testing.T) {
	cp, url := newCP(t)
	code, out, _ := runCLI(t, url, "tok", "machines", "create", "--template", "go", "--name", "my-box")
	if code != client.ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(out, "m2") || !strings.Contains(out, "created") || !strings.Contains(out, "provisioning") {
		t.Fatalf("unexpected output:\n%s", out)
	}
	if cp.lastCreateTemplate != "go" || cp.lastCreateName != "my-box" {
		t.Fatalf("server got name=%q template=%q", cp.lastCreateName, cp.lastCreateTemplate)
	}
}

func TestMachinesCreateJSON(t *testing.T) {
	_, url := newCP(t)
	code, out, _ := runCLI(t, url, "tok", "machines", "create", "--template", "go", "--json")
	if code != client.ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(out, `"id": "m2"`) || !strings.Contains(out, `"state": "provisioning"`) {
		t.Fatalf("expected JSON summary:\n%s", out)
	}
}

func TestMachinesStart(t *testing.T) {
	_, url := newCP(t)
	code, out, _ := runCLI(t, url, "tok", "machines", "start", "m1")
	if code != client.ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(out, "m1") || !strings.Contains(out, "started") || !strings.Contains(out, "running") {
		t.Fatalf("unexpected output:\n%s", out)
	}
}

func TestMachinesStop(t *testing.T) {
	_, url := newCP(t)
	code, out, _ := runCLI(t, url, "tok", "machines", "stop", "m1")
	if code != client.ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(out, "m1") || !strings.Contains(out, "stopped") {
		t.Fatalf("unexpected output:\n%s", out)
	}
}

func TestMachinesStartRequiresID(t *testing.T) {
	_, url := newCP(t)
	code, _, _ := runCLI(t, url, "tok", "machines", "start")
	if code != client.ExitUsage {
		t.Fatalf("exit = %d, want 2", code)
	}
}

func TestAuthLoginVerifiesAndSaves(t *testing.T) {
	_, url := newCP(t)
	// No env token; pass via flag. Login verifies against /api/me then saves.
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("PROTEOS_URL", "")
	t.Setenv("PROTEOS_TOKEN", "")
	var out, errb bytes.Buffer
	code := app.Run(app.Env{Stdout: &out, Stderr: &errb, Version: "test"},
		[]string{"auth", "login", "--url", url, "--token", "tok"})
	if code != client.ExitOK {
		t.Fatalf("login exit = %d: %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "octocat") {
		t.Fatalf("login output: %s", out.String())
	}
	// A subsequent command works off the stored credentials (no env).
	var out2, errb2 bytes.Buffer
	code = app.Run(app.Env{Stdout: &out2, Stderr: &errb2, Version: "test"}, []string{"machines", "ls"})
	if code != client.ExitOK {
		t.Fatalf("post-login machines ls exit = %d: %s", code, errb2.String())
	}
	if !strings.Contains(out2.String(), "alpha") {
		t.Fatalf("post-login output: %s", out2.String())
	}
}

func TestTaskRunDispatch(t *testing.T) {
	_, url := newCP(t)
	code, out, _ := runCLI(t, url, "tok", "task", "run", "--machine", "m1", "--project", "alpha", "do the thing")
	if code != client.ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(out, "t1 dispatched") {
		t.Fatalf("output: %s", out)
	}
}

func TestTaskRunWaitDone(t *testing.T) {
	cp, url := newCP(t)
	cp.taskStatus = "running"
	cp.advanceAt = 2 // becomes done on the 2nd GET
	cp.finalStatus = "done"
	code, out, _ := runCLI(t, url, "tok", "task", "run", "--machine", "m1", "--project", "alpha", "--wait", "x")
	if code != client.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out, "Status:   done") {
		t.Fatalf("output: %s", out)
	}
}

func TestTaskRunWaitFailedExit5(t *testing.T) {
	cp, url := newCP(t)
	cp.taskStatus = "running"
	cp.advanceAt = 1
	cp.finalStatus = "failed"
	code, _, _ := runCLI(t, url, "tok", "task", "run", "--machine", "m1", "--project", "alpha", "--wait", "x")
	if code != client.ExitTaskFail {
		t.Fatalf("exit = %d, want 5", code)
	}
}

func TestTaskWatchStream(t *testing.T) {
	_, url := newCP(t)
	code, out, _ := runCLI(t, url, "tok", "task", "watch", "--machine", "m1", "t1")
	if code != client.ExitOK {
		t.Fatalf("exit = %d", code)
	}
	for _, want := range []string{"hello", "▸ Bash", "file.txt", "— done"} {
		if !strings.Contains(out, want) {
			t.Fatalf("watch output missing %q:\n%s", want, out)
		}
	}
}

func TestTaskWatchJSON(t *testing.T) {
	_, url := newCP(t)
	code, out, _ := runCLI(t, url, "tok", "task", "watch", "--machine", "m1", "--json", "t1")
	if code != client.ExitOK {
		t.Fatalf("exit = %d", code)
	}
	lines := strings.Count(strings.TrimSpace(out), "\n") + 1
	if lines != 4 {
		t.Fatalf("expected 4 NDJSON lines, got %d:\n%s", lines, out)
	}
	if !strings.Contains(out, `"kind":"result"`) {
		t.Fatalf("missing result line:\n%s", out)
	}
}

func TestTaskCancel(t *testing.T) {
	cp, url := newCP(t)
	code, out, _ := runCLI(t, url, "tok", "task", "cancel", "--machine", "m1", "t1")
	if code != client.ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if !cp.canceled {
		t.Fatal("server never saw cancel")
	}
	if !strings.Contains(out, "cancel requested") {
		t.Fatalf("output: %s", out)
	}
}

func TestTaskSend(t *testing.T) {
	cp, url := newCP(t)
	code, out, _ := runCLI(t, url, "tok", "task", "send", "--machine", "m1", "t1", "also fix tests")
	if code != client.ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(cp.lastSend, "also fix tests") {
		t.Fatalf("server received: %s", cp.lastSend)
	}
	if !strings.Contains(out, "follow-up sent") {
		t.Fatalf("output: %s", out)
	}
}

// TestTaskWatchReconnect drops the stream mid-way on the first connection, then
// expects the CLI to reconnect with Last-Event-ID and resume from event 3 — no
// duplicates, no gaps, terminating on the result frame.
func TestTaskWatchReconnect(t *testing.T) {
	var conns int
	var mu sync.Mutex
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/machines/m1/tasks/t1/events" {
			writeErr(w, http.StatusNotFound, "no")
			return
		}
		if r.Header.Get("Authorization") != "Bearer tok" {
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		mu.Lock()
		conns++
		n := conns
		mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		send := func(id, data string) {
			fmt.Fprintf(w, "id: %s\nevent: agent\ndata: %s\n\n", id, data)
			if fl != nil {
				fl.Flush()
			}
		}
		if n == 1 {
			// First connection: two events then drop (no terminal frame).
			send("1", `{"kind":"assistant_text","text":"first"}`)
			send("2", `{"kind":"tool_use","tool":"Bash","tool_id":"x"}`)
			return
		}
		// Reconnect: client must have sent Last-Event-ID: 2.
		if got := r.Header.Get("Last-Event-ID"); got != "2" {
			t.Errorf("reconnect Last-Event-ID = %q, want 2", got)
		}
		send("3", `{"kind":"tool_result","tool_id":"x","output":"done","is_error":false}`)
		send("4", `{"kind":"result","status":"done","cost_usd":0,"num_turns":1,"duration_ms":10}`)
	}))
	t.Cleanup(ts.Close)

	code, out, _ := runCLI(t, ts.URL, "tok", "task", "watch", "--machine", "m1", "t1")
	if code != client.ExitOK {
		t.Fatalf("exit = %d", code)
	}
	for _, want := range []string{"first", "done", "— done"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q after reconnect:\n%s", want, out)
		}
	}
	if strings.Count(out, "first") != 1 {
		t.Fatalf("event 1 duplicated across reconnect:\n%s", out)
	}
}

// TestTaskWatchOversizedFrameTerminates verifies that a data line exceeding the
// scanner's 1 MB limit stops the stream with an error instead of triggering an
// infinite reconnect loop (the server would replay the same oversized frame on
// every reconnect via Last-Event-ID).
func TestTaskWatchOversizedFrameTerminates(t *testing.T) {
	var mu sync.Mutex
	var conns int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/machines/m1/tasks/t1/events" {
			writeErr(w, http.StatusNotFound, "no")
			return
		}
		if r.Header.Get("Authorization") != "Bearer tok" {
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		mu.Lock()
		conns++
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		// Normal event followed by a data line that exceeds the 1 MB scanner limit.
		fmt.Fprintf(w, "id: 1\nevent: agent\ndata: {\"kind\":\"assistant_text\",\"text\":\"hi\"}\n\n")
		if fl != nil {
			fl.Flush()
		}
		fmt.Fprintf(w, "id: 2\nevent: agent\ndata: %s\n\n", strings.Repeat("x", 1<<20+100))
		if fl != nil {
			fl.Flush()
		}
	}))
	t.Cleanup(ts.Close)

	code, _, _ := runCLI(t, ts.URL, "tok", "task", "watch", "--machine", "m1", "t1")
	if code == client.ExitOK {
		t.Fatal("expected non-zero exit for oversized frame, got 0")
	}
	mu.Lock()
	n := conns
	mu.Unlock()
	if n > 1 {
		t.Fatalf("oversized frame triggered reconnect loop: %d connections (want 1)", n)
	}
}

func TestUsageExit2(t *testing.T) {
	_, url := newCP(t)
	// Missing --machine.
	code, _, _ := runCLI(t, url, "tok", "task", "ls")
	if code != client.ExitUsage {
		t.Fatalf("exit = %d, want 2", code)
	}
}

// runArgs executes the CLI with no auth env (for help-only paths).
func runArgs(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	var out, errb bytes.Buffer
	code := app.Run(app.Env{Stdout: &out, Stderr: &errb, Version: "test"}, args)
	return code, out.String(), errb.String()
}

func TestGroupHelpExit0(t *testing.T) {
	for _, group := range []string{"task", "auth", "machines", "templates", "repo", "project", "git"} {
		for _, form := range [][]string{{group}, {group, "-h"}, {group, "help"}} {
			code, out, _ := runArgs(t, form...)
			if code != client.ExitOK {
				t.Fatalf("%v exit = %d, want 0", form, code)
			}
			if !strings.Contains(out, "proteos "+group) {
				t.Fatalf("%v help missing header:\n%s", form, out)
			}
		}
	}
}

func TestCommandHelpDescribesAndExits0(t *testing.T) {
	cases := []struct {
		args []string
		want string // a phrase the description must contain
	}{
		{[]string{"task", "run", "-h"}, "never commits"},
		{[]string{"task", "watch", "-h"}, "reconnecting automatically"},
		{[]string{"task", "send", "-h"}, "resumes a finished task"},
		{[]string{"task", "cancel", "-h"}, "idempotent"},
		{[]string{"task", "get", "--help"}, "Result fields"},
		{[]string{"task", "ls", "-h"}, "newest first"},
		{[]string{"auth", "login", "-h"}, "credentials.json"},
		{[]string{"machines", "get", "-h"}, "Show one machine"},
	}
	for _, c := range cases {
		code, _, errs := runArgs(t, c.args...)
		if code != client.ExitOK {
			t.Fatalf("%v exit = %d, want 0", c.args, code)
		}
		// -h output goes to stderr (flag convention); content + Examples present.
		if !strings.Contains(errs, c.want) {
			t.Fatalf("%v help missing %q:\n%s", c.args, c.want, errs)
		}
		if !strings.Contains(errs, "Usage:") {
			t.Fatalf("%v help missing Usage section:\n%s", c.args, errs)
		}
	}
}

func TestUnknownSubcommandShowsGroupHelpExit2(t *testing.T) {
	code, _, errs := runArgs(t, "task", "frobnicate")
	if code != client.ExitUsage {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(errs, "unknown task subcommand") || !strings.Contains(errs, "Commands:") {
		t.Fatalf("expected error + group help on stderr:\n%s", errs)
	}
}

// TestVersionReportsBuildIdentity checks `proteos version` prints the stamped
// version, commit, and build date.
func TestVersionReportsBuildIdentity(t *testing.T) {
	for _, args := range [][]string{{"version"}, {"--version"}, {"-v"}} {
		var out, errb bytes.Buffer
		code := app.Run(app.Env{
			Stdout: &out, Stderr: &errb,
			Version: "v1.2.3", Commit: "abc1234", Date: "2026-06-29T12:00:00Z",
		}, args)
		if code != client.ExitOK {
			t.Fatalf("%v exit = %d, want 0", args, code)
		}
		for _, want := range []string{"proteos v1.2.3", "commit: abc1234", "built:  2026-06-29T12:00:00Z"} {
			if !strings.Contains(out.String(), want) {
				t.Fatalf("%v output missing %q:\n%s", args, want, out.String())
			}
		}
	}
}

// TestVersionUnknownWhenUnstamped shows un-stamped build metadata renders as
// "unknown" rather than a blank field.
func TestVersionUnknownWhenUnstamped(t *testing.T) {
	var out, errb bytes.Buffer
	code := app.Run(app.Env{Stdout: &out, Stderr: &errb, Version: "dev"}, []string{"version"})
	if code != client.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "commit: unknown") || !strings.Contains(out.String(), "built:  unknown") {
		t.Fatalf("expected unknown commit/date:\n%s", out.String())
	}
}

// TestHelpJSONIncludesBuildIdentity checks the agent-facing tree carries the
// commit and build date alongside the version.
func TestHelpJSONIncludesBuildIdentity(t *testing.T) {
	var out, errb bytes.Buffer
	code := app.Run(app.Env{
		Stdout: &out, Stderr: &errb,
		Version: "v1.2.3", Commit: "abc1234", Date: "2026-06-29T12:00:00Z",
	}, []string{"--help-json"})
	if code != client.ExitOK {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, errb.String())
	}
	var tree struct {
		Version string `json:"version"`
		Commit  string `json:"commit"`
		Date    string `json:"date"`
	}
	if err := json.Unmarshal(out.Bytes(), &tree); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if tree.Version != "v1.2.3" || tree.Commit != "abc1234" || tree.Date != "2026-06-29T12:00:00Z" {
		t.Fatalf("build identity = %+v", tree)
	}
}

// helpJSONCommand mirrors one command of `proteos --help-json` for tests.
type helpJSONCommand struct {
	Path  string `json:"path"`
	Group string `json:"group"`
	Name  string `json:"name"`
	Flags []struct {
		Name string `json:"name"`
		Type string `json:"type"`
	} `json:"flags"`
}

// helpJSONTree mirrors the shape of `proteos --help-json` for tests.
type helpJSONTree struct {
	Program string `json:"program"`
	Version string `json:"version"`
	Groups  []struct {
		Name     string            `json:"name"`
		Commands []helpJSONCommand `json:"commands"`
	} `json:"groups"`
	Commands []helpJSONCommand `json:"commands"`
}

// TestHelpJSONOffline proves --help-json works with no endpoint, no token, and
// no server — and that it covers exactly the documented command tree.
func TestHelpJSONOffline(t *testing.T) {
	for _, form := range [][]string{{"--help-json"}, {"help-json"}} {
		code, out, errs := runArgs(t, form...)
		if code != client.ExitOK {
			t.Fatalf("%v exit = %d, want 0; stderr=%s", form, code, errs)
		}
		if errs != "" {
			t.Fatalf("%v wrote to stderr (should be silent offline):\n%s", form, errs)
		}
		var tree helpJSONTree
		if err := json.Unmarshal([]byte(out), &tree); err != nil {
			t.Fatalf("%v invalid JSON: %v\n%s", form, err, out)
		}
		if tree.Program != "proteos" || tree.Version != "test" {
			t.Fatalf("program/version = %q/%q, want proteos/test", tree.Program, tree.Version)
		}
		var got []string
		for _, g := range tree.Groups {
			for _, c := range g.Commands {
				if c.Group != g.Name {
					t.Errorf("command %q has group %q, want %q", c.Path, c.Group, g.Name)
				}
				got = append(got, c.Path)
			}
		}
		for _, c := range tree.Commands {
			if c.Group != "" {
				t.Errorf("top-level command %q has group %q, want none", c.Path, c.Group)
			}
			got = append(got, c.Path)
		}
		want := []string{
			"version", "help", "help-json",
			"auth login", "auth status", "auth logout",
			"machines ls", "machines get", "machines create", "machines start", "machines stop",
			"templates ls",
			"repo ls",
			"project ls", "project clone", "project ensure",
			"git status", "git diff", "git branch", "git commit", "git push", "git pr",
			"pr view", "pr files", "pr checks", "pr merge", "pr comment",
			"task run", "task ls", "task get", "task watch", "task cancel", "task send",
			"providers ls", "providers get",
			"secrets set", "secrets unset",
		}
		sort.Strings(got)
		sort.Strings(want)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("command tree drift.\n got: %v\nwant: %v", got, want)
		}
	}
}

// TestHelpJSONMatchesCommandHelp is the anti-drift guard: for every command in
// the JSON tree, the path must be dispatchable (`<path> -h` exits 0) and its
// flag set must match exactly what -h prints — the JSON and -h read the same
// flag definitions, so they can never disagree.
func TestHelpJSONMatchesCommandHelp(t *testing.T) {
	_, out, _ := runArgs(t, "--help-json")
	var tree helpJSONTree
	if err := json.Unmarshal([]byte(out), &tree); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	all := tree.Commands
	for _, g := range tree.Groups {
		all = append(all, g.Commands...)
	}
	for _, c := range all {
		args := append(strings.Fields(c.Path), "-h")
		code, _, errs := runArgs(t, args...)
		if code != client.ExitOK {
			t.Errorf("%q -h exit = %d, want 0 (not dispatchable?)", c.Path, code)
			continue
		}
		got := flagNamesFromHelp(errs)
		var want []string
		for _, f := range c.Flags {
			want = append(want, f.Name)
		}
		sort.Strings(got)
		sort.Strings(want)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%q flags drift between -h and --help-json.\n  -h: %v\njson: %v", c.Path, got, want)
		}
	}
}

// flagNamesFromHelp extracts flag names from the "Flags:" section of -h output.
func flagNamesFromHelp(help string) []string {
	i := strings.Index(help, "\nFlags:\n")
	if i < 0 {
		return nil
	}
	var names []string
	for _, ln := range strings.Split(help[i+len("\nFlags:\n"):], "\n") {
		t := strings.TrimSpace(ln)
		if !strings.HasPrefix(t, "-") {
			continue // continuation lines (flag descriptions) are indented further
		}
		t = strings.TrimPrefix(t, "-")
		if j := strings.IndexAny(t, " \t"); j >= 0 {
			t = t[:j]
		}
		if t != "" {
			names = append(names, t)
		}
	}
	return names
}

func TestMachinesListShowsTemplate(t *testing.T) {
	_, url := newCP(t)
	code, out, _ := runCLI(t, url, "tok", "machines", "ls")
	if code != client.ExitOK {
		t.Fatalf("exit = %d", code)
	}
	// The template id "go" is resolved to its catalog label "Go".
	if !strings.Contains(out, "TEMPLATE") || !strings.Contains(out, "Go") {
		t.Fatalf("expected resolved template label:\n%s", out)
	}
}

func TestTemplatesList(t *testing.T) {
	_, url := newCP(t)
	code, out, _ := runCLI(t, url, "tok", "templates", "ls")
	if code != client.ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(out, "Go") || !strings.Contains(out, "Full Stack") {
		t.Fatalf("templates output:\n%s", out)
	}
}

func TestRepoList(t *testing.T) {
	_, url := newCP(t)
	code, out, _ := runCLI(t, url, "tok", "repo", "ls")
	if code != client.ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(out, "octocat/hello-world") {
		t.Fatalf("repo output:\n%s", out)
	}
}

func TestPRView(t *testing.T) {
	_, url := newCP(t)
	code, out, _ := runCLI(t, url, "tok", "pr", "view", "octocat/hello-world", "42")
	if code != client.ExitOK {
		t.Fatalf("exit = %d: %s", code, out)
	}
	if !strings.Contains(out, "#42") || !strings.Contains(out, "Add health check") {
		t.Fatalf("pr view output:\n%s", out)
	}
}

func TestPRViewJSON(t *testing.T) {
	_, url := newCP(t)
	code, out, _ := runCLI(t, url, "tok", "pr", "view", "--json", "octocat/hello-world", "42")
	if code != client.ExitOK {
		t.Fatalf("exit = %d: %s", code, out)
	}
	var pr struct {
		Number int `json:"number"`
	}
	if err := json.Unmarshal([]byte(out), &pr); err != nil || pr.Number != 42 {
		t.Fatalf("pr view json output:\n%s", out)
	}
}

func TestPRViewBadFullName(t *testing.T) {
	_, url := newCP(t)
	code, _, errOut := runCLI(t, url, "tok", "pr", "view", "badname", "42")
	if code != client.ExitUsage {
		t.Fatalf("exit = %d, want usage; stderr=%s", code, errOut)
	}
}

func TestPRFiles(t *testing.T) {
	_, url := newCP(t)
	code, out, _ := runCLI(t, url, "tok", "pr", "files", "octocat/hello-world", "42")
	if code != client.ExitOK {
		t.Fatalf("exit = %d: %s", code, out)
	}
	if !strings.Contains(out, "main.go") || !strings.Contains(out, "M") {
		t.Fatalf("pr files output:\n%s", out)
	}
}

func TestPRChecks(t *testing.T) {
	_, url := newCP(t)
	code, out, _ := runCLI(t, url, "tok", "pr", "checks", "octocat/hello-world", "42")
	if code != client.ExitOK {
		t.Fatalf("exit = %d: %s", code, out)
	}
	if !strings.Contains(out, "1 passed") || !strings.Contains(out, "build") {
		t.Fatalf("pr checks output:\n%s", out)
	}
}

func TestPRMerge(t *testing.T) {
	cp, url := newCP(t)
	code, out, _ := runCLI(t, url, "tok", "pr", "merge", "--method", "squash", "octocat/hello-world", "42")
	if code != client.ExitOK {
		t.Fatalf("exit = %d: %s", code, out)
	}
	if cp.lastMergeMethod != "squash" {
		t.Fatalf("server saw merge method %q", cp.lastMergeMethod)
	}
	if !strings.Contains(out, "merged") {
		t.Fatalf("pr merge output:\n%s", out)
	}
}

func TestPRComment(t *testing.T) {
	cp, url := newCP(t)
	code, out, _ := runCLI(t, url, "tok", "pr", "comment", "-m", "LGTM", "octocat/hello-world", "42")
	if code != client.ExitOK {
		t.Fatalf("exit = %d: %s", code, out)
	}
	if cp.lastCommentBody != "LGTM" {
		t.Fatalf("server saw comment %q", cp.lastCommentBody)
	}
	if !strings.Contains(out, "commented") {
		t.Fatalf("pr comment output:\n%s", out)
	}
}

func TestPRCommentRequiresBody(t *testing.T) {
	_, url := newCP(t)
	code, _, errOut := runCLI(t, url, "tok", "pr", "comment", "octocat/hello-world", "42")
	if code != client.ExitUsage {
		t.Fatalf("exit = %d, want usage; stderr=%s", code, errOut)
	}
}

func TestProjectListEmpty(t *testing.T) {
	_, url := newCP(t)
	code, out, _ := runCLI(t, url, "tok", "project", "ls", "--machine", "m1")
	if code != client.ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(out, "No projects.") {
		t.Fatalf("project output:\n%s", out)
	}
}

func TestProjectCloneDispatch(t *testing.T) {
	cp, url := newCP(t)
	code, out, _ := runCLI(t, url, "tok", "project", "clone", "--machine", "m1", "octocat/hello-world")
	if code != client.ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if cp.lastCloneName != "octocat/hello-world" {
		t.Fatalf("server saw clone of %q", cp.lastCloneName)
	}
	if !strings.Contains(out, "dispatched") {
		t.Fatalf("clone output:\n%s", out)
	}
}

func TestProjectEnsureAlreadyPresent(t *testing.T) {
	cp, url := newCP(t)
	cp.projectsPresent = true
	code, out, _ := runCLI(t, url, "tok", "project", "ensure", "--machine", "m1", "octocat/hello-world")
	if code != client.ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if cp.lastCloneName != "" {
		t.Fatalf("ensure cloned despite project present: %q", cp.lastCloneName)
	}
	if !strings.Contains(out, "already present") {
		t.Fatalf("ensure output:\n%s", out)
	}
}

func TestProjectEnsureClones(t *testing.T) {
	cp, url := newCP(t)
	// Not present initially; the clone flips the projects list to present, which
	// the ensure poll then observes.
	code, out, _ := runCLI(t, url, "tok", "project", "ensure", "--machine", "m1", "octocat/hello-world")
	if code != client.ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if cp.lastCloneName != "octocat/hello-world" {
		t.Fatalf("ensure did not clone: %q", cp.lastCloneName)
	}
	if !strings.Contains(out, "cloned octocat/hello-world into project hello-world") {
		t.Fatalf("ensure output:\n%s", out)
	}
}

func TestGitStatus(t *testing.T) {
	_, url := newCP(t)
	code, out, _ := runCLI(t, url, "tok", "git", "status", "--machine", "m1", "--project", "hello-world")
	if code != client.ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(out, "On branch main") || !strings.Contains(out, "main.go") {
		t.Fatalf("status output:\n%s", out)
	}
}

func TestGitCommit(t *testing.T) {
	cp, url := newCP(t)
	code, out, _ := runCLI(t, url, "tok", "git", "commit", "--machine", "m1", "--project", "hello-world", "-m", "add health check")
	if code != client.ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if cp.lastCommitMsg != "add health check" {
		t.Fatalf("server saw commit msg %q", cp.lastCommitMsg)
	}
	if !strings.Contains(out, "committed abc1234") {
		t.Fatalf("commit output:\n%s", out)
	}
}

func TestGitCommitMissingMessageExit2(t *testing.T) {
	_, url := newCP(t)
	code, _, _ := runCLI(t, url, "tok", "git", "commit", "--machine", "m1", "--project", "hello-world")
	if code != client.ExitUsage {
		t.Fatalf("exit = %d, want 2", code)
	}
}

// --- Providers / secrets (TAV-39) ---

func TestProvidersList(t *testing.T) {
	_, url := newCP(t)
	code, out, _ := runCLI(t, url, "tok", "providers", "ls")
	if code != client.ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(out, "claude") || !strings.Contains(out, "openai") || !strings.Contains(out, "api_key") {
		t.Fatalf("unexpected table:\n%s", out)
	}
}

func TestProvidersListJSON(t *testing.T) {
	_, url := newCP(t)
	code, out, _ := runCLI(t, url, "tok", "providers", "ls", "--json")
	if code != client.ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if !strings.HasPrefix(strings.TrimSpace(out), "[") {
		t.Fatalf("expected bare JSON array without pagination flags:\n%s", out)
	}
	if !strings.Contains(out, `"key": "claude"`) {
		t.Fatalf("expected JSON:\n%s", out)
	}
}

func TestProvidersGet(t *testing.T) {
	_, url := newCP(t)
	code, out, _ := runCLI(t, url, "tok", "providers", "get", "claude")
	if code != client.ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(out, "Claude Code") || !strings.Contains(out, "Key set: false") {
		t.Fatalf("unexpected output:\n%s", out)
	}
}

func TestProvidersGetUnknownExit4(t *testing.T) {
	_, url := newCP(t)
	code, _, errs := runCLI(t, url, "tok", "providers", "get", "nope")
	if code != client.ExitNotFound {
		t.Fatalf("exit = %d, want 4: %s", code, errs)
	}
}

func TestSecretsSetWithField(t *testing.T) {
	cp, url := newCP(t)
	code, out, _ := runCLI(t, url, "tok", "secrets", "set", "--field", "api_key=sk-ant-test", "claude")
	if code != client.ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if cp.lastSetKey != "claude" || cp.lastSetFields["api_key"] != "sk-ant-test" {
		t.Fatalf("server saw key=%q fields=%v", cp.lastSetKey, cp.lastSetFields)
	}
	if !strings.Contains(out, "claude key set") {
		t.Fatalf("output: %s", out)
	}
}

func TestSecretsSetWithKeyShorthand(t *testing.T) {
	cp, url := newCP(t)
	code, _, _ := runCLI(t, url, "tok", "secrets", "set", "--key", "sk-openai-test", "openai")
	if code != client.ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if cp.lastSetFields["api_key"] != "sk-openai-test" {
		t.Fatalf("server saw fields=%v", cp.lastSetFields)
	}
}

func TestSecretsSetWithStdin(t *testing.T) {
	cp, url := newCP(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("PROTEOS_URL", url)
	t.Setenv("PROTEOS_TOKEN", "tok")
	var out, errb bytes.Buffer
	in := strings.NewReader("sk-ant-from-stdin\n")
	code := app.Run(app.Env{Stdin: in, Stdout: &out, Stderr: &errb, Version: "test"}, []string{"secrets", "set", "--stdin", "claude"})
	if code != client.ExitOK {
		t.Fatalf("exit = %d: %s", code, errb.String())
	}
	if cp.lastSetFields["api_key"] != "sk-ant-from-stdin" {
		t.Fatalf("server saw fields=%v", cp.lastSetFields)
	}
}

func TestSecretsSetKeyShorthandAmbiguousExit2(t *testing.T) {
	_, url := newCP(t)
	code, _, errs := runCLI(t, url, "tok", "secrets", "set", "--key", "x", "multi")
	if code != client.ExitUsage {
		t.Fatalf("exit = %d, want 2: %s", code, errs)
	}
	if !strings.Contains(errs, "declares 2 fields") {
		t.Fatalf("errs: %s", errs)
	}
}

func TestSecretsSetNoFieldsExit2(t *testing.T) {
	_, url := newCP(t)
	code, _, _ := runCLI(t, url, "tok", "secrets", "set", "claude")
	if code != client.ExitUsage {
		t.Fatalf("exit = %d, want 2", code)
	}
}

// TestSecretsSetStdinWithoutInputExit2 covers Env{} built without Stdin (as
// runCLI does): --stdin must fail cleanly rather than panic on a nil Reader.
func TestSecretsSetStdinWithoutInputExit2(t *testing.T) {
	_, url := newCP(t)
	code, _, errs := runCLI(t, url, "tok", "secrets", "set", "--stdin", "claude")
	if code != client.ExitUsage {
		t.Fatalf("exit = %d, want 2: %s", code, errs)
	}
}

func TestSecretsUnset(t *testing.T) {
	cp, url := newCP(t)
	code, out, _ := runCLI(t, url, "tok", "secrets", "unset", "claude")
	if code != client.ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if cp.lastDeletedKey != "claude" {
		t.Fatalf("server saw delete of %q", cp.lastDeletedKey)
	}
	if !strings.Contains(out, "claude key removed") {
		t.Fatalf("output: %s", out)
	}
}

func TestSecretsUnsetJSON(t *testing.T) {
	_, url := newCP(t)
	code, out, _ := runCLI(t, url, "tok", "secrets", "unset", "--json", "claude")
	if code != client.ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(out, `"key_set": false`) {
		t.Fatalf("output: %s", out)
	}
}

// --- Machine-readable errors (TAV-39) ---

func TestErrorJSONFromAPIError(t *testing.T) {
	_, url := newCP(t)
	code, _, errs := runCLI(t, url, "tok", "machines", "get", "nope", "--json")
	if code != client.ExitNotFound {
		t.Fatalf("exit = %d, want 4", code)
	}
	var env struct {
		Error  string `json:"error"`
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal([]byte(errs), &env); err != nil {
		t.Fatalf("stderr not JSON: %v\n%s", err, errs)
	}
	if env.Error != "no_machine" {
		t.Fatalf("error envelope = %+v", env)
	}
}

func TestErrorJSONNoEndpoint(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("PROTEOS_URL", "")
	t.Setenv("PROTEOS_TOKEN", "")
	var out, errb bytes.Buffer
	code := app.Run(app.Env{Stdout: &out, Stderr: &errb, Version: "test"}, []string{"machines", "ls", "--json"})
	if code != client.ExitUsage {
		t.Fatalf("exit = %d, want 2: %s", code, errb.String())
	}
	var env struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(errb.Bytes(), &env); err != nil {
		t.Fatalf("stderr not JSON: %v\n%s", err, errb.String())
	}
	if env.Error != "no_endpoint" {
		t.Fatalf("error envelope = %+v", env)
	}
}

func TestErrorJSONUnknownCommand(t *testing.T) {
	var out, errb bytes.Buffer
	code := app.Run(app.Env{Stdout: &out, Stderr: &errb, Version: "test"}, []string{"bogus", "--json"})
	if code != client.ExitUsage {
		t.Fatalf("exit = %d, want 2", code)
	}
	var env struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(errb.Bytes(), &env); err != nil {
		t.Fatalf("stderr not JSON: %v\n%s", err, errb.String())
	}
	if env.Error != "unknown_command" {
		t.Fatalf("error envelope = %+v", env)
	}
}

func TestErrorProseWithoutJSONFlag(t *testing.T) {
	_, url := newCP(t)
	code, _, errs := runCLI(t, url, "tok", "machines", "get", "nope")
	if code != client.ExitNotFound {
		t.Fatalf("exit = %d, want 4", code)
	}
	if !strings.HasPrefix(errs, "proteos: ") || strings.HasPrefix(strings.TrimPrefix(errs, "proteos: "), "{") {
		t.Fatalf("expected prose stderr, got: %s", errs)
	}
}

// --- Pagination (TAV-39) ---

func TestMachinesListPaginationTable(t *testing.T) {
	_, url := newCP(t)
	code, out, _ := runCLI(t, url, "tok", "machines", "ls", "--limit", "1", "--offset", "1")
	if code != client.ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if strings.Contains(out, "m1") || !strings.Contains(out, "m2") {
		t.Fatalf("expected only the second page, got:\n%s", out)
	}
	if !strings.Contains(out, "Showing 2-2 of 2 machines") {
		t.Fatalf("missing pagination footer:\n%s", out)
	}
}

func TestMachinesListPaginationJSON(t *testing.T) {
	_, url := newCP(t)
	code, out, _ := runCLI(t, url, "tok", "machines", "ls", "--limit", "1", "--json")
	if code != client.ExitOK {
		t.Fatalf("exit = %d", code)
	}
	var p struct {
		Items   []client.Machine `json:"items"`
		Total   int              `json:"total"`
		Offset  int              `json:"offset"`
		Limit   int              `json:"limit"`
		HasMore bool             `json:"has_more"`
	}
	if err := json.Unmarshal([]byte(out), &p); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if len(p.Items) != 1 || p.Total != 2 || !p.HasMore || p.Limit != 1 {
		t.Fatalf("unexpected page: %+v", p)
	}
}

// --- Retries (TAV-39) ---

func TestRetrySucceedsAfterTransientFailure(t *testing.T) {
	cp, url := newCP(t)
	cp.machinesFailN = 1 // one 500 before the default 3-attempt budget is exhausted
	code, out, _ := runCLI(t, url, "tok", "machines", "ls")
	if code != client.ExitOK {
		t.Fatalf("exit = %d, want retry to recover", code)
	}
	if !strings.Contains(out, "m1") {
		t.Fatalf("output: %s", out)
	}
}

func TestRetryExhaustedFails(t *testing.T) {
	cp, url := newCP(t)
	cp.machinesFailN = 10 // always fails within the retry budget
	code, _, errs := runCLI(t, url, "tok", "machines", "ls")
	if code != client.ExitError {
		t.Fatalf("exit = %d, want 1: %s", code, errs)
	}
}

func TestNoRetryOnMutationStatusError(t *testing.T) {
	cp, url := newCP(t)
	cp.createFailN = 1
	code, _, _ := runCLI(t, url, "tok", "machines", "create", "--template", "go")
	if code != client.ExitError {
		t.Fatalf("exit = %d, want 1 (POST 5xx must not be retried)", code)
	}
	if cp.createFailN != 0 {
		t.Fatalf("createFailN = %d, want 0 (exactly one request sent)", cp.createFailN)
	}
	// The single failed attempt consumed the fault; a second invocation succeeds.
	code, out, _ := runCLI(t, url, "tok", "machines", "create", "--template", "go")
	if code != client.ExitOK {
		t.Fatalf("second create exit = %d: %s", code, out)
	}
}
