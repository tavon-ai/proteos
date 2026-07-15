package applog_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/tavon-ai/proteos/controlplane/internal/applog"
)

func TestStore_RingBufferEviction(t *testing.T) {
	s := applog.NewStore(3)
	for i := 0; i < 5; i++ {
		s.Add(applog.Entry{Message: string(rune('a' + i)), Source: "api"})
	}
	got := s.List("", 0)
	if len(got) != 3 {
		t.Fatalf("expected 3 entries after eviction, got %d", len(got))
	}
	want := []string{"c", "d", "e"}
	for i, e := range got {
		if e.Message != want[i] {
			t.Fatalf("entry %d = %q, want %q", i, e.Message, want[i])
		}
	}
}

func TestStore_ListFiltersBySource(t *testing.T) {
	s := applog.NewStore(10)
	s.Add(applog.Entry{Message: "1", Source: "api"})
	s.Add(applog.Entry{Message: "2", Source: "ui"})
	s.Add(applog.Entry{Message: "3", Source: "api"})

	api := s.List("api", 0)
	if len(api) != 2 || api[0].Message != "1" || api[1].Message != "3" {
		t.Fatalf("api filter = %+v", api)
	}
	ui := s.List("ui", 0)
	if len(ui) != 1 || ui[0].Message != "2" {
		t.Fatalf("ui filter = %+v", ui)
	}
	all := s.List("", 0)
	if len(all) != 3 {
		t.Fatalf("unfiltered = %+v", all)
	}
}

func TestStore_ListLimitReturnsMostRecent(t *testing.T) {
	s := applog.NewStore(10)
	for i := 0; i < 5; i++ {
		s.Add(applog.Entry{Message: string(rune('a' + i)), Source: "api"})
	}
	got := s.List("", 2)
	if len(got) != 2 || got[0].Message != "d" || got[1].Message != "e" {
		t.Fatalf("limited list = %+v", got)
	}
}

// TestHandler_CapturesAndDelegates verifies the tee handler both stores a
// record for the API to read back and still forwards it to the wrapped
// handler, so wrapping slog.Default() never changes stdout output.
func TestHandler_CapturesAndDelegates(t *testing.T) {
	var buf bytes.Buffer
	inner := slog.NewJSONHandler(&buf, nil)
	store := applog.NewStore(10)
	h := applog.NewHandler(inner, store, "api")
	logger := slog.New(h)

	logger.Info("hello", "count", 3)

	// Delegated to the wrapped handler.
	var decoded map[string]any
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatalf("decode stdout line: %v", err)
	}
	if decoded["msg"] != "hello" {
		t.Fatalf("stdout msg = %v", decoded["msg"])
	}

	// Captured into the store.
	entries := store.List("api", 0)
	if len(entries) != 1 {
		t.Fatalf("expected 1 captured entry, got %d", len(entries))
	}
	e := entries[0]
	if e.Message != "hello" || e.Source != "api" || e.Level != "INFO" {
		t.Fatalf("captured entry = %+v", e)
	}
	if e.Fields["count"] != "3" {
		t.Fatalf("captured fields = %+v", e.Fields)
	}
	if time.Since(e.Time) > time.Minute {
		t.Fatalf("captured time looks wrong: %v", e.Time)
	}
}
