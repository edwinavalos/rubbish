package gitutil

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

const (
	// DefaultReposDir is where bare repo caches live on the host.
	DefaultReposDir = "/opt/rubbish/repos"

	// DefaultWorkspacesDir is where per-session local clones live on the host.
	// These are mounted into the VM via virtio-fs so the VM sees the workspace
	// without any in-VM data transfer.
	DefaultWorkspacesDir = "/opt/rubbish/workspaces"
)

// RepoCache manages a set of bare Git repos on the host and creates fast
// per-session local clones from them.
//
// Workflow C flow:
//  1. EnsureBareRepo — fetch or update a bare repo at DefaultReposDir/<name>.git
//  2. LocalClone    — git clone --local from the bare repo into DefaultWorkspacesDir/<sessID>/<name>
//  3. VM mounts the workspace dir via virtio-fs (wired separately in launcher.go)
//  4. On session teardown, CleanupWorkspace removes DefaultWorkspacesDir/<sessID>
type RepoCache struct {
	reposDir      string
	workspacesDir string

	mu    sync.Mutex
	locks map[string]*sync.Mutex // per bare-repo lock to serialise concurrent fetches
}

// NewRepoCache creates a RepoCache using the given directory roots.
// Callers should use DefaultReposDir and DefaultWorkspacesDir for production.
func NewRepoCache(reposDir, workspacesDir string) *RepoCache {
	return &RepoCache{
		reposDir:      reposDir,
		workspacesDir: workspacesDir,
		locks:         make(map[string]*sync.Mutex),
	}
}

// repoLock returns the per-repo mutex, creating it on first use.
func (c *RepoCache) repoLock(name string) *sync.Mutex {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.locks[name] == nil {
		c.locks[name] = &sync.Mutex{}
	}
	return c.locks[name]
}

// BareRepoPath returns the host path of the bare repo for the given repo name.
func (c *RepoCache) BareRepoPath(repoName string) string {
	return filepath.Join(c.reposDir, repoName+".git")
}

// WorkspaceDir returns the per-session workspace directory on the host.
// This is the directory that gets mounted into the VM via virtio-fs.
func (c *RepoCache) WorkspaceDir(sessionID string) string {
	return filepath.Join(c.workspacesDir, sessionID)
}

// WorkspaceRepoDir returns the path within a session workspace where a specific
// repo is cloned: DefaultWorkspacesDir/<sessID>/<repoName>.
func (c *RepoCache) WorkspaceRepoDir(sessionID, repoName string) string {
	return filepath.Join(c.WorkspaceDir(sessionID), repoName)
}

// EnsureBareRepo ensures a bare repo cache exists at BareRepoPath(repoName).
//
//   - If the bare repo does not exist: runs git clone --bare <url> to create it.
//   - If the bare repo already exists: runs git fetch --prune to update all refs.
//
// Concurrent callers for the same repoName are serialised so only one
// fetch/clone runs at a time.
//
// The optional token is injected into the URL for private repos (GitHub HTTPS
// credential pattern).  Pass "" for public repos or when credentials are
// already configured in the git credential helper.
func (c *RepoCache) EnsureBareRepo(repoURL, repoName, token string) (string, error) {
	mu := c.repoLock(repoName)
	mu.Lock()
	defer mu.Unlock()

	if err := os.MkdirAll(c.reposDir, 0755); err != nil {
		return "", fmt.Errorf("create repos dir %s: %w", c.reposDir, err)
	}

	barePath := c.BareRepoPath(repoName)

	// Build the authenticated URL once.
	authURL := repoURL
	if token != "" {
		authURL = injectToken(repoURL, token)
	}

	if _, err := os.Stat(barePath); os.IsNotExist(err) {
		// First time — clone.
		log.Printf("[repocache] bare clone %s → %s", repoURL, barePath)
		out, err := exec.Command("git", "clone", "--bare", "--", authURL, barePath).CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("git clone --bare %s: %s: %w", repoURL, out, err)
		}

		// Configure the bare repo so future fetches retrieve all branches.
		// A bare clone sets fetch refspec to "+refs/heads/*:refs/heads/*"
		// but not necessarily the remote tracking refs.  Explicitly set it.
		if err := runInRepo(barePath, "git", "config", "remote.origin.fetch",
			"+refs/heads/*:refs/heads/*"); err != nil {
			log.Printf("[repocache] warning: set fetch refspec: %v", err)
		}
	} else if err == nil {
		// Already exists — fetch to update.
		log.Printf("[repocache] fetching updates for %s", barePath)
		// We must use the authenticated URL; the origin remote may still have
		// the bare URL without a token.
		out, err := exec.Command("git", "-C", barePath, "fetch", "--prune", authURL,
			"+refs/heads/*:refs/heads/*").CombinedOutput()
		if err != nil {
			// Non-fatal: log and continue — stale clone is better than failing the session.
			log.Printf("[repocache] warning: fetch %s: %s: %v", repoURL, out, err)
		}
	} else {
		return "", fmt.Errorf("stat %s: %w", barePath, err)
	}

	return barePath, nil
}

