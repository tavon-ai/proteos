package state

import (
	"net/netip"
	"testing"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := NewStore(t.TempDir(), netip.MustParsePrefix("172.30.0.0/24"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestReserveAllocatesLowestFreeIP(t *testing.T) {
	s := newTestStore(t)
	mk := func(a Alloc) Record {
		return Record{MachineID: "", Handle: "", State: "creating", GuestIP: a.GuestIP.String(), TapName: a.TapName, MAC: a.MAC, GatewayIP: a.GatewayIP.String()}
	}

	// Two distinct machines should get .2 then .3 (gateway is .1).
	ids := []string{"aaaaaaaa-0000-0000-0000-000000000001", "bbbbbbbb-0000-0000-0000-000000000002"}
	want := []string{"172.30.0.2", "172.30.0.3"}
	for i, id := range ids {
		rec, existed, err := s.Reserve(id, func(a Alloc) Record { r := mk(a); r.MachineID = id; return r })
		if err != nil {
			t.Fatal(err)
		}
		if existed {
			t.Fatalf("%s should be new", id)
		}
		if rec.GuestIP != want[i] {
			t.Fatalf("machine %d: got IP %s, want %s", i, rec.GuestIP, want[i])
		}
	}
}

func TestReserveIsIdempotent(t *testing.T) {
	s := newTestStore(t)
	id := "cccccccc-0000-0000-0000-000000000003"
	mk := func(a Alloc) Record {
		return Record{MachineID: id, State: "creating", GuestIP: a.GuestIP.String()}
	}
	r1, existed, err := s.Reserve(id, mk)
	if err != nil || existed {
		t.Fatalf("first reserve: existed=%v err=%v", existed, err)
	}
	r2, existed, err := s.Reserve(id, mk)
	if err != nil || !existed {
		t.Fatalf("second reserve: existed=%v err=%v", existed, err)
	}
	if r1.GuestIP != r2.GuestIP {
		t.Fatalf("idempotent reserve changed IP: %s vs %s", r1.GuestIP, r2.GuestIP)
	}
}

func TestFreedIPIsReused(t *testing.T) {
	s := newTestStore(t)
	mk := func(id string) func(a Alloc) Record {
		return func(a Alloc) Record { return Record{MachineID: id, GuestIP: a.GuestIP.String()} }
	}
	a, _, _ := s.Reserve("11111111", mk("11111111")) // .2
	_, _, _ = s.Reserve("22222222", mk("22222222"))  // .3
	if a.GuestIP != "172.30.0.2" {
		t.Fatalf("want .2 got %s", a.GuestIP)
	}
	if err := s.Delete("11111111"); err != nil {
		t.Fatal(err)
	}
	// The lowest free IP is now .2 again.
	c, _, _ := s.Reserve("33333333", mk("33333333"))
	if c.GuestIP != "172.30.0.2" {
		t.Fatalf("freed IP not reused: got %s, want 172.30.0.2", c.GuestIP)
	}
}

func TestDerivations(t *testing.T) {
	id := "deadbeef-1234-5678-9abc-def012345678"
	if got := TapName(id); got != "tapdeadbeef" {
		t.Fatalf("TapName=%q", got)
	}
	if len(TapName(id)) > 15 {
		t.Fatalf("tap name exceeds IFNAMSIZ: %q", TapName(id))
	}
	if got := Handle(id); got != "fc-deadbeef" {
		t.Fatalf("Handle=%q", got)
	}
	if got := MACFor(netip.MustParseAddr("172.30.0.2")); got != "06:00:ac:1e:00:02" {
		t.Fatalf("MACFor=%q", got)
	}
}

func TestUpdatePersists(t *testing.T) {
	dataDir := t.TempDir()
	subnet := netip.MustParsePrefix("172.30.0.0/24")
	s, err := NewStore(dataDir, subnet)
	if err != nil {
		t.Fatal(err)
	}
	id := "44444444"
	_, _, err = s.Reserve(id, func(a Alloc) Record { return Record{MachineID: id, State: "creating", GuestIP: a.GuestIP.String()} })
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.Update(id, func(r *Record) { r.State = "running" }); !ok {
		t.Fatal("update reported missing")
	}
	// A fresh Store over the same dir sees the persisted change.
	s2, err := NewStore(dataDir, subnet)
	if err != nil {
		t.Fatal(err)
	}
	rec, ok, err := s2.Load(id)
	if err != nil || !ok {
		t.Fatalf("reload: ok=%v err=%v", ok, err)
	}
	if rec.State != "running" {
		t.Fatalf("persisted state=%q, want running", rec.State)
	}
}
