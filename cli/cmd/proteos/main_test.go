package main

import (
	"runtime/debug"
	"testing"
)

// defaults mirrors the un-stamped package vars, which is the state
// withBuildInfo is meant to improve on.
func defaults() identity {
	return identity{version: "dev", commit: "none", date: "unknown"}
}

func TestWithBuildInfo(t *testing.T) {
	tests := []struct {
		name string
		id   identity
		bi   *debug.BuildInfo
		ok   bool
		want identity
	}{
		{
			name: "no build info leaves defaults alone",
			id:   defaults(),
			ok:   false,
			want: defaults(),
		},
		{
			// `go install .../cmd/proteos@v0.1.5`: no linker flags, but the
			// resolved module version is embedded.
			name: "module install fills in the version",
			id:   defaults(),
			bi:   &debug.BuildInfo{Main: debug.Module{Version: "v0.1.5"}},
			ok:   true,
			want: identity{version: "v0.1.5", commit: "none", date: "unknown"},
		},
		{
			name: "(devel) is no better than dev",
			id:   defaults(),
			bi:   &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}},
			ok:   true,
			want: defaults(),
		},
		{
			// `go build` from a clean checkout.
			name: "vcs settings fill in commit and date",
			id:   defaults(),
			bi: &debug.BuildInfo{Settings: []debug.BuildSetting{
				{Key: "vcs.modified", Value: "false"},
				{Key: "vcs.revision", Value: "3f8116372b4d1a0c9e5f"},
				{Key: "vcs.time", Value: "2026-08-04T09:12:00Z"},
			}},
			ok:   true,
			want: identity{version: "dev", commit: "3f81163", date: "2026-08-04T09:12:00Z"},
		},
		{
			// vcs.modified sorts before vcs.revision in Settings, so this also
			// covers reading the two independently of their order.
			name: "a dirty tree is marked as such",
			id:   defaults(),
			bi: &debug.BuildInfo{Settings: []debug.BuildSetting{
				{Key: "vcs.modified", Value: "true"},
				{Key: "vcs.revision", Value: "3f8116372b4d1a0c9e5f"},
			}},
			ok:   true,
			want: identity{version: "dev", commit: "3f81163-dirty", date: "unknown"},
		},
		{
			name: "stamped values win over build info",
			id:   identity{version: "v1.2.3", commit: "abc1234", date: "2026-06-29T12:00:00Z"},
			bi: &debug.BuildInfo{
				Main: debug.Module{Version: "v0.1.5"},
				Settings: []debug.BuildSetting{
					{Key: "vcs.revision", Value: "3f8116372b4d1a0c9e5f"},
					{Key: "vcs.time", Value: "2026-08-04T09:12:00Z"},
				},
			},
			ok:   true,
			want: identity{version: "v1.2.3", commit: "abc1234", date: "2026-06-29T12:00:00Z"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.id.withBuildInfo(tt.bi, tt.ok)
			if got != tt.want {
				t.Errorf("withBuildInfo() = %+v, want %+v", got, tt.want)
			}
		})
	}
}
