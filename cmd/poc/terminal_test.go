package main

import "testing"

func TestRepoName(t *testing.T) {
	cases := []struct {
		url  string
		want string
	}{
		{"https://github.com/edwinavalos/rubbish", "rubbish"},
		{"https://github.com/edwinavalos/rubbish.git", "rubbish"},
		{"https://github.com/edwinavalos/rubbish/", "rubbish"},
		{"git@github.com:edwinavalos/rubbish.git", "rubbish"},
		{"", ""},
	}
	for _, tc := range cases {
		got := repoName(tc.url)
		if got != tc.want {
			t.Errorf("repoName(%q) = %q, want %q", tc.url, got, tc.want)
		}
	}
}

func TestSessionStartCmd_WithRepo(t *testing.T) {
	cmd := sessionStartCmd("https://github.com/edwinavalos/rubbish")
	want := "bash -l -c 'cd /root/workspace/rubbish 2>/dev/null || cd /root/workspace; claude; exec bash -l'"
	if cmd != want {
		t.Errorf("sessionStartCmd(repo) = %q, want %q", cmd, want)
	}
}

func TestSessionStartCmd_NoRepo(t *testing.T) {
	cmd := sessionStartCmd("")
	want := "bash -l -c 'claude; exec bash -l'"
	if cmd != want {
		t.Errorf("sessionStartCmd(\"\") = %q, want %q", cmd, want)
	}
}
