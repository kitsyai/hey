package main

import "testing"

func TestGitURL(t *testing.T) {
	cases := map[string]string{
		"acme/widget":                    "https://github.com/acme/widget.git",
		"acme/widget.git":                "https://github.com/acme/widget.git",
		"https://example.com/x/y.git":    "https://example.com/x/y.git",
		"git@github.com:acme/widget.git": "git@github.com:acme/widget.git",
	}
	for in, want := range cases {
		if got := gitURL(in); got != want {
			t.Errorf("gitURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRepoDir(t *testing.T) {
	cases := map[string]string{
		"acme/widget":                    "widget",
		"acme/widget.git":                "widget",
		"https://github.com/acme/widget": "widget",
		"git@github.com:acme/widget.git": "widget",
	}
	for in, want := range cases {
		if got := repoDir(in); got != want {
			t.Errorf("repoDir(%q) = %q, want %q", in, got, want)
		}
	}
}
