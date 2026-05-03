package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	fc "github.com/edwinavalos/rubbish/internal/compute/firecracker"
	"github.com/edwinavalos/rubbish/internal/terminal"
	"github.com/edwinavalos/rubbish/internal/vm"
	"golang.org/x/crypto/ssh"
)

//go:embed static/index.html
var indexHTML []byte

//go:embed static/terminal.html
var terminalHTML []byte

// ---- Session ----------------------------------------------------------------

type Session struct {
	ID        string    `json:"id"`
	Slot      int       `json:"slot"`
	Status    string    `json:"status"` // starting | ready | failed | stopped
	RepoURL   string    `json:"repo_url"`
	Branch    string    `json:"branch,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	Error     string    `json:"error,omitempty"`

	v      *vm.VM
	bridge *terminal.Bridge
	cancel context.CancelFunc
}

// ---- SessionManager ---------------------------------------------------------

type SessionManager struct {
	mu       sync.RWMutex
	sessions map[string]*Session
	slots    [vm.MaxSlots]bool
	snapMgr  *fc.SnapshotManager
	signer   ssh.Signer
	credPath string
}

func NewSessionManager(snapMgr *fc.SnapshotManager, signer ssh.Signer, credPath string) *SessionManager {
	return &SessionManager{
		sessions: make(map[string]*Session),
		snapMgr:  snapMgr,
		signer:   signer,
		credPath: credPath,
	}
}

func (m *SessionManager) allocSlot() (int, bool) {
	for i := range m.slots {
		if !m.slots[i] {
			m.slots[i] = true
			return i, true
		}
	}
	return 0, false
}

func (m *SessionManager) freeSlot(slot int) {
	m.mu.Lock()
	m.slots[slot] = false
	m.mu.Unlock()
}

// Create allocates a slot and starts a VM in the background. Returns immediately
// with status "starting".
func (m *SessionManager) Create(parentCtx context.Context, repoURL, branch, githubToken string) (*Session, error) {
	m.mu.Lock()
	slot, ok := m.allocSlot()
	if !ok {
		m.mu.Unlock()
		return nil, fmt.Errorf("no slots available (max %d concurrent sessions)", vm.MaxSlots)
	}

	id := uuid.New().String()
	ctx, cancel := context.WithCancel(parentCtx)
	sess := &Session{
		ID:        id,
		Slot:      slot,
		Status:    "starting",
		RepoURL:   repoURL,
		Branch:    branch,
		CreatedAt: time.Now(),
		cancel:    cancel,
	}
	m.sessions[id] = sess
	m.mu.Unlock()

	go m.boot(ctx, sess, githubToken)
	return sess, nil
}

func (m *SessionManager) boot(ctx context.Context, sess *Session, githubToken string) {
	fail := func(err error) {
		m.mu.Lock()
		sess.Status = "failed"
		sess.Error = err.Error()
		m.slots[sess.Slot] = false
		m.mu.Unlock()
		fmt.Printf("[session %s] failed: %v\n", sess.ID[:8], err)
	}

	// 1. Snapshot
	device, err := m.snapMgr.CreateSnapshot(sess.ID)
	if err != nil {
		fail(fmt.Errorf("create snapshot: %w", err))
		return
	}

	// 2. Inject per-slot network config
	if err := vm.InjectNetworkConfig(device, vm.SlotIP(sess.Slot), "172.16.0.1"); err != nil {
		m.snapMgr.DeleteSnapshot(sess.ID) //nolint:errcheck
		fail(fmt.Errorf("inject network: %w", err))
		return
	}

	// 3. Boot VM
	v, err := vm.Launch(ctx, sess.Slot, device)
	if err != nil {
		m.snapMgr.DeleteSnapshot(sess.ID) //nolint:errcheck
		fail(fmt.Errorf("launch vm: %w", err))
		return
	}
	sess.v = v

	// 4. Wait for SSH
	if err := v.WaitForSSH(ctx); err != nil {
		v.Stop(ctx)                        //nolint:errcheck
		m.snapMgr.DeleteSnapshot(sess.ID) //nolint:errcheck
		fail(fmt.Errorf("wait for ssh: %w", err))
		return
	}

	vmAddr := fmt.Sprintf("%s:%d", vm.SlotIP(sess.Slot), vm.VMSSHPort)
	bridge := terminal.NewBridge(vmAddr, m.signer)
	sess.bridge = bridge

	// 5. Configure git credentials
	token := githubToken
	if token == "" {
		token = loadSavedToken(m.credPath)
	}
	if token != "" {
		if githubToken != "" {
			if err := saveToken(m.credPath, githubToken); err != nil {
				fmt.Printf("[session %s] warning: save token: %v\n", sess.ID[:8], err)
			}
		}
		cmds := []string{
			"git config --global credential.helper store",
			fmt.Sprintf(`printf 'https://oauth2:%s@github.com\n' > /root/.git-credentials`, token),
		}
		if err := bridge.RunSetup(cmds); err != nil {
			fmt.Printf("[session %s] warning: git credential setup: %v\n", sess.ID[:8], err)
		}
	}

	// 6. Clone repo
	if sess.RepoURL != "" {
		var cloneCmd string
		if sess.Branch != "" {
			cloneCmd = fmt.Sprintf("git clone -b %s %s /root/workspace 2>&1 | tail -5", sess.Branch, sess.RepoURL)
		} else {
			cloneCmd = fmt.Sprintf("git clone %s /root/workspace 2>&1 | tail -5", sess.RepoURL)
		}
		if err := bridge.RunSetup([]string{cloneCmd}); err != nil {
			fmt.Printf("[session %s] warning: git clone: %v\n", sess.ID[:8], err)
		}
	}

	m.mu.Lock()
	sess.Status = "ready"
	m.mu.Unlock()
	fmt.Printf("[session %s] ready (slot=%d ip=%s)\n", sess.ID[:8], sess.Slot, vm.SlotIP(sess.Slot))
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
	return out
}

func (m *SessionManager) Stop(id string) error {
	m.mu.Lock()
	sess, ok := m.sessions[id]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("session %s not found", id)
	}
	if sess.Status == "stopped" {
		m.mu.Unlock()
		return nil
	}
	sess.Status = "stopped"
	m.mu.Unlock()

	sess.cancel()
	if sess.v != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		sess.v.Stop(ctx) //nolint:errcheck
	}
	m.snapMgr.DeleteSnapshot(id) //nolint:errcheck
	m.freeSlot(sess.Slot)
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

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func registerHandlers(mux *http.ServeMux, mgr *SessionManager, rootCtx context.Context) {
	// Saved token availability (boolean only — never return the token value)
	mux.HandleFunc("/api/saved-token", func(w http.ResponseWriter, r *http.Request) {
		has := loadSavedToken(mgr.credPath) != ""
		writeJSON(w, http.StatusOK, map[string]bool{"has_token": has})
	})

	// Session list page
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		w.Write(indexHTML)
	})

	// Terminal page for a session
	mux.HandleFunc("/terminal/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write(terminalHTML)
	})

	// Create session
	mux.HandleFunc("/api/sessions", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, http.StatusOK, mgr.List())

		case http.MethodPost:
			var body struct {
				RepoURL     string `json:"repo_url"`
				Branch      string `json:"branch"`
				GithubToken string `json:"github_token"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			sess, err := mgr.Create(rootCtx, body.RepoURL, body.Branch, body.GithubToken)
			if err != nil {
				http.Error(w, err.Error(), http.StatusServiceUnavailable)
				return
			}
			writeJSON(w, http.StatusAccepted, sess)

		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})

	// Stop / delete session
	mux.HandleFunc("/api/sessions/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		id := strings.TrimPrefix(r.URL.Path, "/api/sessions/")
		if err := mgr.Stop(id); err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	// WebSocket terminal
	mux.HandleFunc("/ws/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/ws/")
		sess, ok := mgr.Get(id)
		if !ok {
			http.Error(w, "session not found", http.StatusNotFound)
			return
		}
		if sess.Status != "ready" {
			http.Error(w, fmt.Sprintf("session not ready (status: %s)", sess.Status), http.StatusServiceUnavailable)
			return
		}
		sess.bridge.ServeWS(w, r)
	})
}

