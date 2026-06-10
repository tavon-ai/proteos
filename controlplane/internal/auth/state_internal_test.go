package auth

import (
	"strconv"
	"testing"
	"time"
)

func TestStateRoundTrip(t *testing.T) {
	key := []byte("k")
	s, err := newState(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateState(key, s); err != nil {
		t.Fatalf("valid state rejected: %v", err)
	}
}

func TestStateWrongKeyRejected(t *testing.T) {
	s, _ := newState([]byte("right"))
	if err := validateState([]byte("wrong"), s); err == nil {
		t.Fatal("state signed with a different key should be rejected")
	}
}

func TestStateExpiredRejected(t *testing.T) {
	key := []byte("k")
	// Craft a state whose expiry is in the past but signature is valid.
	payload := "abc." + strconv.FormatInt(time.Now().Add(-time.Minute).Unix(), 10)
	expired := payload + "." + sign(key, payload)
	if err := validateState(key, expired); err == nil {
		t.Fatal("expired state should be rejected")
	}
}

func TestStateTamperedRejected(t *testing.T) {
	key := []byte("k")
	s, _ := newState(key)
	if err := validateState(key, s+"x"); err == nil {
		t.Fatal("tampered signature should be rejected")
	}
}
