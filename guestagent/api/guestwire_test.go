package guestwire

import "testing"

func TestCleanWorkdir(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"", "", false},
		{"/workspace", "/workspace", true},
		{"/workspace/", "/workspace", true},
		{"/workspace/repo", "/workspace/repo", true},
		{"/workspace/owner-repo", "/workspace/owner-repo", true},
		{"/workspace/a/b/c", "/workspace/a/b/c", true},
		{"/workspace/repo/../repo", "/workspace/repo", true},
		// Escapes and foreign roots are rejected.
		{"/workspace/../etc/passwd", "", false},
		{"/workspace/..", "", false},
		{"/etc/passwd", "", false},
		{"/root", "", false},
		{"workspace/repo", "", false}, // relative
		{"/workspaceother", "", false}, // prefix-only, not nested
	}
	for _, c := range cases {
		got, ok := CleanWorkdir(c.in)
		if ok != c.ok || got != c.want {
			t.Errorf("CleanWorkdir(%q) = (%q, %v), want (%q, %v)", c.in, got, ok, c.want, c.ok)
		}
	}
}
