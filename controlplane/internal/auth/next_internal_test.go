package auth

import "testing"

// The table is chat's, deliberately: sanitizeNext is a port, and the two must
// not drift into disagreeing about what a safe redirect target is.
func TestSanitizeNext(t *testing.T) {
	cases := map[string]string{
		"":             "/",
		"/":            "/",
		"/m":           "/m",
		"/m/abc/pr/7":  "/m/abc/pr/7",
		"/ok?x=1#frag": "/ok?x=1#frag",

		// Every one of these is an open redirect if it gets through.
		"//evil.example":       "/",
		"/\\evil.example":      "/",
		"https://evil.example": "/",
		"javascript:alert(1)":  "/",
		"evil.example":         "/",
		// CR/LF would split the Location header.
		"/line\r\nInjection: yes": "/",
	}
	for in, want := range cases {
		if got := sanitizeNext(in); got != want {
			t.Errorf("sanitizeNext(%q) = %q, want %q", in, got, want)
		}
	}
}
