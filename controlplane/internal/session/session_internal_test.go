package session

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

func TestSessionIDStringInvalid(t *testing.T) {
	if got := SessionIDString(pgtype.UUID{}); got != "" {
		t.Errorf("invalid UUID => %q, want empty", got)
	}
}

func TestSessionIDStringCanonicalForm(t *testing.T) {
	// 00112233-4455-6677-8899-aabbccddeeff
	u := pgtype.UUID{
		Bytes: [16]byte{0x00, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88, 0x99, 0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff},
		Valid: true,
	}
	const want = "00112233-4455-6677-8899-aabbccddeeff"
	if got := SessionIDString(u); got != want {
		t.Errorf("SessionIDString = %q, want %q", got, want)
	}
}

func TestHashTokenIsDeterministicSHA256(t *testing.T) {
	a := hashToken("token")
	b := hashToken("token")
	if !bytes.Equal(a, b) {
		t.Error("hashToken not deterministic")
	}
	if len(a) != 32 {
		t.Errorf("hash length = %d, want 32", len(a))
	}
	if bytes.Equal(a, hashToken("different")) {
		t.Error("distinct inputs collided")
	}
}

func TestConstantTimeEqual(t *testing.T) {
	a := hashToken("x")
	if !Equal(a, hashToken("x")) {
		t.Error("equal hashes reported unequal")
	}
	if Equal(a, hashToken("y")) {
		t.Error("unequal hashes reported equal")
	}
}

func TestAuthenticateEmptyToken(t *testing.T) {
	m := NewManager(nil, time.Hour)
	_, _, err := m.Authenticate(context.Background(), "")
	if !errors.Is(err, ErrInvalidSession) {
		t.Fatalf("empty token => %v, want ErrInvalidSession", err)
	}
}

func TestAliveByIDInvalidUUID(t *testing.T) {
	m := NewManager(nil, time.Hour)
	_, err := m.AliveByID(context.Background(), pgtype.UUID{})
	if !errors.Is(err, ErrInvalidSession) {
		t.Fatalf("invalid id => %v, want ErrInvalidSession", err)
	}
}

func TestRevokeEmptyTokenNoop(t *testing.T) {
	m := NewManager(nil, time.Hour)
	if err := m.Revoke(context.Background(), ""); err != nil {
		t.Fatalf("Revoke(\"\") = %v, want nil no-op", err)
	}
}

func TestManagerAccessors(t *testing.T) {
	m := NewManager(nil, 90*time.Minute)
	if m.TTL() != 90*time.Minute {
		t.Errorf("TTL = %v", m.TTL())
	}
	// SetRevocationListener is a pure setter; just exercise both branches.
	m.SetRevocationListener(nil)
	m.SetRevocationListener(stubRevoker{})
}

type stubRevoker struct{}

func (stubRevoker) SessionRevoked(string) {}
