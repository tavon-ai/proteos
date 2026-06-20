package httpapi

import "testing"

func TestParseOwnerRepo(t *testing.T) {
	cases := []struct {
		remote      string
		owner, repo string
		ok          bool
	}{
		{"https://github.com/octocat/hello.git", "octocat", "hello", true},
		{"https://github.com/octocat/hello", "octocat", "hello", true},
		{"http://ghe.example.com/octocat/hello.git", "octocat", "hello", true},
		{"git@github.com:octocat/hello.git", "octocat", "hello", true},
		{"ssh://git@github.com/octocat/hello.git", "octocat", "hello", true},
		{"", "", "", false},
		{"https://github.com/octocat", "", "", false},
		{"not a url", "", "", false},
	}
	for _, c := range cases {
		owner, repo, ok := parseOwnerRepo(c.remote)
		if ok != c.ok || owner != c.owner || repo != c.repo {
			t.Errorf("parseOwnerRepo(%q) = (%q,%q,%v), want (%q,%q,%v)",
				c.remote, owner, repo, ok, c.owner, c.repo, c.ok)
		}
	}
}
