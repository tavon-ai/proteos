package client_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tavon-ai/proteos/cli/internal/client"
)

// TestStreamEventsIgnoresHTTPClientTimeout verifies that the global Timeout set
// on http.Client does not kill a long-lived SSE stream. The server deliberately
// pauses longer than the configured client timeout between events; the stream
// must survive and deliver all events.
func TestStreamEventsIgnoresHTTPClientTimeout(t *testing.T) {
	const serverPause = 150 * time.Millisecond
	const clientTimeout = 50 * time.Millisecond // much shorter than serverPause

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		send := func(id, data string) {
			fmt.Fprintf(w, "id: %s\nevent: agent\ndata: %s\n\n", id, data)
			if fl != nil {
				fl.Flush()
			}
		}
		send("1", `{"kind":"assistant_text","text":"hello"}`)
		time.Sleep(serverPause)
		send("2", `{"kind":"result","status":"done"}`)
	}))
	t.Cleanup(ts.Close)

	c := client.New(ts.URL, "")
	c.HTTP.Timeout = clientTimeout

	var got []client.Event
	err := c.StreamEvents(context.Background(), "m1", "t1", "", func(_ string, ev client.Event) error {
		got = append(got, ev)
		return nil
	})
	if err != nil {
		t.Fatalf("StreamEvents returned error (stream was killed by HTTP client timeout?): %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d events, want 2", len(got))
	}
}
