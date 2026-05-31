package main

import (
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	fc "github.com/edwinavalos/rubbish/internal/compute/firecracker"
	"github.com/edwinavalos/rubbish/internal/gitutil"
	"github.com/edwinavalos/rubbish/internal/mcp"
	"github.com/edwinavalos/rubbish/internal/profile"
	"github.com/edwinavalos/rubbish/internal/session"
	"github.com/edwinavalos/rubbish/internal/sessionstore"
	"github.com/edwinavalos/rubbish/internal/terminal"
	"github.com/edwinavalos/rubbish/internal/vm"
	"github.com/edwinavalos/rubbish/internal/workflow"
	"golang.org/x/crypto/ssh"
)

// Compile-time interface checks.
var (
	_ workflow.SessionStarter = (*SessionManager)(nil)
	_ mcp.WorkflowHandler     = (*workflow.Engine)(nil)
	_ mcp.SessionHandler      = (*SessionManager)(nil)
)

//go:embed static/index.html
var indexHTML []byte

//go:embed static/profile.html
var profileHTML []byte

//go:embed static/orchestrator.html
var orchestratorHTML []byte

// ---- Interfaces for testability ---------------------------------------------

type snapshotter interface {
	CreateSnapshot(id string) (string, error)
	DeleteSnapshot(id string) error
	InjectNetworkConfig(device, ip, gateway string) error
}

type vmLauncher interface {
	Launch(ctx context.Context, slot int, rootfsPath string, memMiB int64) (vmHandle, error)
}

type vmHandle interface {
	WaitForSSH(ctx context.Context) error
	Stop(ctx context.Context) error
}

type bridgeFactory interface {
	NewBridge(addr string, signer ssh.Signer) setupRunner
}

type setupRunner interface {
	RunSetup(cmds []string) error
	RunSetupCapture(cmds []string, w io.Writer) error
	// RunCapture runs a single command and returns stdout and stderr separately.
	// Used for the non-interactive claude -p invocation so we can inspect stderr
	// for usage-limit errors without false-positives from task output.
	RunCapture(cmd string) (stdout, stderr string, err error)
}

// claudeJSONOutput is the parsed result of `claude -p --output-format json`.
type claudeJSONOutput struct {
	Result  string `json:"result"`
	IsError bool   `json:"is_error"`
}

// parseClaudeJSON extracts the result text from claude's JSON output.
// Falls back to the raw string if stdout is not valid JSON, so old/text-mode
// invocations continue to work without modification.
func parseClaudeJSON(stdout string) (result string, isError bool) {
	stdout = strings.TrimSpace(stdout)
	if stdout == "" {
		return "", false
	}
	var r claudeJSONOutput
	if err := json.Unmarshal([]byte(stdout), &r); err != nil {
		return stdout, false
	}
	return r.Result, r.IsError
}

// ---- Real adapters ----------------------------------------------------------

type realSnapshotter struct{ m *fc.SnapshotManager }

func (r *realSnapshotter) CreateSnapshot(id string) (string, error) { return r.m.CreateSnapshot(id) }
func (r *realSnapshotter) DeleteSnapshot(id string) error           { return r.m.DeleteSnapshot(id) }
func (r *realSnapshotter) InjectNetworkConfig(device, ip, gateway string) error {
	return vm.InjectNetworkConfig(device, ip, gateway)
}

type realVMLauncher struct{}

func (realVMLauncher) Launch(ctx context.Context, slot int, rootfsPath string, memMiB int64) (vmHandle, error) {
	return vm.Launch(ctx, slot, rootfsPath, memMiB)
}

type realBridgeFactory struct{ signer ssh.Signer }

func (r *realBridgeFactory) NewBridge(addr string, _ ssh.Signer) setupRunner {
	return terminal.NewBridge(addr, "claude", r.signer)
}

// ---- Session ----------------------------------------------------------------

