package gitutil

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// makeTestRepo creates a small git repo in dir and commits a file, so that
// git clone --bare and git clone --local work against it in tests.
func makeTestRepo(t *testing.T, dir string) {
	t.Helper()
	run := func(args ...string) {
		t.Helper()
		cmd := runInRepoErr(dir, args...)
		if cmd != nil {
			t.Fatalf("git %v: %v", args, cmd)
		}
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	run("git", "init")
	run("git", "config", "user.email", "test@test.com")
	run("git", "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello\n"), 0644); err != nil {
		t.Fatal(err)
	}
	run("git", "add", ".")
	run("git", "commit", "-m", "init")
}

func runInRepoErr(dir string, args ...string) error {
	return runInRepo(dir, args...)
}

// ---- injectToken -------------------------------------------------------------

func TestInjectToken(t *testing.T) {
	cases := []struct {
		rawURL string
		token  string
		want   string
	}{
		{
			rawURL: "https://github.com/user/repo.git",
			token:  "ghp_abc",
			want:   "https://oauth2:ghp_abc@github.com/user/repo.git",
		},
		{
			// existing credentials should be replaced
			rawURL: "https://oldtoken@github.com/user/repo.git",
			token:  "ghp_new",
			want:   "https://oauth2:ghp_new@github.com/user/repo.git",
		},
		{
			// SSH URL — should be returned unchanged
			rawURL: "git@github.com:user/repo.git",
			token:  "ghp_abc",
			want:   "git@github.com:user/repo.git",
		},
		{
			// empty token — no injection
			rawURL: "https://github.com/user/repo.git",
			token:  "",
			want:   "https://github.com/user/repo.git",
		},
	}
	for _, tc := range cases {
		got := injectToken(tc.rawURL, tc.token)
		if got != tc.want {
			t.Errorf("injectToken(%q, %q) = %q, want %q", tc.rawURL, tc.token, got, tc.want)
		}
	}
}

// ---- BareRepoPath / WorkspaceDir helpers ------------------------------------

func TestPathHelpers(t *testing.T) {
	c := NewRepoCache("/repos", "/workspaces")

	if got := c.BareRepoPath("myrepo"); got != "/repos/myrepo.git" {
		t.Errorf("BareRepoPath = %q", got)
	}
	if got := c.WorkspaceDir("sess-abc"); got != "/workspaces/sess-abc" {
		t.Errorf("WorkspaceDir = %q", got)
	}
	if got := c.WorkspaceRepoDir("sess-abc", "myrepo"); got != "/workspaces/sess-abc/myrepo" {
		t.Errorf("WorkspaceRepoDir = %q", got)
	}
}

// ---- EnsureBareRepo ---------------------------------------------------------

func TestEnsureBareRepo_Clone(t *testing.T) {
	tmp := t.TempDir()
	srcRepo := filepath.Join(tmp, "src")
	makeTestRepo(t, srcRepo)

	reposDir := filepath.Join(tmp, "repos")
	cache := NewRepoCache(reposDir, filepath.Join(tmp, "workspaces"))

	barePath, err := cache.EnsureBareRepo(srcRepo, "myrepo", "")
	if err != nil {
		t.Fatalf("EnsureBareRepo: %v", err)
	}

	if !strings.HasSuffix(barePath, "myrepo.git") {
		t.Errorf("barePath = %q, want suffix myrepo.git", barePath)
	}
	// The bare repo must contain a HEAD file.
	if _, err := os.Stat(filepath.Join(barePath, "HEAD")); err != nil {
		t.Errorf("bare repo missing HEAD: %v", err)
	}
}

