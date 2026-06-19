package machine

import (
	"encoding/json"
	"fmt"
	"os"
)

// Resources is a machine's resource spec: pinned vCPUs, memory, and persistent
// disk size. It is the in-Go shape of the resource_spec JSON column
// ({"vcpus":2,"mem_mib":2048,"disk_mib":10240}).
type Resources struct {
	Vcpus   int `json:"vcpus"`
	MemMiB  int `json:"mem_mib"`
	DiskMiB int `json:"disk_mib"`
}

// Bound is an inclusive [Min,Max] range for a single resource dimension.
type Bound struct {
	Min int `json:"min"`
	Max int `json:"max"`
}

// ResourceLimits bounds the per-resource overrides a user may request at create
// time (Slice 2 decision #3). It is global (same caps for every template); the
// mins are fixed and the maxes come from PROTEOS_MAX_VCPUS / _MEM_MIB / _DISK_MIB.
type ResourceLimits struct {
	Vcpus   Bound `json:"vcpus"`
	MemMiB  Bound `json:"mem_mib"`
	DiskMiB Bound `json:"disk_mib"`
}

// NewResourceLimits builds the caps from the configured maxes, filling the fixed
// floors (1 vcpu / 1024 MiB / 5120 MiB) the create flow enforces.
func NewResourceLimits(maxVcpus, maxMemMiB, maxDiskMiB int) ResourceLimits {
	return ResourceLimits{
		Vcpus:   Bound{Min: 1, Max: maxVcpus},
		MemMiB:  Bound{Min: 1024, Max: maxMemMiB},
		DiskMiB: Bound{Min: 5120, Max: maxDiskMiB},
	}
}

// Empty reports whether the limits are unset (zero value). The service skips
// bound-checking when limits are empty (tests that construct a Spec without
// caps); production always configures them.
func (l ResourceLimits) Empty() bool { return l == ResourceLimits{} }

// Validate returns an InvalidResourcesError if any dimension of r falls outside
// its bound. A nil return means r is acceptable.
func (l ResourceLimits) Validate(r Resources) error {
	if err := l.Vcpus.check("vcpus", r.Vcpus); err != nil {
		return err
	}
	if err := l.MemMiB.check("mem_mib", r.MemMiB); err != nil {
		return err
	}
	if err := l.DiskMiB.check("disk_mib", r.DiskMiB); err != nil {
		return err
	}
	return nil
}

func (b Bound) check(name string, v int) error {
	if v < b.Min || v > b.Max {
		return InvalidResourcesError{Detail: fmt.Sprintf("%s must be %d..%d", name, b.Min, b.Max)}
	}
	return nil
}

// InvalidResourcesError is returned by Create when a requested resource override
// is out of range. Detail is a human-readable elaboration the API surfaces as
// {"error":"invalid_resources","detail":"vcpus must be 1..8"}.
type InvalidResourcesError struct{ Detail string }

func (e InvalidResourcesError) Error() string { return "machine: invalid resources: " + e.Detail }

// Template is one entry in the machine-template catalog: a named rootfs image
// (the language toolchain layer) plus its default resource spec. The platform
// layer and kernel are identical across templates, so a template varies only the
// rootfs image and the suggested resources. RootfsRef/KernelRef are internal
// build detail and are never exposed to the SPA (only id/label/description and
// the defaults are — see TemplateView).
type Template struct {
	ID          string    `json:"id"`
	Label       string    `json:"label"`
	Description string    `json:"description"`
	RootfsRef   string    `json:"rootfs_ref"`
	KernelRef   string    `json:"kernel_ref"`
	Defaults    Resources `json:"defaults"`
}

// Catalog is the ordered, static set of templates a user can create machines
// from (Slice 1 decision #1: a static config, embedded default or loaded from
// PROTEOS_TEMPLATES_FILE). The first entry is the default when create omits a
// template id. A zero Catalog is Empty(): the service then falls back to the
// global Spec refs/resources and stamps no template id (the legacy path, used by
// tests that construct a Spec without a catalog).
type Catalog struct {
	templates []Template
	byID      map[string]Template
}

