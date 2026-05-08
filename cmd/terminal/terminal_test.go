package main

import "testing"

func TestSessionStartCmd_WithRepo(t *testing.T) {
	got := sessionStartCmd("https://github.com/edwinavalos/rubbish")
	want := `bash -l -c 'tmux new-session -A -s rubbish bash -l -c "cd ~/workspace/rubbish 2>/dev/null || cd ~; claude --dangerously-skip-permissions; exec bash -l"'`
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
