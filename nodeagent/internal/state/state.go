// Package state persists per-machine runtime facts to disk so the node-agent
// re-attaches across restarts, and owns IP/tap/MAC allocation from the host
// subnet. One JSON file per machine under <dataDir>/machines/<id>.json, written
// atomically (temp file + rename). The agent — not the control plane — owns
// allocation, because it owns the host network namespace.
package state

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// Record is the full on-disk state of one machine. It is the source of truth
// the driver re-attaches to after an agent restart.
type Record struct {
	MachineID string `json:"machine_id"`
	Handle    string `json:"handle"`
	State     string `json:"state"`  // agentapi.State*
	Reason    string `json:"reason"` // error reason, if any

	// Desired shape (echoed back from the ensure request).
	Vcpus     int    `json:"vcpus"`
	MemMiB    int    `json:"mem_mib"`
	KernelRef string `json:"kernel_ref"`
	RootfsRef string `json:"rootfs_ref"`

	// Network allocation (owned by the agent).
	TapName   string `json:"tap_name"`
	GuestIP   string `json:"guest_ip"`
	GatewayIP string `json:"gateway_ip"`
	MAC       string `json:"mac"`

	// Runtime handle to the backing process / VMM, used for liveness probing on
	// re-attach. For the dev driver this is the stub child pid; for firecracker
	// it is the jailed firecracker pid.
	Pid int `json:"pid"`
}

// Store is a concurrency-safe, disk-backed collection of machine Records plus
// the IP allocator for one host subnet.
type Store struct {
	mu      sync.Mutex
	dir     string // <dataDir>/machines
	subnet  netip.Prefix
	gateway netip.Addr
}

// NewStore opens (creating if needed) the machines directory under dataDir and
// fixes the gateway as the subnet's first usable address (.1).
func NewStore(dataDir string, subnet netip.Prefix) (*Store, error) {
	dir := filepath.Join(dataDir, "machines")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create state dir: %w", err)
	}
	gw := subnet.Masked().Addr().Next() // network addr + 1
	return &Store{dir: dir, subnet: subnet.Masked(), gateway: gw}, nil
}

// Gateway returns the host-side gateway address for the subnet.
func (s *Store) Gateway() netip.Addr { return s.gateway }

func (s *Store) path(id string) string { return filepath.Join(s.dir, id+".json") }

// MachineDir is a per-machine directory under the store root, for runtime
// artifacts that are not the state record itself (e.g. the dev driver's
// guest.sock). It is sibling to the machine's <id>.json file.
func (s *Store) MachineDir(id string) string { return filepath.Join(s.dir, id) }

// Reserve returns the existing record for id, or — if none exists — allocates
// network resources (lowest free IP, derived tap name + MAC), persists a fresh
// record in the "creating" state, and returns it. Allocation and persistence
// happen under the same lock so concurrent ensures never double-allocate an IP.
// The bool reports whether the record already existed.
func (s *Store) Reserve(id string, mk func(alloc Alloc) Record) (Record, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if rec, ok, err := s.loadLocked(id); err != nil {
		return Record{}, false, err
	} else if ok {
		return rec, true, nil
	}

	ip, err := s.allocateIPLocked()
	if err != nil {
		return Record{}, false, err
	}
	alloc := Alloc{
		TapName:   TapName(id),
		GuestIP:   ip,
		GatewayIP: s.gateway,
		MAC:       MACFor(ip),
	}
	rec := mk(alloc)
	if err := s.saveLocked(rec); err != nil {
		return Record{}, false, err
	}
	return rec, false, nil
}

// Alloc is the network allocation handed to the record constructor in Reserve.
type Alloc struct {
	TapName   string
	GuestIP   netip.Addr
	GatewayIP netip.Addr
	MAC       string
}

// Save atomically persists rec.
func (s *Store) Save(rec Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveLocked(rec)
}

// Load returns the record for id and whether it exists.
func (s *Store) Load(id string) (Record, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadLocked(id)
}