func TestEnsureBareRepo_Fetch(t *testing.T) {
	tmp := t.TempDir()
	srcRepo := filepath.Join(tmp, "src")
	makeTestRepo(t, srcRepo)

	reposDir := filepath.Join(tmp, "repos")
	cache := NewRepoCache(reposDir, filepath.Join(tmp, "workspaces"))

	// First call creates the bare repo.
	if _, err := cache.EnsureBareRepo(srcRepo, "myrepo", ""); err != nil {
		t.Fatalf("first EnsureBareRepo: %v", err)
	}

	// Add another commit to the source.
	if err := os.WriteFile(filepath.Join(srcRepo, "extra.txt"), []byte("extra\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := runInRepo(srcRepo, "git", "add", "."); err != nil {
		t.Fatal(err)
	}
	if err := runInRepo(srcRepo, "git", "config", "user.email", "test@test.com"); err != nil {
		t.Fatal(err)
	}
	if err := runInRepo(srcRepo, "git", "config", "user.name", "Test"); err != nil {
		t.Fatal(err)
	}
	if err := runInRepo(srcRepo, "git", "commit", "-m", "second commit"); err != nil {
		t.Fatal(err)
	}

	// Second call should fetch without error.
	barePath, err := cache.EnsureBareRepo(srcRepo, "myrepo", "")
	if err != nil {
		t.Fatalf("second EnsureBareRepo: %v", err)
	}
	if barePath == "" {
		t.Error("barePath empty")
	}
}

// ---- LocalClone -------------------------------------------------------------

func TestLocalClone(t *testing.T) {
	tmp := t.TempDir()
	srcRepo := filepath.Join(tmp, "src")
	makeTestRepo(t, srcRepo)

	cache := NewRepoCache(
		filepath.Join(tmp, "repos"),
		filepath.Join(tmp, "workspaces"),
	)

	barePath, err := cache.EnsureBareRepo(srcRepo, "myrepo", "")
	if err != nil {
		t.Fatalf("EnsureBareRepo: %v", err)
	}

	destPath, err := cache.LocalClone(barePath, "session-1", "myrepo", "")
	if err != nil {
		t.Fatalf("LocalClone: %v", err)
	}

	// The cloned directory must contain our committed file.
	if _, err := os.Stat(filepath.Join(destPath, "README.md")); err != nil {
		t.Errorf("cloned repo missing README.md: %v", err)
	}
	// The path should match WorkspaceRepoDir.
	want := cache.WorkspaceRepoDir("session-1", "myrepo")
	if destPath != want {
		t.Errorf("LocalClone path = %q, want %q", destPath, want)
	}
}

func TestLocalClone_Branch(t *testing.T) {
	tmp := t.TempDir()
	srcRepo := filepath.Join(tmp, "src")
	makeTestRepo(t, srcRepo)

	// Create a feature branch in the source.
	if err := runInRepo(srcRepo, "git", "checkout", "-b", "feature"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcRepo, "feature.txt"), []byte("feat\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := runInRepo(srcRepo, "git", "add", "."); err != nil {
		t.Fatal(err)
	}
	if err := runInRepo(srcRepo, "git", "config", "user.email", "test@test.com"); err != nil {
		t.Fatal(err)
	}
	if err := runInRepo(srcRepo, "git", "config", "user.name", "Test"); err != nil {
		t.Fatal(err)
	}
	if err := runInRepo(srcRepo, "git", "commit", "-m", "feature commit"); err != nil {
		t.Fatal(err)
	}

	cache := NewRepoCache(
		filepath.Join(tmp, "repos"),
		filepath.Join(tmp, "workspaces"),
	)

	barePath, err := cache.EnsureBareRepo(srcRepo, "myrepo", "")
	if err != nil {
		t.Fatalf("EnsureBareRepo: %v", err)
	}

	destPath, err := cache.LocalClone(barePath, "session-1", "myrepo", "feature")
	if err != nil {
		t.Fatalf("LocalClone -b feature: %v", err)
	}

	// feature.txt should exist (it's only on the feature branch).
	if _, err := os.Stat(filepath.Join(destPath, "feature.txt")); err != nil {
		t.Errorf("feature.txt not found in feature-branch clone: %v", err)
	}
}

// ---- CleanupWorkspace -------------------------------------------------------

func TestCleanupWorkspace(t *testing.T) {
	tmp := t.TempDir()
	cache := NewRepoCache(filepath.Join(tmp, "repos"), filepath.Join(tmp, "workspaces"))

	wsDir := cache.WorkspaceDir("sess-xyz")
	if err := os.MkdirAll(wsDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wsDir, "file.txt"), []byte("data"), 0644); err != nil {
		t.Fatal(err)
	}

	cache.CleanupWorkspace("sess-xyz")

	if _, err := os.Stat(wsDir); !os.IsNotExist(err) {
		t.Errorf("workspace dir still exists after cleanup: %v", err)
	}
}

func TestCleanupWorkspace_NonExistent(t *testing.T) {
	tmp := t.TempDir()
	cache := NewRepoCache(filepath.Join(tmp, "repos"), filepath.Join(tmp, "workspaces"))
	// Should not panic or return error for a non-existent workspace.
	cache.CleanupWorkspace("no-such-session")
}