type Session struct {
	ID        string    `json:"id"`
	Slot      int       `json:"slot"`
	RepoURL   string    `json:"repo_url"`
	Branch    string    `json:"branch,omitempty"`
	DevMode   bool      `json:"dev_mode,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	Error     string    `json:"error,omitempty"`
	Role      string    `json:"role,omitempty"`
	Prompt    string    `json:"prompt,omitempty"`
	Result    string    `json:"result,omitempty"`

	// workspacePath is the host-side directory created by the bare-repo local
	// clone (Workflow C).  It is set during boot() and cleaned up by Stop().
	// Empty when no repo was requested or when Workflow C is disabled.
	workspacePath string

	sm     *session.StateMachine
	v      vmHandle
	bridge setupRunner
	cancel context.CancelFunc
}

// SessionStatus returns the current lifecycle state.
func (s *Session) SessionStatus() session.State { return s.sm.State() }

// MarshalJSON emits the JSON shape the frontend expects, with "status" from the state machine.
func (s *Session) MarshalJSON() ([]byte, error) {
	type wire struct {
		ID        string        `json:"id"`
		Slot      int           `json:"slot"`
		Status    session.State `json:"status"`
		RepoURL   string        `json:"repo_url"`
		Branch    string        `json:"branch,omitempty"`
		DevMode   bool          `json:"dev_mode,omitempty"`
		CreatedAt time.Time     `json:"created_at"`
		Error     string        `json:"error,omitempty"`
		Role      string        `json:"role,omitempty"`
		Prompt    string        `json:"prompt,omitempty"`
		Result    string        `json:"result,omitempty"`
	}
	return json.Marshal(wire{
		ID:        s.ID,
		Slot:      s.Slot,
		Status:    s.sm.State(),
		RepoURL:   s.RepoURL,
		Branch:    s.Branch,
		DevMode:   s.DevMode,
		CreatedAt: s.CreatedAt,
		Error:     s.Error,
		Role:      s.Role,
		Prompt:    s.Prompt,
		Result:    s.Result,
	})
}

// ---- SessionManager ---------------------------------------------------------

type SessionManager struct {
	mu         sync.RWMutex
	sessions   map[string]*Session
	usedSlots  map[int]bool
	maxSlots   int // 0 = unlimited
	snap       snapshotter
	launcher   vmLauncher
	bridges    bridgeFactory
	signer     ssh.Signer
	credPath   string
	prof       *profile.Store
	store      *sessionstore.Store
	devHostIP  string // host IP used for NFS mounts in dev-mode sessions
	repoCache  *gitutil.RepoCache
	serverCtx  context.Context // lifetime context for background goroutines (boot, etc.)
}

func NewSessionManager(snapMgr *fc.SnapshotManager, signer ssh.Signer, credPath string, prof *profile.Store, store *sessionstore.Store, maxSlots int, devHostIP string) *SessionManager {
	return newSessionManager(
		&realSnapshotter{snapMgr},
		realVMLauncher{},
		&realBridgeFactory{signer},
		signer,
		credPath,
		prof,
		store,
		maxSlots,
		devHostIP,
		gitutil.NewRepoCache(gitutil.DefaultReposDir, gitutil.DefaultWorkspacesDir),
	)
}

func newSessionManager(snap snapshotter, launcher vmLauncher, bridges bridgeFactory, signer ssh.Signer, credPath string, prof *profile.Store, store *sessionstore.Store, maxSlots int, devHostIP string, repoCache *gitutil.RepoCache) *SessionManager {
	return &SessionManager{
		sessions:  make(map[string]*Session),
		usedSlots: make(map[int]bool),
		maxSlots:  maxSlots,
		snap:      snap,
		launcher:  launcher,
		bridges:   bridges,
		signer:    signer,
		credPath:  credPath,
		prof:      prof,
		store:     store,
		devHostIP: devHostIP,
		repoCache: repoCache,
	}
}

func (m *SessionManager) persistSession(sess *Session) {
	if m.store == nil {
		return
	}
	m.mu.RLock()
	row := sessionstore.Row{
		ID:        sess.ID,
		Slot:      sess.Slot,
		Status:    string(sess.sm.State()),
		RepoURL:   sess.RepoURL,
		Branch:    sess.Branch,
		ErrorMsg:  sess.Error,
		DevMode:   sess.DevMode,
		CreatedAt: sess.CreatedAt,
		UpdatedAt: time.Now(),
		Role:      sess.Role,
		Prompt:    sess.Prompt,
		Result:    sess.Result,
	}
	m.mu.RUnlock()
	if err := m.store.Upsert(row); err != nil {
		slog.Warn("session: persist error", "session", sess.ID[:8], "err", err)
	}
}

// newSM creates a state machine starting at initial with logging and DB-persist
// hooks wired up. sess must be the owning Session (pointer is safe to capture).
func (m *SessionManager) newSM(sess *Session, initial session.State) *session.StateMachine {
	sm := session.New(initial)
	sm.OnTransition(func(from, to session.State, elapsed time.Duration) {
		slog.Info("session: state transition", "session", sess.ID[:8], "from", from, "to", to, "elapsed_s", elapsed.Seconds())
		m.persistSession(sess)
	})
	return sm
}

func (m *SessionManager) allocSlot() (int, bool) {
	if m.maxSlots > 0 && len(m.usedSlots) >= m.maxSlots {
		return 0, false
	}
	for i := 0; ; i++ {
		if m.maxSlots > 0 && i >= m.maxSlots {
			return 0, false
		}
		if !m.usedSlots[i] {
			m.usedSlots[i] = true
			return i, true
		}
	}
}

func (m *SessionManager) freeSlot(slot int) {
	m.mu.Lock()
	delete(m.usedSlots, slot)
	m.mu.Unlock()
}

// Create allocates a slot and starts a VM in the background.
// Returns immediately with status "provisioning".
func (m *SessionManager) Create(parentCtx context.Context, repoURL, branch, githubToken string, devMode bool, role, prompt string) (*Session, error) {
	if role == "" {
		role = "interactive"
	}
	m.mu.Lock()
	slot, ok := m.allocSlot()
	if !ok {
		m.mu.Unlock()
		return nil, fmt.Errorf("no slots available (max %d concurrent sessions)", m.maxSlots)
	}

	id := uuid.New().String()
	ctx, cancel := context.WithCancel(parentCtx)

	sess := &Session{
		ID:        id,
		Slot:      slot,
		RepoURL:   normalizeRepoURL(repoURL),
		Branch:    branch,
		DevMode:   devMode,
		CreatedAt: time.Now(),
		cancel:    cancel,
		Role:      role,
		Prompt:    prompt,
	}
	sess.sm = m.newSM(sess, session.StateProvisioning)
	m.sessions[id] = sess
	m.mu.Unlock()

	m.persistSession(sess) // initial provisioning state
	go m.boot(ctx, sess, githubToken)
	return sess, nil
}

func (m *SessionManager) boot(ctx context.Context, sess *Session, githubToken string) {
	bootStart := time.Now()
	sid := sess.ID[:8]

	// t logs how long a named step took and returns the current time for the next step.
	t := func(step string, since time.Time) time.Time {
		slog.Info("session: step", "session", sid, "step", step, "elapsed", time.Since(since).Round(time.Millisecond))
		return time.Now()
	}

	fail := func(err error) {
		m.mu.Lock()
		sess.Error = err.Error()
		delete(m.usedSlots, sess.Slot)
		m.mu.Unlock()
		sess.sm.Transition(session.StateFailed) //nolint:errcheck — hook persists
		slog.Error("session: failed", "session", sid, "err", err)
	}

	// Resolve the token once — prefer the per-request token, then the stored profile/file.
	token := githubToken
	if token == "" && m.prof != nil {
		if p, err := m.prof.Load(); err == nil {
			token = p.GitHubToken
		}
	}
	if token == "" {
		token = loadSavedToken(m.credPath)
	}

	// 1a. Bare-repo fetch + host-side local clone (Workflow C).
	//
	// Done before VM launch so the workspace directory is ready when the VM
	// starts.  The VM will see it via a virtio-fs mount (wired in launcher.go).
	// This runs entirely on the host and is fast (hardlinks, no network for the
	// clone step).
	repoName := gitutil.RepoName(sess.RepoURL)
	if repoName == "" && sess.RepoURL != "" {
		repoName = "repo"
	}
	if sess.RepoURL != "" && m.repoCache != nil {
		stepT := time.Now()
		barePath, err := m.repoCache.EnsureBareRepo(sess.RepoURL, repoName, token)
		stepT = t("repo_fetch", stepT)
		if err != nil {
			slog.Warn("session: bare repo fetch", "session", sid, "err", err)
		} else {
			destPath, err := m.repoCache.LocalClone(barePath, sess.ID, repoName, sess.Branch, sess.RepoURL)
			t("local_clone", stepT)
			if err != nil {
				slog.Warn("session: local clone", "session", sid, "err", err)
			} else {
				m.mu.Lock()
				sess.workspacePath = destPath
				m.mu.Unlock()
			}
		}
	}

	// 1b. Create snapshot (Provisioning state)
	stepT := time.Now()
	device, err := m.snap.CreateSnapshot(sess.ID)
	t("snapshot_create", stepT)
	if err != nil {
		fail(fmt.Errorf("create snapshot: %w", err))
		return
	}

	// 2. Inject network config (still Provisioning)
	stepT = time.Now()
	if err := m.snap.InjectNetworkConfig(device, vm.SlotIP(sess.Slot), "172.16.0.1"); err != nil {
		t("inject_network", stepT)
		m.snap.DeleteSnapshot(sess.ID) //nolint:errcheck
		fail(fmt.Errorf("inject network: %w", err))
		return
	}
	t("inject_network", stepT)

	// 3. Boot VM → Booting
	if err := sess.sm.Transition(session.StateBooting); err != nil {
		m.snap.DeleteSnapshot(sess.ID) //nolint:errcheck
		slog.Error("session: transition error", "session", sid, "err", err)
		return
	}
	memMiB := int64(512)
	if sess.DevMode {
		memMiB = 2048
	}

	stepT = time.Now()
	v, err := m.launcher.Launch(ctx, sess.Slot, device, memMiB)
	t("launch_vm", stepT)
	if err != nil {
		m.snap.DeleteSnapshot(sess.ID) //nolint:errcheck
		fail(fmt.Errorf("launch vm: %w", err))
		return
	}
	sess.v = v

	// 4. Wait for SSH → WaitingSSH
	if err := sess.sm.Transition(session.StateWaitingSSH); err != nil {
		v.Stop(ctx)                    //nolint:errcheck
		m.snap.DeleteSnapshot(sess.ID) //nolint:errcheck
		slog.Error("session: transition error", "session", sid, "err", err)
		return
	}
	stepT = time.Now()
	if err := v.WaitForSSH(ctx); err != nil {
		t("wait_ssh", stepT)
		v.Stop(ctx)                    //nolint:errcheck
		m.snap.DeleteSnapshot(sess.ID) //nolint:errcheck
		fail(fmt.Errorf("wait for ssh: %w", err))
		return
	}
	t("wait_ssh", stepT)

	// 5. Git setup → Configuring
	if err := sess.sm.Transition(session.StateConfiguring); err != nil {
		slog.Error("session: transition error", "session", sid, "err", err)
		return
	}

	vmAddr := fmt.Sprintf("%s:%d", vm.SlotIP(sess.Slot), vm.VMSSHPort)
	bridge := m.bridges.NewBridge(vmAddr, m.signer)
	sess.bridge = bridge

	// Ensure claude's authorized_keys exists. The new rootfs has this baked in,
	// but the existing rootfs only has root's key. A root bridge write is
	// idempotent — it's a no-op when the key is already present.
	if m.signer != nil {
		pubKey := strings.TrimRight(string(ssh.MarshalAuthorizedKey(m.signer.PublicKey())), "\n")
		rootBridge := terminal.NewBridge(vmAddr, "root", m.signer)
		stepT = time.Now()
		if err := rootBridge.RunSetup([]string{
			"passwd -u claude 2>/dev/null || true", // adduser -D locks the account; unlock for key auth
			"mkdir -p /home/claude/.ssh",
			"echo " + pubKey + " > /home/claude/.ssh/authorized_keys",
			"chmod 700 /home/claude/.ssh",
			"chmod 600 /home/claude/.ssh/authorized_keys",
			"chown -R claude:claude /home/claude/.ssh",
		}); err != nil {
			slog.Warn("session: inject claude pubkey", "session", sid, "err", err)
		}
		t("configure_key", stepT)
	}

	// token was resolved at the top of boot() before the snapshot was created.
	if token != "" {
		if githubToken != "" {
			if err := saveToken(m.credPath, githubToken); err != nil {
				slog.Warn("session: save token", "session", sid, "err", err)
			}
		}
		cmds := []string{
			"git config --global credential.helper store",
			fmt.Sprintf(`printf 'https://oauth2:%s@github.com\n' > ~/.git-credentials`, token),
		}
		stepT = time.Now()
		if err := bridge.RunSetup(cmds); err != nil {
			slog.Warn("session: git credential setup", "session", sid, "err", err)
		}
		t("configure_git", stepT)
	} else {
		slog.Warn("session: no GitHub token available, private repos will fail", "session", sid)
	}

	// Mount the workspace into the VM via NFS and verify it landed.
	// workspacePath is set when the host-side bare-repo clone succeeded.
	if sess.RepoURL != "" {
		repoName := gitutil.RepoName(sess.RepoURL)
		if repoName == "" {
			repoName = "repo"
		}
		m.mu.RLock()
		hostWS := sess.workspacePath
		m.mu.RUnlock()
		if hostWS == "" {
			fail(fmt.Errorf("workspace not prepared for %s: bare-repo clone failed earlier", sess.RepoURL))
			return
		}
		stepT = time.Now()
		if err := mountWorkspace(bridge, m.devHostIP, hostWS, repoName); err != nil {
			fail(fmt.Errorf("mount workspace: %w", err))
			return
		}
		t("mount_workspace", stepT)
	}

	// Mount the shared Go build cache so incremental builds stay under the
	// VM's RAM limit.  Non-fatal: a miss here degrades to cold-cache builds.
	stepT = time.Now()
	if err := mountGoCache(bridge, m.devHostIP); err != nil {
		slog.Warn("session: mount go-cache", "session", sid, "err", err)
	}
	t("mount_go_cache", stepT)

	// Set a unique per-session hostname so the prompt shows the session ID.
	// claude has NOPASSWD sudo baked into the rootfs, so no privilege escalation needed.
	vmHostname := "rubbish-" + sid
	stepT = time.Now()
	if err := bridge.RunSetup([]string{
		"sudo hostname " + vmHostname,
		"echo " + vmHostname + " | sudo tee /etc/hostname > /dev/null",
	}); err != nil {
		slog.Warn("session: set hostname", "session", sid, "err", err)
	}
	t("configure_hostname", stepT)

	// Inject profile credentials and Go toolchain config into the VM environment.
	// GOTOOLCHAIN=local prevents Go from attempting to download a newer toolchain
	// when go.mod specifies a version newer than what's installed in the rootfs.
	if err := bridge.RunSetup([]string{
		"printf 'export GOTOOLCHAIN=local\\nexport PATH=/usr/local/go/bin:$PATH\\n' >> ~/.profile",
	}); err != nil {
		slog.Warn("session: inject go env", "session", sid, "err", err)
	}

	if m.prof != nil {
		if p, err := m.prof.Load(); err == nil && p.ClaudeOAuthToken != "" {
			// Build a node one-liner that merges our settings into ~/.claude.json.
			// Using node avoids a JSON dependency in the rootfs and lets us read
			// the actual installed Claude version so release-notes prompts are skipped.
			wsPath := ""
			if repoName != "" {
				wsPath = "/home/claude/workspace/" + repoName
			}
			nodeScript := fmt.Sprintf(
				`const fs=require('fs'),h=require('os').homedir(),p=h+'/.claude.json';`+
					`let c={};try{c=JSON.parse(fs.readFileSync(p,'utf8'))}catch(_){}c.hasCompletedOnboarding=true;`+
					`try{const v=require('child_process').execSync('claude --version 2>/dev/null').toString().split(' ')[0].trim();`+
					`c.lastOnboardingVersion=v;c.lastReleaseNotesSeen=v}catch(_){}if(!c.projects)c.projects={};`+
					`const wp=%q;if(wp){if(!c.projects[wp])c.projects[wp]={};c.projects[wp].hasTrustDialogAccepted=true;}`+
					`fs.writeFileSync(p,JSON.stringify(c),{mode:0o600})`,
				wsPath,
			)
			cmds := []string{
				fmt.Sprintf("printf 'export CLAUDE_CODE_OAUTH_TOKEN=%s\\n' >> ~/.profile", p.ClaudeOAuthToken),
				"chmod 600 ~/.profile",
				"node -e " + shellQuote(nodeScript),
			}
			stepT = time.Now()
			if err := bridge.RunSetup(cmds); err != nil {
				slog.Warn("session: inject credentials", "session", sid, "err", err)
			}
			t("configure_profile", stepT)
		}
	}

	// 6. Dev seed (optional)
	if sess.DevMode {
		stepT = time.Now()
		if err := seedDevFiles(bridge, m.devHostIP); err != nil {
			slog.Warn("session: dev seed", "session", sid, "err", err)
		}
		t("configure_seed", stepT)
	}

	// 7. Ready
	if err := sess.sm.Transition(session.StateReady); err != nil {
		slog.Error("session: transition error", "session", sid, "err", err)
		return
	}
	slog.Info("session: ready", "session", sid, "slot", sess.Slot, "ip", vm.SlotIP(sess.Slot), "total", time.Since(bootStart).Round(time.Millisecond))

	// 8. Non-interactive path: run claude -p, capture output, then stop.
	if sess.Role != "interactive" {
		slog.Info("session: non-interactive mode, running claude -p", "session", sid, "role", sess.Role)

		// --output-format json sends errors (including usage limits) to stderr and
		// structured output to stdout, letting us distinguish them cleanly.
		innerCmd := "claude --dangerously-skip-permissions --output-format json -p " + shellQuote(sess.Prompt)
		cmd := "bash -lc " + shellQuote(innerCmd)
		stdout, stderr, cmdErr := sess.bridge.RunCapture(cmd)
		if cmdErr != nil {
			slog.Warn("session: claude exit error", "session", sid, "err", cmdErr)
		}

		// Check stderr for usage-limit message before touching the result.
		// With --output-format json, all errors including limit hits go to stderr.
		if workflow.IsUsageLimitError(stderr) {
			slog.Warn("session: usage limit detected", "session", sid)
			m.mu.Lock()
			sess.Error = "usage limit reached"
			m.mu.Unlock()
			if err := sess.sm.Transition(session.StateFailed); err != nil {
				slog.Error("session: transition to failed error", "session", sid, "err", err)
			}
			m.persistSession(sess)
			sess.cancel()
			if sess.v != nil {
				stopCtx, stopCancel := context.WithTimeout(context.Background(), 8*time.Second)
				if err := sess.v.Stop(stopCtx); err != nil {
					slog.Warn("session: stop vm after usage limit", "session", sid, "err", err)
				}
				stopCancel()
				vm.WaitForVMDead(vm.SlotIP(sess.Slot))
			}
			m.snap.DeleteSnapshot(sess.ID) //nolint:errcheck
			m.freeSlot(sess.Slot)
			m.mu.Lock()
			delete(m.sessions, sess.ID)
			m.mu.Unlock()
			if m.repoCache != nil {
				m.repoCache.CleanupWorkspace(sess.ID)
			}
			return
		}

		// Parse the JSON result from stdout; fall back to raw text for compatibility.
		result, _ := parseClaudeJSON(stdout)

		m.mu.Lock()
		sess.Result = result
		m.mu.Unlock()

		slog.Info("session: claude -p finished, stopping", "session", sid, "bytes", len(result))

		// Transition to Stopping and run cleanup inline (we're already in a goroutine).
		if err := sess.sm.Transition(session.StateStopping); err != nil {
			slog.Error("session: transition to stopping error", "session", sid, "err", err)
		}
		// Persist result before stopping.
		m.persistSession(sess)

		sess.cancel()
		if sess.v != nil {
			stopCtx, stopCancel := context.WithTimeout(context.Background(), 8*time.Second)
			if err := sess.v.Stop(stopCtx); err != nil {
				slog.Warn("session: stop vm", "session", sid, "err", err)
			}
			stopCancel()
			vm.WaitForVMDead(vm.SlotIP(sess.Slot))
		}
		m.snap.DeleteSnapshot(sess.ID) //nolint:errcheck
		m.freeSlot(sess.Slot)

		sess.sm.Transition(session.StateStopped) //nolint:errcheck
		// Persist final stopped state with result before removing from map.
		m.persistSession(sess)

		m.mu.Lock()
		delete(m.sessions, sess.ID)
		m.mu.Unlock()

		if m.repoCache != nil {
			m.repoCache.CleanupWorkspace(sess.ID)
		}
		slog.Info("session: non-interactive session stopped and removed", "session", sid)
	}
}

func (m *SessionManager) Get(id string) (*Session, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.sessions[id]
	return s, ok
}

func (m *SessionManager) List() []*Session {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out
}

func (m *SessionManager) Stop(id string) error {
	m.mu.RLock()
	sess, ok := m.sessions[id]
	m.mu.RUnlock()
	if !ok {
		return fmt.Errorf("session %s not found", id)
	}

	if err := sess.sm.Transition(session.StateStopping); err != nil {
		if !errors.Is(err, session.ErrInvalidTransition) {
			return err
		}
		switch sess.sm.State() {
		case session.StateStopped, session.StateFailed:
			m.mu.Lock()
			delete(m.sessions, id)
			m.mu.Unlock()
			if m.store != nil {
				m.store.Delete(id) //nolint:errcheck
			}
			return nil
		case session.StateStopping:
			// Cleanup goroutine already running; caller gets success immediately.
			return nil
		default:
			return err
		}
	}

	// Run the blocking cleanup (VM shutdown, dmsetup, workspace removal) in a
	// goroutine so the HTTP DELETE handler returns immediately.  The session
	// stays in the map as StateStopping until the goroutine finishes.
	go func() {
		sess.cancel()
		if sess.v != nil {
			stopCtx, stopCancel := context.WithTimeout(context.Background(), 8*time.Second)
			if err := sess.v.Stop(stopCtx); err != nil {
				slog.Warn("session: stop vm", "session", id[:8], "err", err)
			}
			stopCancel()
			// Wait until port 22 stops answering before freeing the slot.
			// Without this a new session can grab the same slot and have its
			// WaitForSSH succeed immediately against the dying VM.
			vm.WaitForVMDead(vm.SlotIP(sess.Slot))
		}
		m.snap.DeleteSnapshot(id) //nolint:errcheck
		m.freeSlot(sess.Slot)

		sess.sm.Transition(session.StateStopped) //nolint:errcheck

		m.mu.Lock()
		delete(m.sessions, id)
		m.mu.Unlock()
		if m.store != nil {
			m.store.Delete(id) //nolint:errcheck
		}
		slog.Info("session: stopped and removed", "session", id[:8])

		// NFS workspace cleanup runs last: if the VM died while holding the
		// NFS mount open, os.RemoveAll blocks until the NFS server TCP-keepalive
		// times out the dead client (~60-90s). Doing this after the session is
		// removed from the map means users see "stopped" immediately.
		if m.repoCache != nil {
			m.repoCache.CleanupWorkspace(id)
		}
	}()

	return nil
}

func (m *SessionManager) Restart(parentCtx context.Context, id string) error {
	m.mu.Lock()
	sess, ok := m.sessions[id]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("session %s not found", id)
	}
	if sess.sm.State() != session.StateStopped {
		m.mu.Unlock()
		return fmt.Errorf("session %s is not stopped (status: %s)", id, sess.sm.State())
	}
	slot, ok := m.allocSlot()
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("no slots available (max %d concurrent sessions)", m.maxSlots)
	}
	ctx, cancel := context.WithCancel(parentCtx)
	sess.Slot = slot
	sess.cancel = cancel
	sess.v = nil
	sess.bridge = nil
	sess.Error = ""
	sess.sm = m.newSM(sess, session.StateProvisioning)
	m.mu.Unlock()

	m.persistSession(sess) // initial provisioning state for the restart
	go m.boot(ctx, sess, "")
	return nil
}

func (m *SessionManager) StopAll() {
	m.mu.RLock()
	ids := make([]string, 0, len(m.sessions))
	for id := range m.sessions {
		ids = append(ids, id)
	}
	m.mu.RUnlock()
	for _, id := range ids {
		m.Stop(id) //nolint:errcheck
	}
}

// ---- workflow.SessionStarter adapter methods --------------------------------

// CreateSession implements workflow.SessionStarter. It creates a non-interactive
// session and returns its ID. Uses the server context so the boot goroutine
// isn't canceled when the calling HTTP request finishes.
func (m *SessionManager) CreateSession(ctx context.Context, role, prompt, repoURL, branch string) (string, error) {
	bootCtx := m.serverCtx
	if bootCtx == nil {
		bootCtx = context.Background()
	}
	sess, err := m.Create(bootCtx, repoURL, branch, "", false, role, prompt)
	if err != nil {
		return "", err
	}
	return sess.ID, nil
}

// WaitForSession implements workflow.SessionStarter. It polls the session store
// every 2 seconds until the session reaches StateStopped (returns Result) or
// StateFailed (returns an error). Returns ctx.Err() if the context expires.
func (m *SessionManager) WaitForSession(ctx context.Context, id string) (string, error) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-ticker.C:
			if m.store != nil {
				row, ok, err := m.store.Get(id)
				if err != nil {
					return "", fmt.Errorf("WaitForSession store.Get: %w", err)
				}
				if !ok {
					return "", fmt.Errorf("session %s not found", id)
				}
				switch session.State(row.Status) {
				case session.StateStopped:
					return row.Result, nil
				case session.StateFailed:
					if strings.Contains(row.ErrorMsg, "usage limit") {
						return "", fmt.Errorf("session failed: %s: %w", row.ErrorMsg, workflow.ErrUsageLimit)
					}
					return "", fmt.Errorf("session failed: %s", row.ErrorMsg)
				}
				continue
			}
			// Fallback: use in-memory map when there is no persistent store.
			m.mu.RLock()
			sess, ok := m.sessions[id]
			m.mu.RUnlock()
			if !ok {
				return "", fmt.Errorf("session %s not found", id)
			}
			switch sess.sm.State() {
			case session.StateStopped:
				m.mu.RLock()
				result := sess.Result
				m.mu.RUnlock()
				return result, nil
			case session.StateFailed:
				m.mu.RLock()
				sessErr := sess.Error
				m.mu.RUnlock()
				if strings.Contains(sessErr, "usage limit") {
					return "", fmt.Errorf("session failed: %s: %w", sessErr, workflow.ErrUsageLimit)
				}
				return "", fmt.Errorf("session failed: %s", sessErr)
			}
		}
	}
}

// ---- mcp.SessionHandler adapter methods ------------------------------------

// GetSession implements mcp.SessionHandler. Returns the sessionstore.Row cast to any.
func (m *SessionManager) GetSession(id string) (any, bool) {
	if m.store != nil {
		row, ok, err := m.store.Get(id)
		if err != nil || !ok {
			return nil, false
		}
		return row, true
	}
	// Fallback: build a Row from the in-memory session.
	m.mu.RLock()
	sess, ok := m.sessions[id]
	m.mu.RUnlock()
	if !ok {
		return nil, false
	}
	m.mu.RLock()
	row := sessionstore.Row{
		ID:        sess.ID,
		Slot:      sess.Slot,
		Status:    string(sess.sm.State()),
		RepoURL:   sess.RepoURL,
		Branch:    sess.Branch,
		ErrorMsg:  sess.Error,
		DevMode:   sess.DevMode,
		CreatedAt: sess.CreatedAt,
		Role:      sess.Role,
		Prompt:    sess.Prompt,
		Result:    sess.Result,
	}
	m.mu.RUnlock()
	return row, true
}

// ListSessions implements mcp.SessionHandler. Returns all session rows as []any.
func (m *SessionManager) ListSessions() ([]any, error) {
	if m.store != nil {
		rows, err := m.store.List()
		if err != nil {
			return nil, err
		}
		out := make([]any, len(rows))
		for i, r := range rows {
			out[i] = r
		}
		return out, nil
	}
	// Fallback: build from in-memory sessions.
	sessions := m.List()
	out := make([]any, len(sessions))
	for i, sess := range sessions {
		m.mu.RLock()
		out[i] = sessionstore.Row{
			ID:        sess.ID,
			Slot:      sess.Slot,
			Status:    string(sess.sm.State()),
			RepoURL:   sess.RepoURL,
			Branch:    sess.Branch,
			ErrorMsg:  sess.Error,
			DevMode:   sess.DevMode,
			CreatedAt: sess.CreatedAt,
			Role:      sess.Role,
			Prompt:    sess.Prompt,
			Result:    sess.Result,
		}
		m.mu.RUnlock()
	}
	return out, nil
}

// StopSession implements mcp.SessionHandler. Delegates to Stop().
func (m *SessionManager) StopSession(id string) error {
	return m.Stop(id)
}

// loadFromDB populates the manager with sessions persisted in the store.
// For ready sessions whose Firecracker process is still alive (server restart),
// it reconnects them using a DetachedVM. For sessions that cannot be recovered
// it marks them failed. True orphan FC processes are killed.
// Must be called before the server starts accepting requests.
func (m *SessionManager) loadFromDB(parentCtx context.Context) {
	if m.store == nil {
		return
	}
	rows, err := m.store.List()
	if err != nil {
		slog.Warn("startup: load sessions from DB", "err", err)
		return
	}

	liveSlots := make(map[int]bool)

	for _, row := range rows {
		status := session.State(row.Status)
		ctx, cancel := context.WithCancel(parentCtx)

		sess := &Session{
			ID:        row.ID,
			Slot:      row.Slot,
			RepoURL:   row.RepoURL,
			Branch:    row.Branch,
			DevMode:   row.DevMode,
			CreatedAt: row.CreatedAt,
			Error:     row.ErrorMsg,
			cancel:    cancel,
		}

		switch status {
		case session.StateReady:
			if vm.IsSocketAlive(row.Slot) {
				// Server process restarted but the FC VM is still running — reconnect.
				sess.sm = m.newSM(sess, session.StateReady)
				sess.v = vm.NewDetachedVM(row.Slot)
				vmAddr := fmt.Sprintf("%s:%d", vm.SlotIP(row.Slot), vm.VMSSHPort)
				sess.bridge = m.bridges.NewBridge(vmAddr, m.signer)
				liveSlots[row.Slot] = true
				slog.Info("startup: recovered ready session", "session", row.ID[:8], "slot", row.Slot, "ip", vm.SlotIP(row.Slot))
			} else {
				// Host restarted — VM is gone.
				cancel()
				ctx, cancel = context.WithCancel(parentCtx)
				sess.cancel = cancel
				sess.Error = "lost on host restart"
				sess.sm = m.newSM(sess, session.StateFailed)
				m.store.Upsert(sessionstore.Row{ //nolint:errcheck
					ID: row.ID, Slot: row.Slot, Status: string(session.StateFailed),
					RepoURL: row.RepoURL, Branch: row.Branch, ErrorMsg: sess.Error,
					DevMode: row.DevMode, CreatedAt: row.CreatedAt, UpdatedAt: time.Now(),
				})
				slog.Warn("startup: session was ready but VM is gone, marked failed", "session", row.ID[:8])
			}

		case session.StateProvisioning, session.StateBooting,
			session.StateWaitingSSH, session.StateConfiguring, session.StateStopping:
			// Mid-boot / mid-stop state — kill any surviving FC and mark failed.
			cancel()
			ctx, cancel = context.WithCancel(parentCtx)
			sess.cancel = cancel
			if vm.IsSocketAlive(row.Slot) {
				d := vm.NewDetachedVM(row.Slot)
				d.Stop(context.Background()) //nolint:errcheck
			}
			sess.Error = "lost on server restart"
			sess.sm = m.newSM(sess, session.StateFailed)
			m.store.Upsert(sessionstore.Row{ //nolint:errcheck
				ID: row.ID, Slot: row.Slot, Status: string(session.StateFailed),
				RepoURL: row.RepoURL, Branch: row.Branch, ErrorMsg: sess.Error,
				DevMode: row.DevMode, CreatedAt: row.CreatedAt, UpdatedAt: time.Now(),
			})
			slog.Warn("startup: session in mid-boot state, marked failed", "session", row.ID[:8], "status", status)

		default: // stopped, failed — terminal states, load as-is
			cancel()
			sess.cancel = func() {}
			sess.sm = m.newSM(sess, status)
			slog.Info("startup: loaded terminal session", "session", row.ID[:8], "status", status)
		}

		_ = ctx // context held by sess.cancel for live sessions; suppresses unused-variable warning

		m.mu.Lock()
		m.sessions[row.ID] = sess
		if liveSlots[row.Slot] {
			m.usedSlots[row.Slot] = true
		}
		m.mu.Unlock()
	}

	// Kill any FC sockets that have no corresponding DB session.
	if err := vm.CleanupOrphansExcept(m.maxSlots, liveSlots); err != nil {
		slog.Warn("startup: cleanup orphans", "err", err)
	}

}

// normalizeRepoURL converts SSH git URLs to HTTPS so the token credential
// helper works regardless of which format the user pastes.
// e.g. "git@github.com:user/repo.git" → "https://github.com/user/repo.git"
func normalizeRepoURL(rawURL string) string {
	if strings.HasPrefix(rawURL, "git@") {
		s := strings.TrimPrefix(rawURL, "git@")
		if idx := strings.Index(s, ":"); idx >= 0 {
			s = s[:idx] + "/" + s[idx+1:]
		}
		return "https://" + s
	}
	return rawURL
}

const devSeedDir = "/opt/rubbish/dev-seed"

// nfsBase is the host path exported via NFS for shared Claude memory.
const nfsBase = "/opt/rubbish/claude-shared"

// mountGoCache NFS-mounts the shared Go build cache into the VM at the
// default GOCACHE path.  Multiple VMs share the same cache safely because
// Go's cache uses content-addressed files written via atomic rename.
func mountGoCache(bridge setupRunner, hostIP string) error {
	const (
		hostPath  = "/opt/rubbish/go-cache"
		guestPath = "/home/claude/.cache/go-build"
	)
	return bridge.RunSetup([]string{
		"mkdir -p " + guestPath,
		fmt.Sprintf("sudo mount -t nfs %s:%s %s -o vers=4,noatime,soft", hostIP, hostPath, guestPath),
	})
}

// mountWorkspace NFS-mounts the session's host workspace directory into the VM
// at /home/claude/workspace/<repoName>.  The host must export
// /opt/rubbish/workspaces via NFS to the VM bridge subnet.
func mountWorkspace(bridge setupRunner, hostIP, hostPath, repoName string) error {
	guestMount := "/home/claude/workspace/" + repoName
	return bridge.RunSetup([]string{
		"sudo mkdir -p " + guestMount,
		fmt.Sprintf("sudo mount -t nfs %s:%s %s -o vers=4,noatime,soft", hostIP, hostPath, guestMount),
	})
}

// seedDevFiles injects developer context into a VM:
//   - Global CLAUDE.md — base64-injected (static, no sync needed)
//   - Global memory + project memory — NFS-mounted so all dev sessions share
//     live read/write access to the same files on the host
//   - Deploy SSH key + SSH config + known_hosts — base64-injected
func seedDevFiles(bridge setupRunner, hostIP string) error {
	const (
		nfsGlobalMem = nfsBase + "/memory"
		nfsProjects  = nfsBase + "/projects"
		vmHome       = "/home/claude"
		vmGlobalMem  = vmHome + "/.claude/memory"
		vmProjects   = vmHome + "/.claude/projects"
		vmSSHDir     = vmHome + "/.ssh"
		nfsMountOpts = "vers=4,noatime,soft"
	)

	// injectFile base64-encodes a host file and writes it into the VM.
	injectFile := func(srcPath, dstPath string) error {
		data, err := os.ReadFile(srcPath)
		if err != nil {
			return fmt.Errorf("read %s: %w", srcPath, err)
		}
		encoded := base64.StdEncoding.EncodeToString(data)
		cmd := fmt.Sprintf("echo '%s' | base64 -d > %s", encoded, dstPath)
		return bridge.RunSetup([]string{cmd})
	}

	// Create directory structure under claude's home.
	if err := bridge.RunSetup([]string{
		"mkdir -p " + vmHome + "/.claude",
		"mkdir -p " + vmGlobalMem,
		"mkdir -p " + vmProjects,
		"mkdir -p " + vmSSHDir,
	}); err != nil {
		return fmt.Errorf("seed mkdirs: %w", err)
	}

	// CLAUDE.md — static, base64-injected (does not need live sync).
	if err := injectFile(devSeedDir+"/claude/CLAUDE.md", vmHome+"/.claude/CLAUDE.md"); err != nil {
		slog.Warn("seed: CLAUDE.md", "err", err)
	}

	// Memory dirs — NFS-mounted for live shared read/write across all dev sessions.
	// Needs sudo since bridge connects as claude (who has NOPASSWD sudo in the rootfs).
	if err := bridge.RunSetup([]string{
		fmt.Sprintf("sudo mount -t nfs %s:%s %s -o %s", hostIP, nfsGlobalMem, vmGlobalMem, nfsMountOpts),
		fmt.Sprintf("sudo mount -t nfs %s:%s %s -o %s", hostIP, nfsProjects, vmProjects, nfsMountOpts),
	}); err != nil {
		return fmt.Errorf("nfs mount: %w", err)
	}
	slog.Info("seed: NFS memory mounts established", "host", hostIP)

	// Deploy SSH key (fatal — without it the VM can't deploy)
	if err := injectFile(devSeedDir+"/id_deploy", vmSSHDir+"/id_deploy"); err != nil {
		return fmt.Errorf("inject deploy key: %w", err)
	}
	if err := bridge.RunSetup([]string{"chmod 600 " + vmSSHDir + "/id_deploy"}); err != nil {
		return fmt.Errorf("chmod deploy key: %w", err)
	}

	// SSH config so `ssh 192.168.1.35` uses the deploy key as claude.
	// base64-injected to avoid shell quoting issues with embedded newlines.
	sshConfig := fmt.Sprintf("Host %s\n  User claude\n  IdentityFile %s/id_deploy\n  StrictHostKeyChecking yes\n", hostIP, vmSSHDir)
	sshConfigB64 := base64.StdEncoding.EncodeToString([]byte(sshConfig))
	if err := bridge.RunSetup([]string{
		"echo " + sshConfigB64 + " | base64 -d > " + vmSSHDir + "/config",
		"chmod 600 " + vmSSHDir + "/config",
	}); err != nil {
		return fmt.Errorf("ssh config: %w", err)
	}

	// Pre-trust the host fingerprint so first deploy doesn't prompt
	knownHosts, err := os.ReadFile(devSeedDir + "/known_hosts")
	if err == nil && len(knownHosts) > 0 {
		if err := injectFile(devSeedDir+"/known_hosts", vmSSHDir+"/known_hosts"); err != nil {
			slog.Warn("seed: known_hosts", "err", err)
		} else {
			bridge.RunSetup([]string{"chmod 644 " + vmSSHDir + "/known_hosts"}) //nolint:errcheck
		}
	}

	slog.Info("seed: dev files injected", "host", hostIP)
	return nil
}

// ---- Credential helpers -----------------------------------------------------

func loadSavedToken(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func saveToken(path, token string) error {
	return os.WriteFile(path, []byte(token), 0600)
}

// ---- HTTP handlers ----------------------------------------------------------

// shellQuote wraps s in single quotes safe for POSIX shell, escaping any
// embedded single quotes via the '"'"' idiom.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func registerHandlers(mux *http.ServeMux, mgr *SessionManager, engine *workflow.Engine, rootCtx context.Context) {
	mux.HandleFunc("/api/saved-token", func(w http.ResponseWriter, r *http.Request) {
		has := loadSavedToken(mgr.credPath) != ""
		writeJSON(w, http.StatusOK, map[string]bool{"has_token": has})
	})

	mux.HandleFunc("/api/profile", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			resp := map[string]string{"claude_oauth_token": "", "github_token": ""}
			if mgr.prof != nil {
				if p, err := mgr.prof.Load(); err == nil {
					resp["claude_oauth_token"] = profile.MaskToken(p.ClaudeOAuthToken)
					resp["github_token"] = profile.MaskToken(p.GitHubToken)
				}
			}
			writeJSON(w, http.StatusOK, resp)

		case http.MethodPut:
			var body struct {
				ClaudeOAuthToken string `json:"claude_oauth_token"`
				GitHubToken      string `json:"github_token"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			if mgr.prof != nil {
				// Merge: load existing so omitted fields aren't wiped.
				existing, _ := mgr.prof.Load()
				if body.ClaudeOAuthToken != "" {
					existing.ClaudeOAuthToken = body.ClaudeOAuthToken
				}
				if body.GitHubToken != "" {
					existing.GitHubToken = body.GitHubToken
				}
				if err := mgr.prof.Save(existing); err != nil {
					http.Error(w, "failed to save profile", http.StatusInternalServerError)
					return
				}
			}
			w.WriteHeader(http.StatusNoContent)

		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})

	mux.HandleFunc("/profile", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write(profileHTML)
	})

	mux.HandleFunc("/orchestrator", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write(orchestratorHTML)
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		w.Write(indexHTML)
	})

	mux.HandleFunc("/api/sessions", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, http.StatusOK, mgr.List())

		case http.MethodPost:
			var body struct {
				RepoURL     string `json:"repo_url"`
				Branch      string `json:"branch"`
				GithubToken string `json:"github_token"`
				DevMode     bool   `json:"dev_mode"`
				Role        string `json:"role"`
				Prompt      string `json:"prompt"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			sess, err := mgr.Create(rootCtx, body.RepoURL, body.Branch, body.GithubToken, body.DevMode, body.Role, body.Prompt)
			if err != nil {
				http.Error(w, err.Error(), http.StatusServiceUnavailable)
				return
			}
			writeJSON(w, http.StatusAccepted, sess)

		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})

	mux.HandleFunc("/api/sessions/", func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/api/sessions/")

		if strings.HasSuffix(path, "/result") {
			if r.Method != http.MethodGet {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			id := strings.TrimSuffix(path, "/result")
			sess, ok := mgr.Get(id)
			if !ok {
				http.Error(w, "session not found", http.StatusNotFound)
				return
			}
			if sess.SessionStatus() != session.StateStopped {
				http.Error(w, "result not yet available", http.StatusConflict)
				return
			}
			writeJSON(w, http.StatusOK, map[string]string{"result": sess.Result})
			return
		}

		if strings.HasSuffix(path, "/start") {
			if r.Method != http.MethodPost {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			id := strings.TrimSuffix(path, "/start")
			if err := mgr.Restart(rootCtx, id); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			w.WriteHeader(http.StatusAccepted)
			return
		}

		if strings.HasSuffix(path, "/favorite") {
			if r.Method != http.MethodPost {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			if mgr.store == nil {
				http.Error(w, "store not available", http.StatusServiceUnavailable)
				return
			}
			id := strings.TrimSuffix(path, "/favorite")
			sess, ok := mgr.Get(id)
			if !ok {
				http.Error(w, "session not found", http.StatusNotFound)
				return
			}
			name := gitutil.RepoName(sess.RepoURL)
			if name == "" {
				name = sess.ID[:8]
			}
			fav := sessionstore.Favorite{
				ID:        uuid.New().String(),
				Name:      name,
				RepoURL:   sess.RepoURL,
				Branch:    sess.Branch,
				CreatedAt: time.Now(),
			}
			if err := mgr.store.UpsertFavorite(fav); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			writeJSON(w, http.StatusCreated, fav)
			return
		}

		if r.Method != http.MethodDelete {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if err := mgr.Stop(path); err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("/api/favorites", func(w http.ResponseWriter, r *http.Request) {
		if mgr.store == nil {
			http.Error(w, "store not available", http.StatusServiceUnavailable)
			return
		}
		switch r.Method {
		case http.MethodGet:
			favs, err := mgr.store.ListFavorites()
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			if favs == nil {
				favs = []sessionstore.Favorite{}
			}
			writeJSON(w, http.StatusOK, favs)

		case http.MethodPost:
			var body struct {
				Name    string `json:"name"`
				RepoURL string `json:"repo_url"`
				Branch  string `json:"branch"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			fav := sessionstore.Favorite{
				ID:        uuid.New().String(),
				Name:      body.Name,
				RepoURL:   body.RepoURL,
				Branch:    body.Branch,
				CreatedAt: time.Now(),
			}
			if err := mgr.store.UpsertFavorite(fav); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			writeJSON(w, http.StatusCreated, fav)

		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})

	mux.HandleFunc("/api/favorites/", func(w http.ResponseWriter, r *http.Request) {
		if mgr.store == nil {
			http.Error(w, "store not available", http.StatusServiceUnavailable)
			return
		}
		if r.Method != http.MethodDelete {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		id := strings.TrimPrefix(r.URL.Path, "/api/favorites/")
		if err := mgr.store.DeleteFavorite(id); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("/api/workflows/", func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/api/workflows/")
		// GET /api/workflows/ — list all workflows
		if path == "" && r.Method == http.MethodGet {
			if engine == nil {
				writeJSON(w, http.StatusOK, []any{})
				return
			}
			writeJSON(w, http.StatusOK, engine.List())
			return
		}
		// POST /api/workflows/{id}/resume
		id, action, _ := strings.Cut(path, "/")
		if r.Method != http.MethodPost || action != "resume" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if engine == nil {
			http.Error(w, "workflow engine not available", http.StatusServiceUnavailable)
			return
		}
		if err := engine.Resume(rootCtx, id); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "resuming", "workflow_id": id})
	})

	// POST /api/dev/test-workflow — injects a simulated workflow that cycles
	// through all stages without calling Claude. Safe to use in production for
	// UI testing; each invocation creates one workflow run.
	mux.HandleFunc("/api/dev/test-workflow", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if engine == nil {
			http.Error(w, "workflow engine not available", http.StatusServiceUnavailable)
			return
		}
		id, err := engine.InjectDemoWorkflow(rootCtx)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]string{"workflow_id": id})
	})

}