// Update applies fn to the record for id and persists the result atomically,
// all under the store lock — so read-modify-write races between the HTTP
// handlers and the async boot/stop goroutines are impossible. Returns
// ErrNotFound-equivalent (ok=false) if the record is gone.
func (s *Store) Update(id string, fn func(rec *Record)) (Record, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok, err := s.loadLocked(id)
	if err != nil || !ok {
		return Record{}, ok, err
	}
	fn(&rec)
	if err := s.saveLocked(rec); err != nil {
		return Record{}, true, err
	}
	return rec, true, nil
}

// Delete removes the record for id (freeing its IP). Missing ⇒ nil.
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Remove(s.path(id)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("delete state: %w", err)
	}
	return nil
}

// List returns every persisted record, sorted by machine id for determinism.
func (s *Store) List() ([]Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listLocked()
}

// --- locked internals -------------------------------------------------------

func (s *Store) loadLocked(id string) (Record, bool, error) {
	b, err := os.ReadFile(s.path(id))
	if os.IsNotExist(err) {
		return Record{}, false, nil
	}
	if err != nil {
		return Record{}, false, fmt.Errorf("read state %s: %w", id, err)
	}
	var rec Record
	if err := json.Unmarshal(b, &rec); err != nil {
		return Record{}, false, fmt.Errorf("decode state %s: %w", id, err)
	}
	return rec, true, nil
}

func (s *Store) saveLocked(rec Record) error {
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}
	final := s.path(rec.MachineID)
	tmp, err := os.CreateTemp(s.dir, rec.MachineID+".*.tmp")
	if err != nil {
		return fmt.Errorf("temp state: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeds
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return fmt.Errorf("write state: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close state: %w", err)
	}
	if err := os.Rename(tmpName, final); err != nil {
		return fmt.Errorf("rename state: %w", err)
	}
	return nil
}

func (s *Store) listLocked() ([]Record, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("read state dir: %w", err)
	}
	var out []Record
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		id := strings.TrimSuffix(name, ".json")
		rec, ok, err := s.loadLocked(id)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, rec)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].MachineID < out[j].MachineID })
	return out, nil
}

// allocateIPLocked returns the lowest free host address in the subnet, skipping
// the network address and the gateway. "Free" means not present in any
// persisted record. Caller must hold s.mu.
func (s *Store) allocateIPLocked() (netip.Addr, error) {
	recs, err := s.listLocked()
	if err != nil {
		return netip.Addr{}, err
	}
	used := make(map[netip.Addr]bool, len(recs))
	for _, r := range recs {
		if a, err := netip.ParseAddr(r.GuestIP); err == nil {
			used[a] = true
		}
	}

	// Candidates start at gateway+1 and run to the last address before
	// broadcast. For a /24 that is .2 .. .254.
	addr := s.gateway.Next()
	for s.subnet.Contains(addr) {
		next := addr.Next()
		// Stop before the broadcast address (last in the prefix).
		if !s.subnet.Contains(next) {
			break
		}
		if !used[addr] {
			return addr, nil
		}
		addr = next
	}
	return netip.Addr{}, fmt.Errorf("no free IP in subnet %s", s.subnet)
}

// TapName derives an IFNAMSIZ-safe tap device name from the machine UUID:
// "tap" + the first 8 hex chars (dashes stripped). 3+8 = 11 < 15.
func TapName(machineID string) string {
	return "tap" + ID8(machineID)
}

// Handle derives the VM handle reported to the control plane: "fc-<id8>".
func Handle(machineID string) string {
	return "fc-" + ID8(machineID)
}

// ID8 returns the first 8 hex characters of a machine UUID with dashes removed.
func ID8(machineID string) string {
	h := strings.ReplaceAll(machineID, "-", "")
	if len(h) > 8 {
		h = h[:8]
	}
	return h
}

// MACFor builds a locally-administered MAC from the guest IPv4: 06:00 followed
// by the four address octets (the spike's scheme). Non-IPv4 ⇒ a stable
// all-zero-host fallback that still has the 06:00 LAA prefix.
func MACFor(ip netip.Addr) string {
	if ip.Is4() {
		o := ip.As4()
		return fmt.Sprintf("06:00:%02x:%02x:%02x:%02x", o[0], o[1], o[2], o[3])
	}
	return "06:00:00:00:00:00"
}
