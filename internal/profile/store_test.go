package profile_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/edwinavalos/rubbish/internal/profile"
)

func TestLoad_MissingFile(t *testing.T) {
	s := profile.NewStore(filepath.Join(t.TempDir(), "profile.json"))
	p, err := s.Load()
	if err != nil {
		t.Fatalf("Load on missing file: %v", err)
	}
	if p.ClaudeOAuthToken != "" {
		t.Errorf("expected empty token, got %q", p.ClaudeOAuthToken)
	}
}

func TestSave_And_Load_RoundTrip(t *testing.T) {
	s := profile.NewStore(filepath.Join(t.TempDir(), "profile.json"))
	want := profile.Profile{
		ClaudeOAuthToken: "sk-ant-oat01-testtoken",
		GitHubToken:      "ghp_testtoken",
	}
	if err := s.Save(want); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := s.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.ClaudeOAuthToken != want.ClaudeOAuthToken {
		t.Errorf("ClaudeOAuthToken = %q, want %q", got.ClaudeOAuthToken, want.ClaudeOAuthToken)
	}
	if got.GitHubToken != want.GitHubToken {
		t.Errorf("GitHubToken = %q, want %q", got.GitHubToken, want.GitHubToken)
	}
}

func TestSave_FilePermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "profile.json")
	s := profile.NewStore(path)
	if err := s.Save(profile.Profile{ClaudeOAuthToken: "tok"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Errorf("file mode = %04o, want 0600", perm)
	}
}

func TestSave_EmptyToken(t *testing.T) {
	s := profile.NewStore(filepath.Join(t.TempDir(), "profile.json"))
	if err := s.Save(profile.Profile{}); err != nil {
		t.Fatalf("Save empty: %v", err)
	}
	p, err := s.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if p.ClaudeOAuthToken != "" {
		t.Errorf("expected empty token after saving empty, got %q", p.ClaudeOAuthToken)
	}
}

func TestMaskToken(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"sk-ant-oat01-abcdefgh", "*****************efgh"},
		{"abcd", "abcd"},
		{"abc", "abc"},
		{"", ""},
	}
	for _, tt := range tests {
		got := profile.MaskToken(tt.input)
		if got != tt.want {
			t.Errorf("MaskToken(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}