// ---- main -------------------------------------------------------------------

func main() {
	keyFlag := flag.String("key", "", "path to SSH private key for VM access")
	flag.Parse()

	if *keyFlag == "" {
		// Fall back to positional arg for backwards compat
		if flag.NArg() > 0 {
			*keyFlag = flag.Arg(0)
		} else {
			log.Fatal("usage: rubbish-poc --key <path-to-ssh-private-key>")
		}
	}

	keyBytes, err := os.ReadFile(*keyFlag)
	if err != nil {
		log.Fatalf("read ssh key: %v", err)
	}
	signer, err := ssh.ParsePrivateKey(keyBytes)
	if err != nil {
		log.Fatalf("parse ssh key: %v", err)
	}

	log.Println("[startup] cleaning up orphaned VMs...")
	if err := vm.CleanupOrphans(vm.MaxSlots); err != nil {
		log.Printf("[startup] cleanup warning: %v", err)
	}

	snapMgr, err := fc.NewSnapshotManager("/dev/mapper/rubbish-pool", "/opt/rubbish/dm/next-volume-id")
	if err != nil {
		log.Fatalf("snapshot manager: %v", err)
	}

	credPath := "/opt/rubbish/creds/github-token"
	if err := os.MkdirAll("/opt/rubbish/creds", 0700); err != nil {
		log.Fatalf("create creds dir: %v", err)
	}

	mgr := NewSessionManager(snapMgr, signer, credPath)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mux := http.NewServeMux()
	registerHandlers(mux, mgr, ctx)

	srv := &http.Server{
		Addr:              ":8080",
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	go func() {
		fmt.Println("[poc] serving on http://192.168.1.35:8080")
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	fmt.Println("[poc] shutting down...")
	mgr.StopAll()
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutCancel()
	srv.Shutdown(shutCtx) //nolint:errcheck
}
