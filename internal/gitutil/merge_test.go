package gitutil

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// initBareRepo creates an empty bare git repo at dir.
func initBareRepo(t *testing.T, dir string) {
	t.Helper()
	if out, err := exec.Command("git", "init", "--bare", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %v\n%s", err, out)
	}
}

// initWorkingClone clones a bare repo and sets up identity for commits.
func initWorkingClone(t *testing.T, bareDir, cloneDir string) {
	t.Helper()
	if out, err := exec.Command("git", "clone", bareDir, cloneDir).CombinedOutput(); err != nil {
		t.Fatalf("git clone: %v\n%s", err, out)
	}
	for _, cfg := range [][2]string{
		{"user.email", "test@rubbish.local"},
		{"user.name", "Test"},
	} {
		if out, err := exec.Command("git", "-C", cloneDir, "config", cfg[0], cfg[1]).CombinedOutput(); err != nil {
			t.Fatalf("git config %s: %v\n%s", cfg[0], err, out)
		}
	}
}

// commitFile commits a file with the given content on the current branch.
func commitFile(t *testing.T, cloneDir, filename, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(cloneDir, filename), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"add", filename},
		{"commit", "-m", "add " + filename},
	} {
		if out, err := exec.Command("git", append([]string{"-C", cloneDir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

// pushBranch pushes the current branch to origin.
func pushBranch(t *testing.T, cloneDir, branch string) {
	t.Helper()
	if out, err := exec.Command("git", "-C", cloneDir, "push", "origin", branch).CombinedOutput(); err != nil {
		t.Fatalf("git push %s: %v\n%s", branch, err, out)
	}
}

// setupRepoWithBranches creates a bare repo plus an initial commit on main,
// then creates the named branches (each adding a unique file) and pushes them.
// Returns the bare repo path (usable as repoURL with file:// scheme).
func setupRepoWithBranches(t *testing.T, branches []string, conflictBranch bool) string {
	t.Helper()
	tmp := t.TempDir()
	bareDir := filepath.Join(tmp, "bare.git")
	initBareRepo(t, bareDir)

	// Seed main with an initial commit.
	seedDir := filepath.Join(tmp, "seed")
	initWorkingClone(t, bareDir, seedDir)
	commitFile(t, seedDir, "base.txt", "base")
	if out, err := exec.Command("git", "-C", seedDir, "push", "-u", "origin", "main").CombinedOutput(); err != nil {
		t.Fatalf("push main: %v\n%s", err, out)
	}

	for i, branch := range branches {
		safe := strings.ReplaceAll(branch, "/", "-")
		wdir := filepath.Join(tmp, "worker-"+safe)
		initWorkingClone(t, bareDir, wdir)
		if out, err := exec.Command("git", "-C", wdir, "checkout", "-b", branch).CombinedOutput(); err != nil {
			t.Fatalf("checkout -b %s: %v\n%s", branch, err, out)
		}
		var filename, content string
		if conflictBranch {
			// Both branches edit the same file → conflict on merge.
			filename = "conflict.txt"
			content = "version from " + branch
		} else {
			filename = "file-" + safe + ".txt"
			content = "content from " + branch
		}
		commitFile(t, wdir, filename, content)
		if i == 0 && conflictBranch {
			// Push first branch so second can be based off main (not it).
			pushBranch(t, wdir, branch)
			continue
		}
		pushBranch(t, wdir, branch)
	}

	return "file://" + bareDir
}

func TestMergeImplementBranches_EmptyList(t *testing.T) {
	merged, failed, err := MergeImplementBranches(context.Background(), "file:///nonexistent", "main", "", nil)
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if len(merged) != 0 || len(failed) != 0 {
		t.Fatalf("expected empty slices, got merged=%v failed=%v", merged, failed)
	}
}

func TestMergeImplementBranches_AllSucceed(t *testing.T) {
	branches := []string{"implement/aabbccdd-0", "implement/aabbccdd-1"}
	repoURL := setupRepoWithBranches(t, branches, false)

	merged, failed, err := MergeImplementBranches(context.Background(), repoURL, "main", "", branches)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(failed) != 0 {
		t.Fatalf("expected no failures, got %v", failed)
	}
	if len(merged) != 2 {
		t.Fatalf("expected 2 merged, got %v", merged)
	}

	// Verify both files are on main in the bare repo.
	tmp := t.TempDir()
	verifyDir := filepath.Join(tmp, "verify")
	repoPath := repoURL[len("file://"):]
	if out, err := exec.Command("git", "clone", repoPath, verifyDir).CombinedOutput(); err != nil {
		t.Fatalf("verify clone: %v\n%s", err, out)
	}
	for _, b := range branches {
		safe := strings.ReplaceAll(b, "/", "-")
		fname := "file-" + safe + ".txt"
		if _, err := os.Stat(filepath.Join(verifyDir, fname)); err != nil {
			t.Errorf("file %s not found in main after merge: %v", fname, err)
		}
	}

	// Verify implement branches were deleted from remote.
	for _, b := range branches {
		out, _ := exec.Command("git", "-C", verifyDir, "ls-remote", "origin", "refs/heads/"+b).Output()
		if len(out) > 0 {
			t.Errorf("branch %s still exists on remote after merge", b)
		}
	}
}

func TestMergeImplementBranches_ConflictOnSecond(t *testing.T) {
	branches := []string{"implement/aabbccdd-0", "implement/aabbccdd-1"}
	repoURL := setupRepoWithBranches(t, branches, true)

	merged, failed, err := MergeImplementBranches(context.Background(), repoURL, "main", "", branches)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(merged) != 1 || merged[0] != branches[0] {
		t.Errorf("expected merged=[%s], got %v", branches[0], merged)
	}
	if len(failed) != 1 || failed[0] != branches[1] {
		t.Errorf("expected failed=[%s], got %v", branches[1], failed)
	}
}

func TestMergeImplementBranches_BranchNotFound(t *testing.T) {
	repoURL := setupRepoWithBranches(t, nil, false)

	merged, failed, err := MergeImplementBranches(context.Background(), repoURL, "main", "", []string{"implement/doesnotexist-0"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(merged) != 0 {
		t.Errorf("expected nothing merged, got %v", merged)
	}
	if len(failed) != 1 {
		t.Errorf("expected 1 failed, got %v", failed)
	}
}

func TestMergeImplementBranches_ContextCancel(t *testing.T) {
	branches := []string{"implement/aabbccdd-0"}
	repoURL := setupRepoWithBranches(t, branches, false)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	_, _, err := MergeImplementBranches(ctx, repoURL, "main", "", branches)
	if err == nil {
		t.Fatal("expected error from cancelled context, got nil")
	}
}
