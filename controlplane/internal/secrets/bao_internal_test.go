package secrets

import "testing"

// TestNormalizePrefix covers the slash-normalization edge cases: an empty or
// slash-only prefix disables namespacing, and any leading/trailing slashes are
// collapsed to a single trailing slash so callers can concatenate directly.
func TestNormalizePrefix(t *testing.T) {
	cases := map[string]string{
		"":               "",
		"/":              "",
		"//":             "",
		"proteos":        "proteos/",
		"/proteos":       "proteos/",
		"proteos/":       "proteos/",
		"/proteos/":      "proteos/",
		"team/proteos":   "team/proteos/",
		"/team/proteos/": "team/proteos/",
	}
	for in, want := range cases {
		if got := normalizePrefix(in); got != want {
			t.Errorf("normalizePrefix(%q) = %q, want %q", in, got, want)
		}
	}
}
