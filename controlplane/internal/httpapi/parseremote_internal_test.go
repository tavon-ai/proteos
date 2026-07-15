package httpapi

import (
	"errors"
	"testing"
)

func TestParseRemote(t *testing.T) {
	cases := []struct {
		remote            string
		host, owner, repo string
		ok                bool
	}{
		{"https://github.com/octocat/hello.git", "github.com", "octocat", "hello", true},
		{"https://github.com/octocat/hello", "github.com", "octocat", "hello", true},
		{"http://ghe.example.com/octocat/hello.git", "ghe.example.com", "octocat", "hello", true},
		{"https://git.example.com:3000/octocat/hello.git", "git.example.com:3000", "octocat", "hello", true},
		{"https://CodeBerg.org/octocat/hello", "codeberg.org", "octocat", "hello", true},
		{"git@github.com:octocat/hello.git", "github.com", "octocat", "hello", true},
		{"ssh://git@github.com/octocat/hello.git", "github.com", "octocat", "hello", true},
		{"", "", "", "", false},
		{"https://github.com/octocat", "", "", "", false},
		{"not a url", "", "", "", false},
	}
	for _, c := range cases {
		host, owner, repo, ok := parseRemote(c.remote)
		if ok != c.ok || host != c.host || owner != c.owner || repo != c.repo {
			t.Errorf("parseRemote(%q) = (%q,%q,%q,%v), want (%q,%q,%q,%v)",
				c.remote, host, owner, repo, ok, c.host, c.owner, c.repo, c.ok)
		}
	}
}

func TestParseCloneURL(t *testing.T) {
	public := []string{"codeberg.org", "git.example.com:3000"}
	cases := []struct {
		name, raw          string
		host, fullName     string
		err, forbiddenHost bool
	}{
		{"github bare", "https://github.com/octocat/hello", "github.com", "octocat/hello", false, false},
		{"github .git", "https://github.com/octocat/hello.git", "github.com", "octocat/hello", false, false},
		{"trailing slash", "https://codeberg.org/octocat/hello/", "codeberg.org", "octocat/hello", false, false},
		{"public host", "https://codeberg.org/octocat/hello.git", "codeberg.org", "octocat/hello", false, false},
		{"public host with port", "https://git.example.com:3000/octocat/hello", "git.example.com:3000", "octocat/hello", false, false},
		{"host case-insensitive", "https://CodeBerg.org/octocat/hello", "codeberg.org", "octocat/hello", false, false},
		{"surrounding space", "  https://github.com/octocat/hello  ", "github.com", "octocat/hello", false, false},
		{"host not allowlisted", "https://evil.example.com/octocat/hello", "", "", true, true},
		{"http scheme", "http://codeberg.org/octocat/hello", "", "", true, false},
		{"scp-like", "git@codeberg.org:octocat/hello.git", "", "", true, false},
		{"userinfo", "https://user@codeberg.org/octocat/hello", "", "", true, false},
		{"query", "https://codeberg.org/octocat/hello?x=1", "", "", true, false},
		{"fragment", "https://codeberg.org/octocat/hello#main", "", "", true, false},
		{"extra path segment", "https://codeberg.org/octocat/hello/tree", "", "", true, false},
		{"owner only", "https://codeberg.org/octocat", "", "", true, false},
		{"traversal", "https://codeberg.org/../etc", "", "", true, false},
		{"no host", "https:///octocat/hello", "", "", true, false},
		{"empty", "", "", "", true, false},
	}
	for _, c := range cases {
		host, fullName, err := parseCloneURL(c.raw, "github.com", public)
		if (err != nil) != c.err || host != c.host || fullName != c.fullName {
			t.Errorf("%s: parseCloneURL(%q) = (%q,%q,%v), want (%q,%q,err=%v)",
				c.name, c.raw, host, fullName, err, c.host, c.fullName, c.err)
		}
		if c.forbiddenHost != errors.Is(err, errForbiddenHost) {
			t.Errorf("%s: parseCloneURL(%q) err = %v, want errForbiddenHost=%v", c.name, c.raw, err, c.forbiddenHost)
		}
	}

	// An empty allowlist admits only the auth host — today's deployments.
	if _, _, err := parseCloneURL("https://codeberg.org/octocat/hello", "github.com", nil); !errors.Is(err, errForbiddenHost) {
		t.Errorf("empty allowlist: err = %v, want errForbiddenHost", err)
	}
}
