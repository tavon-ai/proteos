package machine_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tavon/proteos/controlplane/internal/machine"
	"github.com/tavon/proteos/controlplane/internal/nodeclient"
	"github.com/tavon/proteos/controlplane/internal/secrets"
	"github.com/tavon/proteos/controlplane/internal/store"
	"github.com/tavon/proteos/controlplane/internal/testutil"
)

// svcWithCatalog builds a lifecycle service backed by a real (testcontainer) DB,
// a fake agent, and a two-template catalog, returning the service + the user id.
func svcWithCatalog(t *testing.T) (*machine.Service, pgtype.UUID) {
	t.Helper()
	pool, q := testutil.Postgres(t)
	ctx := context.Background()

	user, err := q.UpsertUser(ctx, store.UpsertUserParams{GithubUserID: 7, Login: "u"})
	if err != nil {
		t.Fatal(err)
	}
	host, err := q.UpsertHostByName(ctx, store.UpsertHostByNameParams{Name: "h", AgentUrl: "http://x"})
	if err != nil {
		t.Fatal(err)
	}

	agent := newFakeAgent()
	srv := httptest.NewServer(agent.handler())
	t.Cleanup(srv.Close)

	catalog, err := machine.NewCatalog([]machine.Template{
		{ID: "base", Label: "Base", RootfsRef: "rootfs-base.ext4", Defaults: machine.Resources{Vcpus: 2, MemMiB: 2048, DiskMiB: 10240}},
		{ID: "full", Label: "Full", RootfsRef: "rootfs-full.ext4", Defaults: machine.Resources{Vcpus: 4, MemMiB: 4096, DiskMiB: 20480}},
	}, "vmlinux-6.1")
	if err != nil {
		t.Fatal(err)
	}

	svc := machine.NewService(pool, nodeclient.New(srv.URL, "tok"), machine.NewBroker(), secrets.NewMemStore(), host.ID, machine.Spec{
		Vcpus: 2, MemMiB: 2048, DiskMiB: 10240, KernelRef: "k1", RootfsRef: "r1",
		Catalog: catalog, Limits: machine.NewResourceLimits(8, 16384, 51200),
	})
	return svc, user.ID
}

func TestCreate_DefaultTemplate(t *testing.T) {
	svc, userID := svcWithCatalog(t)
	m, err := svc.Create(context.Background(), userID, machine.CreateOptions{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if m.TemplateID == nil || *m.TemplateID != "base" {
		t.Fatalf("template_id=%v, want base (catalog default)", m.TemplateID)
	}
	if m.RootfsRef != "rootfs-base.ext4" || m.KernelRef != "vmlinux-6.1" {
		t.Fatalf("refs=(%q,%q), want base template refs", m.RootfsRef, m.KernelRef)
	}
	if r := mustSpec(t, m.ResourceSpec); r.Vcpus != 2 || r.MemMiB != 2048 || r.DiskMiB != 10240 {
		t.Fatalf("resource_spec=%+v, want base defaults", r)
	}
}

func TestCreate_NamedTemplateUsesItsDefaults(t *testing.T) {
	svc, userID := svcWithCatalog(t)
	m, err := svc.Create(context.Background(), userID, machine.CreateOptions{TemplateID: "full"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if m.TemplateID == nil || *m.TemplateID != "full" {
		t.Fatalf("template_id=%v, want full", m.TemplateID)
	}
	if m.RootfsRef != "rootfs-full.ext4" {
		t.Fatalf("rootfs_ref=%q, want full image", m.RootfsRef)
	}
	if r := mustSpec(t, m.ResourceSpec); r.Vcpus != 4 || r.MemMiB != 4096 || r.DiskMiB != 20480 {
		t.Fatalf("resource_spec=%+v, want full defaults", r)
	}
}

func TestCreate_ResourceOverrideWithinCaps(t *testing.T) {
	svc, userID := svcWithCatalog(t)
	vcpus, mem, disk := 6, 8192, 30720
	m, err := svc.Create(context.Background(), userID, machine.CreateOptions{
		TemplateID: "base", Vcpus: &vcpus, MemMiB: &mem, DiskMiB: &disk,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Overrides win over the base template defaults.
	if r := mustSpec(t, m.ResourceSpec); r.Vcpus != 6 || r.MemMiB != 8192 || r.DiskMiB != 30720 {
		t.Fatalf("resource_spec=%+v, want overrides", r)
	}
	// The persistent disk is sized from the overridden disk_mib.
	disks, err := svc.DiskFor(context.Background(), m.ID)
	if err != nil || disks == nil {
		t.Fatalf("disk lookup: %v", err)
	}
	if int(disks.SizeMib) != 30720 {
		t.Fatalf("disk size=%d, want 30720 (override)", disks.SizeMib)
	}
}

func TestCreate_ResourceOverridePartial(t *testing.T) {
	svc, userID := svcWithCatalog(t)
	mem := 6144
	m, err := svc.Create(context.Background(), userID, machine.CreateOptions{TemplateID: "full", MemMiB: &mem})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Only mem overridden; vcpus/disk stay at the full template's defaults.
	if r := mustSpec(t, m.ResourceSpec); r.Vcpus != 4 || r.MemMiB != 6144 || r.DiskMiB != 20480 {
		t.Fatalf("resource_spec=%+v, want full defaults with mem override", r)
	}
}

func TestCreate_ResourceOverrideOutOfCaps(t *testing.T) {
	svc, userID := svcWithCatalog(t)
	tooMany := 99
	_, err := svc.Create(context.Background(), userID, machine.CreateOptions{TemplateID: "base", Vcpus: &tooMany})
	var ire machine.InvalidResourcesError
	if !errors.As(err, &ire) {
		t.Fatalf("err=%v, want InvalidResourcesError", err)
	}
	if ire.Detail != "vcpus must be 1..8" {
		t.Fatalf("detail=%q", ire.Detail)
	}
}

func TestCreate_UnknownTemplate(t *testing.T) {
	svc, userID := svcWithCatalog(t)
	_, err := svc.Create(context.Background(), userID, machine.CreateOptions{TemplateID: "nope"})
	if !errors.Is(err, machine.ErrUnknownTemplate) {
		t.Fatalf("err=%v, want ErrUnknownTemplate", err)
	}
}

func mustSpec(t *testing.T, raw []byte) machine.Resources {
	t.Helper()
	var r machine.Resources
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatalf("decode resource_spec: %v", err)
	}
	return r
}
