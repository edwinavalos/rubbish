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
	got := sessionStartCmd("https://github.com/edwinavalos/rubbish")
	want := `bash -l -c 'tmux new-session -A -s rubbish bash -l -c "cd /root/workspace/rubbish 2>/dev/null || cd ~; claude --dangerously-skip-permissions; exec bash -l"'`
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestSessionStartCmd_NoRepo(t *testing.T) {
	got := sessionStartCmd("")
	want := `bash -l -c 'tmux new-session -A -s rubbish bash -l -c "claude --dangerously-skip-permissions; exec bash -l"'`
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
