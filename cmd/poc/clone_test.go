package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edwinavalos/rubbish/internal/profile"
	"github.com/edwinavalos/rubbish/internal/session"
)

// errOnMatchRunner wraps capturingSetupRunner and returns an error when a
// command contains a given substring. Used to simulate clone failures.
type errOnMatchRunner struct {
	capturingSetupRunner
	match string
	err   error
}

func (r *errOnMatchRunner) RunSetup(cmds []string) error {
	r.capturingSetupRunner.RunSetup(cmds) //nolint:errcheck
	for _, cmd := range cmds {
		if strings.Contains(cmd, r.match) {
			return r.err
		}
	}
	return nil
}

// indexOfMatch returns the index of the first command containing substr, or -1.
func indexOfMatch(cmds []string, substr string) int {
	for i, cmd := range cmds {
		if strings.Contains(cmd, substr) {
			return i
		}
	}
	return -1
}

// ---- normalizeRepoURL ---------------------------------------------------------

func TestNormalizeRepoURL(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"https://github.com/user/repo", "https://github.com/user/repo"},
		{"https://github.com/user/repo.git", "https://github.com/user/repo.git"},
		{"git@github.com:user/repo.git", "https://github.com/user/repo.git"},
		{"git@github.com:user/repo", "https://github.com/user/repo"},
		{"git@gitlab.com:org/project.git", "https://gitlab.com/org/project.git"},
		{"", ""},
	}
	for _, tc := range cases {
		got := normalizeRepoURL(tc.input)
		if got != tc.want {
			t.Errorf("normalizeRepoURL(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

// ---- Clone boot tests ---------------------------------------------------------

func storeWithGHToken(t *testing.T, token string) *profile.Store {
	t.Helper()
	store := profile.NewStore(filepath.Join(t.TempDir(), "profile.json"))
	if err := store.Save(profile.Profile{GitHubToken: token}); err != nil {
		t.Fatal(err)
	}
	return store
}

func TestBoot_ClonesHTTPSRepo(t *testing.T) {
	runner := &capturingSetupRunner{}
	store := storeWithGHToken(t, "ghp_testtoken")
	mgr := newTestManagerWithProfile(&fakeSnapshotter{}, &fakeLauncher{handle: &fakeVMHandle{}}, &fakeBridgeFactory{runner}, store)

	sess, err := mgr.Create(context.Background(), "https://github.com/example/myrepo", "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, mgr, sess.ID, session.StateReady, 3*time.Second)

	cmds := runner.allCmds()
	cloneIdx := indexOfMatch(cmds, "git clone")
	if cloneIdx < 0 {
		t.Fatalf("no git clone command issued; got: %v", cmds)
	}
	if !strings.Contains(cmds[cloneIdx], "https://github.com/example/myrepo") {
		t.Errorf("clone command = %q, want https URL", cmds[cloneIdx])
	}
	if !strings.Contains(cmds[cloneIdx], "/root/workspace/myrepo") {
		t.Errorf("clone command = %q, want clone into /root/workspace/myrepo", cmds[cloneIdx])
	}
}

func TestBoot_NormalizesSSHURL(t *testing.T) {
	runner := &capturingSetupRunner{}
	store := storeWithGHToken(t, "ghp_testtoken")
	mgr := newTestManagerWithProfile(&fakeSnapshotter{}, &fakeLauncher{handle: &fakeVMHandle{}}, &fakeBridgeFactory{runner}, store)

	sess, err := mgr.Create(context.Background(), "git@github.com:example/myrepo.git", "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, mgr, sess.ID, session.StateReady, 3*time.Second)

	// Stored URL should be HTTPS.
	if strings.Contains(sess.RepoURL, "git@") {
		t.Errorf("sess.RepoURL = %q, want SSH URL converted to HTTPS", sess.RepoURL)
	}

	// Clone command should use HTTPS, not SSH.
	cmds := runner.allCmds()
	for _, cmd := range cmds {
		if strings.Contains(cmd, "git clone") && strings.Contains(cmd, "git@") {
			t.Errorf("clone command still uses SSH URL: %q", cmd)
		}
	}
	cloneIdx := indexOfMatch(cmds, "git clone")
	if cloneIdx < 0 {
		t.Fatalf("no git clone command issued; got: %v", cmds)
	}
	if !strings.Contains(cmds[cloneIdx], "https://") {
		t.Errorf("clone command = %q, want https:// URL", cmds[cloneIdx])
	}
}

func TestBoot_CredentialsSetBeforeClone(t *testing.T) {
	runner := &capturingSetupRunner{}
	store := storeWithGHToken(t, "ghp_testtoken")
	mgr := newTestManagerWithProfile(&fakeSnapshotter{}, &fakeLauncher{handle: &fakeVMHandle{}}, &fakeBridgeFactory{runner}, store)

	sess, _ := mgr.Create(context.Background(), "https://github.com/example/myrepo", "", "", false)
	waitState(t, mgr, sess.ID, session.StateReady, 3*time.Second)

	cmds := runner.allCmds()
	credIdx := indexOfMatch(cmds, ".git-credentials")
	cloneIdx := indexOfMatch(cmds, "git clone")

	if credIdx < 0 {
		t.Fatal("no .git-credentials setup command found")
	}
	if cloneIdx < 0 {
		t.Fatal("no git clone command found")
	}
	if credIdx >= cloneIdx {
		t.Errorf("credentials set at index %d, clone at index %d — credentials must come first", credIdx, cloneIdx)
	}
}

func TestBoot_CloneFailure_SessionStillReady(t *testing.T) {
	runner := &errOnMatchRunner{match: "git clone", err: errors.New("exit status 128")}
	store := storeWithGHToken(t, "ghp_testtoken")
	mgr := newTestManagerWithProfile(&fakeSnapshotter{}, &fakeLauncher{handle: &fakeVMHandle{}}, &fakeBridgeFactory{runner}, store)

	sess, err := mgr.Create(context.Background(), "https://github.com/example/myrepo", "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	// Clone failure must not prevent the session from reaching ready.
	waitState(t, mgr, sess.ID, session.StateReady, 3*time.Second)
}

func TestBoot_NoCloneWithoutRepo(t *testing.T) {
	runner := &capturingSetupRunner{}
	mgr := newTestManager(&fakeSnapshotter{}, &fakeLauncher{handle: &fakeVMHandle{}}, &fakeBridgeFactory{runner})

	sess, _ := mgr.Create(context.Background(), "", "", "", false)
	waitState(t, mgr, sess.ID, session.StateReady, 3*time.Second)

	for _, cmd := range runner.allCmds() {
		if strings.Contains(cmd, "git clone") {
			t.Errorf("unexpected git clone with no repo URL; cmd: %q", cmd)
		}
	}
}

func TestBoot_BranchClone(t *testing.T) {
	runner := &capturingSetupRunner{}
	store := storeWithGHToken(t, "ghp_testtoken")
	mgr := newTestManagerWithProfile(&fakeSnapshotter{}, &fakeLauncher{handle: &fakeVMHandle{}}, &fakeBridgeFactory{runner}, store)

	sess, _ := mgr.Create(context.Background(), "https://github.com/example/myrepo", "feature-branch", "", false)
	waitState(t, mgr, sess.ID, session.StateReady, 3*time.Second)

	cmds := runner.allCmds()
	cloneIdx := indexOfMatch(cmds, "git clone")
	if cloneIdx < 0 {
		t.Fatal("no git clone command found")
	}
	if !strings.Contains(cmds[cloneIdx], "-b feature-branch") {
		t.Errorf("clone command = %q, want -b feature-branch", cmds[cloneIdx])
	}
}