// ---- main -------------------------------------------------------------------

func main() {
	keyFlag := flag.String("key", "", "path to SSH private key for VM access")
	slotsFlag := flag.Int("slots", 4, "max concurrent VM sessions (0 = unlimited)")
	devHostIPFlag := flag.String("dev-host-ip", "192.168.1.35", "host IP for NFS mounts in dev-mode sessions")
	flag.Parse()

	if *keyFlag == "" {
		if flag.NArg() > 0 {
			*keyFlag = flag.Arg(0)
		} else {
			slog.Error("usage: rubbish-poc --key <path-to-ssh-private-key>"); os.Exit(1)
		}
	}

	keyBytes, err := os.ReadFile(*keyFlag)
	if err != nil {
		slog.Error("read ssh key", "err", err); os.Exit(1)
	}
	signer, err := ssh.ParsePrivateKey(keyBytes)
	if err != nil {
		slog.Error("parse ssh key", "err", err); os.Exit(1)
	}

	snapMgr, err := fc.NewSnapshotManager("/dev/mapper/rubbish-pool", "/opt/rubbish/dm/next-volume-id")
	if err != nil {
		slog.Error("snapshot manager", "err", err); os.Exit(1)
	}

	credPath := "/opt/rubbish/creds/github-token"
	if err := os.MkdirAll("/opt/rubbish/creds", 0700); err != nil {
		slog.Error("create creds dir", "err", err); os.Exit(1)
	}

	store, err := sessionstore.Open("/opt/rubbish/sessions.db")
	if err != nil {
		slog.Error("open session store", "err", err); os.Exit(1)
	}
	defer store.Close()

	prof := profile.NewStore("/opt/rubbish/creds/profile.json")
	mgr := NewSessionManager(snapMgr, signer, credPath, prof, store, *slotsFlag, *devHostIPFlag)

	slog.Info("startup: recovering sessions from DB")
	mgr.loadFromDB(context.Background())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	wfStore, err := workflow.Open(filepath.Join(filepath.Dir("/opt/rubbish/sessions.db"), "workflows.json"))
	if err != nil {
		slog.Error("open workflow store", "err", err); os.Exit(1)
	}

	mgr.serverCtx = ctx

	engine := workflow.NewEngine(ctx, wfStore, mgr, 3)
	engine.RecoverInProgress(ctx) //nolint:errcheck

	mux := http.NewServeMux()
	registerHandlers(mux, mgr, engine, ctx)

	mcpServer := mcp.NewServer(engine, mgr)
	mux.Handle("/mcp", mcpServer)

	srv := &http.Server{
		Addr:              ":8080",
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	go func() {
		slog.Info("poc: serving", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("server", "err", err); os.Exit(1)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	slog.Info("poc: shutting down")
	// Do NOT call StopAll — ready VMs are left running so they can be recovered
	// on the next startup via loadFromDB + DetachedVM reconnection.
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutCancel()
	srv.Shutdown(shutCtx) //nolint:errcheck
}
