package taskevents

import (
	"encoding/json"
	"testing"
	"time"
)

func data(s string) json.RawMessage { return json.RawMessage(`{"t":"` + s + `"}`) }

func TestHub_SnapshotAndLiveFanOut(t *testing.T) {
	h := New(100, time.Minute)

	// Two events published before anyone subscribes form the snapshot.
	h.Publish("task1", data("a"), false)
	h.Publish("task1", data("b"), false)

	backlog, ch, cancel, terminal := h.Subscribe("task1", 0)
	defer cancel()
	if terminal {
		t.Fatal("not terminal yet")
	}
	if len(backlog) != 2 || backlog[0].Seq != 1 || backlog[1].Seq != 2 {
		t.Fatalf("snapshot = %+v", backlog)
	}

	// A live event arrives on the channel.
	h.Publish("task1", data("c"), false)
	select {
	case f := <-ch:
		if f.Seq != 3 {
			t.Fatalf("live seq = %d, want 3", f.Seq)
		}
	case <-time.After(time.Second):
		t.Fatal("live event not delivered")
	}
}

func TestHub_ReconnectReplayAfterSeq(t *testing.T) {
	h := New(100, time.Minute)
	for range 5 {
		h.Publish("t", data("x"), false)
	}
	// Reconnect claiming the first 3 were already seen.
	backlog, _, cancel, _ := h.Subscribe("t", 3)
	defer cancel()
	if len(backlog) != 2 || backlog[0].Seq != 4 || backlog[1].Seq != 5 {
		t.Fatalf("replay = %+v", backlog)
	}
}

func TestHub_BoundedRingTruncates(t *testing.T) {
	h := New(3, time.Minute)
	var firstTrunc int
	for range 6 {
		if h.Publish("t", data("x"), false) {
			firstTrunc++
		}
	}
	if firstTrunc != 1 {
		t.Fatalf("first-truncation signalled %d times, want exactly 1", firstTrunc)
	}
	backlog, _, cancel, _ := h.Subscribe("t", 0)
	defer cancel()
	// Only the last 3 survive; their seqs are monotonic (4,5,6).
	if len(backlog) != 3 || backlog[0].Seq != 4 || backlog[2].Seq != 6 {
		t.Fatalf("ring = %+v", backlog)
	}
}

func TestHub_TerminalMarksAndCloses(t *testing.T) {
	h := New(100, time.Minute)
	h.Publish("t", data("a"), false)
	h.Publish("t", data("done"), true)

	backlog, _, cancel, terminal := h.Subscribe("t", 0)
	defer cancel()
	if !terminal {
		t.Fatal("expected terminal after a terminal publish")
	}
	if !backlog[len(backlog)-1].Terminal {
		t.Fatal("last backlog frame should be terminal")
	}
}

func TestHub_LiveTerminalDelivered(t *testing.T) {
	h := New(100, time.Minute)
	_, ch, cancel, _ := h.Subscribe("t", 0)
	defer cancel()
	h.Publish("t", data("a"), false)
	h.Publish("t", data("end"), true)

	var got []Frame
	for f := range ch {
		got = append(got, f)
		if f.Terminal {
			break
		}
	}
	if len(got) != 2 || !got[1].Terminal {
		t.Fatalf("live frames = %+v", got)
	}
}

func TestHub_ReapAfterRetention(t *testing.T) {
	h := New(100, 20*time.Millisecond)
	h.Publish("t", data("a"), false)
	h.Publish("t", data("end"), true) // terminal + no subscribers → schedule reap

	// After the retention window the stream is gone: a fresh subscribe sees an
	// empty, non-terminal stream (a brand-new one).
	time.Sleep(60 * time.Millisecond)
	backlog, _, cancel, terminal := h.Subscribe("t", 0)
	defer cancel()
	if terminal || len(backlog) != 0 {
		t.Fatalf("expected reaped stream, got backlog=%+v terminal=%v", backlog, terminal)
	}
}

func TestHub_SubscribeDuringRetentionKeepsStream(t *testing.T) {
	h := New(100, 30*time.Millisecond)
	h.Publish("t", data("a"), false)
	h.Publish("t", data("end"), true)

	// A subscriber arriving within the window cancels the reap and still sees the
	// history; the cancel then reschedules the reap.
	backlog, _, cancel, terminal := h.Subscribe("t", 0)
	if !terminal || len(backlog) != 2 {
		t.Fatalf("expected the still-live terminal stream, got backlog=%+v terminal=%v", backlog, terminal)
	}
	cancel()
}
