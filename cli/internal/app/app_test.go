package app_test

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/tavon/proteos/cli/internal/app"
	"github.com/tavon/proteos/cli/internal/client"
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
		fmt.Fprint(w, `[{"id":"m1","name":"alpha","state":"running","guest_ip":"10.0.0.2","created_at":"2026-06-20T00:00:00Z"}]`)
	})
	mux.HandleFunc("GET /api/machines/{id}", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		if r.PathValue("id") != "m1" {
			writeErr(w, http.StatusNotFound, "no_machine")
			return
		}
		fmt.Fprint(w, `{"id":"m1","name":"alpha","state":"running","guest_ip":"10.0.0.2","created_at":"2026-06-20T00:00:00Z"}`)
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

func TestUsageExit2(t *testing.T) {
	_, url := newCP(t)
	// Missing --machine.
	code, _, _ := runCLI(t, url, "tok", "task", "ls")
	if code != client.ExitUsage {
		t.Fatalf("exit = %d, want 2", code)
	}
}