// NewCatalog validates and indexes an ordered template list. It rejects an empty
// list, blank/duplicate ids, a missing rootfs_ref, or non-positive default
// resources — a bad catalog is a startup error, never a per-machine boot
// failure. defaultKernelRef fills any template that omits its own kernel_ref
// (the kernel is global; templates rarely set it).
func NewCatalog(templates []Template, defaultKernelRef string) (Catalog, error) {
	if len(templates) == 0 {
		return Catalog{}, fmt.Errorf("template catalog is empty")
	}
	byID := make(map[string]Template, len(templates))
	out := make([]Template, 0, len(templates))
	for i, t := range templates {
		if t.ID == "" {
			return Catalog{}, fmt.Errorf("template[%d]: id is required", i)
		}
		if _, dup := byID[t.ID]; dup {
			return Catalog{}, fmt.Errorf("template %q: duplicate id", t.ID)
		}
		if t.RootfsRef == "" {
			return Catalog{}, fmt.Errorf("template %q: rootfs_ref is required", t.ID)
		}
		if t.KernelRef == "" {
			t.KernelRef = defaultKernelRef
		}
		if t.KernelRef == "" {
			return Catalog{}, fmt.Errorf("template %q: kernel_ref is required (no default kernel set)", t.ID)
		}
		if t.Defaults.Vcpus <= 0 || t.Defaults.MemMiB <= 0 || t.Defaults.DiskMiB <= 0 {
			return Catalog{}, fmt.Errorf("template %q: defaults must have positive vcpus, mem_mib, disk_mib", t.ID)
		}
		byID[t.ID] = t
		out = append(out, t)
	}
	return Catalog{templates: out, byID: byID}, nil
}

// SingleTemplateCatalog builds a one-entry catalog ("base") from the global
// image refs and resource spec. It is the fallback when no PROTEOS_TEMPLATES_FILE
// is configured, so a deployment with a single baked image still presents one
// selectable template and stamps template_id="base" on new machines.
func SingleTemplateCatalog(rootfsRef, kernelRef string, defaults Resources) (Catalog, error) {
	return NewCatalog([]Template{{
		ID:          "base",
		Label:       "Base",
		Description: "Platform baseline image.",
		RootfsRef:   rootfsRef,
		KernelRef:   kernelRef,
		Defaults:    defaults,
	}}, kernelRef)
}

// LoadCatalogFile reads a JSON catalog file of the shape
// {"templates":[{"id","label","description","rootfs_ref","kernel_ref","defaults":{...}}]}.
// defaultKernelRef fills any entry that omits kernel_ref.
func LoadCatalogFile(path, defaultKernelRef string) (Catalog, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Catalog{}, fmt.Errorf("read templates file: %w", err)
	}
	var doc struct {
		Templates []Template `json:"templates"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return Catalog{}, fmt.Errorf("parse templates file %s: %w", path, err)
	}
	return NewCatalog(doc.Templates, defaultKernelRef)
}

// parseResources decodes a machine's resource_spec JSON column into Resources.
// A malformed/empty spec yields the zero value (callers treat zero fields as
// "fall back to the global Spec").
func parseResources(b []byte) Resources {
	var r Resources
	if len(b) == 0 {
		return r
	}
	_ = json.Unmarshal(b, &r)
	return r
}

// Empty reports whether the catalog has no templates (the zero value). The
// service treats an empty catalog as the legacy single-image path.
func (c Catalog) Empty() bool { return len(c.templates) == 0 }

// Templates returns the catalog entries in declared order (the first is the
// default). The returned slice is a copy; callers may not mutate the catalog.
func (c Catalog) Templates() []Template {
	out := make([]Template, len(c.templates))
	copy(out, c.templates)
	return out
}

// Default returns the first template (the create-time default). Only valid when
// !Empty().
func (c Catalog) Default() Template { return c.templates[0] }

// Get returns the template with the given id, or ok=false if unknown.
func (c Catalog) Get(id string) (Template, bool) {
	t, ok := c.byID[id]
	return t, ok
}
