package machine

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func validTemplates() []Template {
	return []Template{
		{ID: "base", Label: "Base", RootfsRef: "rootfs-base.ext4", Defaults: Resources{Vcpus: 2, MemMiB: 2048, DiskMiB: 10240}},
		{ID: "full", Label: "Full", RootfsRef: "rootfs-full.ext4", KernelRef: "vmlinux-7", Defaults: Resources{Vcpus: 4, MemMiB: 4096, DiskMiB: 20480}},
	}
}

func TestNewCatalog_DefaultsAndLookup(t *testing.T) {
	c, err := NewCatalog(validTemplates(), "vmlinux-6.1")
	if err != nil {
		t.Fatalf("NewCatalog: %v", err)
	}
	if c.Empty() {
		t.Fatal("catalog should not be empty")
	}
	if got := c.Default().ID; got != "base" {
		t.Fatalf("Default()=%q, want base (first entry)", got)
	}
	// base omits kernel_ref ⇒ filled from the default; full keeps its own.
	if got, _ := c.Get("base"); got.KernelRef != "vmlinux-6.1" {
		t.Fatalf("base kernel_ref=%q, want default vmlinux-6.1", got.KernelRef)
	}
	if got, _ := c.Get("full"); got.KernelRef != "vmlinux-7" {
		t.Fatalf("full kernel_ref=%q, want its own vmlinux-7", got.KernelRef)
	}
	if _, ok := c.Get("nope"); ok {
		t.Fatal("Get(nope) should be !ok")
	}
	if n := len(c.Templates()); n != 2 {
		t.Fatalf("Templates() len=%d, want 2", n)
	}
}

func TestNewCatalog_Validation(t *testing.T) {
	cases := []struct {
		name string
		in   []Template
		ker  string
	}{
		{"empty list", nil, "k"},
		{"blank id", []Template{{Label: "x", RootfsRef: "r", Defaults: Resources{1, 1, 1}}}, "k"},
		{"dup id", []Template{
			{ID: "a", RootfsRef: "r", Defaults: Resources{1, 1, 1}},
			{ID: "a", RootfsRef: "r2", Defaults: Resources{1, 1, 1}},
		}, "k"},
		{"missing rootfs", []Template{{ID: "a", Defaults: Resources{1, 1, 1}}}, "k"},
		{"no kernel anywhere", []Template{{ID: "a", RootfsRef: "r", Defaults: Resources{1, 1, 1}}}, ""},
		{"non-positive defaults", []Template{{ID: "a", RootfsRef: "r", Defaults: Resources{0, 1, 1}}}, "k"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewCatalog(tc.in, tc.ker); err == nil {
				t.Fatal("expected error, got nil")
			}
		})
	}
}

func TestSingleTemplateCatalog(t *testing.T) {
	c, err := SingleTemplateCatalog("rootfs.ext4", "vmlinux-6.1", Resources{Vcpus: 2, MemMiB: 2048, DiskMiB: 10240})
	if err != nil {
		t.Fatalf("SingleTemplateCatalog: %v", err)
	}
	d := c.Default()
	if d.ID != "base" || d.RootfsRef != "rootfs.ext4" || d.KernelRef != "vmlinux-6.1" {
		t.Fatalf("unexpected base template: %+v", d)
	}
}

func TestLoadCatalogFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "templates.json")
	body := `{"templates":[
		{"id":"go","label":"Go development","rootfs_ref":"rootfs-go.ext4","defaults":{"vcpus":2,"mem_mib":2048,"disk_mib":10240}},
		{"id":"full","label":"Full","rootfs_ref":"rootfs-full.ext4","defaults":{"vcpus":4,"mem_mib":4096,"disk_mib":20480}}
	]}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadCatalogFile(path, "vmlinux-6.1")
	if err != nil {
		t.Fatalf("LoadCatalogFile: %v", err)
	}
	if c.Default().ID != "go" {
		t.Fatalf("default=%q, want go", c.Default().ID)
	}
	if got, _ := c.Get("go"); got.KernelRef != "vmlinux-6.1" {
		t.Fatalf("go kernel_ref=%q, want filled default", got.KernelRef)
	}

	if _, err := LoadCatalogFile(filepath.Join(dir, "missing.json"), "k"); err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestResourceLimits(t *testing.T) {
	l := NewResourceLimits(8, 16384, 51200)
	if l.Empty() {
		t.Fatal("configured limits should not be Empty()")
	}
	if (ResourceLimits{}).Empty() == false {
		t.Fatal("zero limits should be Empty()")
	}
	// Fixed floors.
	if l.Vcpus.Min != 1 || l.MemMiB.Min != 1024 || l.DiskMiB.Min != 5120 {
		t.Fatalf("unexpected floors: %+v", l)
	}
	// In-range passes.
	if err := l.Validate(Resources{Vcpus: 4, MemMiB: 4096, DiskMiB: 20480}); err != nil {
		t.Fatalf("in-range should pass: %v", err)
	}
	// Each dimension out of range yields InvalidResourcesError naming it.
	cases := []struct {
		name string
		r    Resources
		want string
	}{
		{"vcpus high", Resources{Vcpus: 9, MemMiB: 4096, DiskMiB: 20480}, "vcpus must be 1..8"},
		{"vcpus low", Resources{Vcpus: 0, MemMiB: 4096, DiskMiB: 20480}, "vcpus must be 1..8"},
		{"mem high", Resources{Vcpus: 2, MemMiB: 99999, DiskMiB: 20480}, "mem_mib must be 1024..16384"},
		{"disk low", Resources{Vcpus: 2, MemMiB: 4096, DiskMiB: 100}, "disk_mib must be 5120..51200"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := l.Validate(tc.r)
			var ire InvalidResourcesError
			if !errors.As(err, &ire) {
				t.Fatalf("err=%v, want InvalidResourcesError", err)
			}
			if ire.Detail != tc.want {
				t.Fatalf("detail=%q, want %q", ire.Detail, tc.want)
			}
		})
	}
}

func TestParseResources(t *testing.T) {
	r := parseResources([]byte(`{"vcpus":4,"mem_mib":4096,"disk_mib":20480}`))
	if r.Vcpus != 4 || r.MemMiB != 4096 || r.DiskMiB != 20480 {
		t.Fatalf("parseResources got %+v", r)
	}
	if z := parseResources(nil); z != (Resources{}) {
		t.Fatalf("nil spec should be zero, got %+v", z)
	}
	if z := parseResources([]byte("not json")); z != (Resources{}) {
		t.Fatalf("malformed spec should be zero, got %+v", z)
	}
}