// LocalClone creates a per-session workspace by doing a git clone --local from
// the bare repo.  --local uses hardlinks on the same filesystem (instant, no
// data copied).  The destination is WorkspaceRepoDir(sessionID, repoName).
//
// If branch is non-empty, -b <branch> is passed to git clone.
//
// Returns the path to the cloned directory.
func (c *RepoCache) LocalClone(barePath, sessionID, repoName, branch string) (string, error) {
	wsDir := c.WorkspaceDir(sessionID)
	if err := os.MkdirAll(wsDir, 0755); err != nil {
		return "", fmt.Errorf("create workspace dir %s: %w", wsDir, err)
	}

	dest := c.WorkspaceRepoDir(sessionID, repoName)

	args := []string{"clone", "--local"}
	if branch != "" {
		args = append(args, "-b", branch)
	}
	args = append(args, "--", barePath, dest)

	log.Printf("[repocache] local clone %s → %s", barePath, dest)
	cmd := exec.Command("git", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("git clone --local %s: %s: %w", barePath, out, err)
	}

	log.Printf("[repocache] local clone complete: %s", dest)
	return dest, nil
}

// CleanupWorkspace removes the per-session workspace directory on the host.
// It should be called after the VM has been stopped (so the virtio-fs mount
// is already unmounted) and after DeleteSnapshot.
//
// Errors are logged but not returned — the caller should proceed regardless.
func (c *RepoCache) CleanupWorkspace(sessionID string) {
	wsDir := c.WorkspaceDir(sessionID)
	if err := os.RemoveAll(wsDir); err != nil {
		log.Printf("[repocache] warning: cleanup workspace %s: %v", wsDir, err)
	} else {
		log.Printf("[repocache] cleaned up workspace %s", wsDir)
	}
}

// injectToken inserts an OAuth token into an HTTPS GitHub URL so that
// git operations authenticate without an interactive prompt.
//
//	https://github.com/user/repo  →  https://oauth2:<token>@github.com/user/repo
//
// Returns rawURL unchanged when token is empty.
func injectToken(rawURL, token string) string {
	if token == "" {
		return rawURL
	}
	const https = "https://"
	if !strings.HasPrefix(rawURL, https) {
		return rawURL // SSH or other — leave as-is
	}
	rest := strings.TrimPrefix(rawURL, https)
	// Remove any existing user:pass@ to avoid double-injection.
	if at := strings.Index(rest, "@"); at >= 0 {
		rest = rest[at+1:]
	}
	return https + "oauth2:" + token + "@" + rest
}

// runInRepo runs a git sub-command with the repo as the working directory.
func runInRepo(repoPath string, args ...string) error {
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = repoPath
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%v: %s: %w", args, out, err)
	}
	return nil
}
