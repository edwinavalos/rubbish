package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edwinavalos/rubbish/internal/profile"
	"github.com/edwinavalos/rubbish/internal/session"
)

// capturingSetupRunner records every RunSetup call for inspection.
type capturingSetupRunner struct {
	mu    sync.Mutex
	calls [][]string
}

func (r *capturingSetupRunner) RunSetup(cmds []string) error {
	r.mu.Lock()
	r.calls = append(r.calls, append([]string(nil), cmds...))
	r.mu.Unlock()
	return nil
}

func (r *capturingSetupRunner) allCmds() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, call := range r.calls {
		out = append(out, call...)
	}
	return out
}

// newTestManagerWithProfile wires a profile store into the session manager.
func newTestManagerWithProfile(
	snap *fakeSnapshotter,
	launcher *fakeLauncher,
	bridge *fakeBridgeFactory,
	prof *profile.Store,
) *SessionManager {
	return newSessionManager(snap, launcher, bridge, nil, "", prof, nil)
}

// ---- Injection tests ---------------------------------------------------------

func TestBoot_InjectsClaudeToken(t *testing.T) {
	runner := &capturingSetupRunner{}
	store := profile.NewStore(filepath.Join(t.TempDir(), "profile.json"))
	if err := store.Save(profile.Profile{ClaudeOAuthToken: "sk-ant-oat01-testtoken"}); err != nil {
		t.Fatal(err)
	}

	mgr := newTestManagerWithProfile(
		&fakeSnapshotter{},
		&fakeLauncher{handle: &fakeVMHandle{}},
		&fakeBridgeFactory{runner: runner},
		store,
	)

	sess, err := mgr.Create(context.Background(), "", "", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	waitState(t, mgr, sess.ID, session.StateReady, 3*time.Second)

	found := false
	for _, cmd := range runner.allCmds() {
		if strings.Contains(cmd, "CLAUDE_CODE_OAUTH_TOKEN") && strings.Contains(cmd, "sk-ant-oat01-testtoken") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected CLAUDE_CODE_OAUTH_TOKEN injection; got commands: %v", runner.allCmds())
	}
}

func TestBoot_SkipsInjectionWhenNoToken(t *testing.T) {
	runner := &capturingSetupRunner{}
	store := profile.NewStore(filepath.Join(t.TempDir(), "profile.json"))
	// Save empty profile — no token set.
	store.Save(profile.Profile{}) //nolint:errcheck

	mgr := newTestManagerWithProfile(
		&fakeSnapshotter{},
		&fakeLauncher{handle: &fakeVMHandle{}},
		&fakeBridgeFactory{runner: runner},
		store,
	)

	sess, _ := mgr.Create(context.Background(), "", "", "")
	waitState(t, mgr, sess.ID, session.StateReady, 3*time.Second)

	for _, cmd := range runner.allCmds() {
		if strings.Contains(cmd, "CLAUDE_CODE_OAUTH_TOKEN") {
			t.Errorf("unexpected CLAUDE_CODE_OAUTH_TOKEN injection when no token set; cmd: %q", cmd)
		}
	}
}

// ---- Profile API tests -------------------------------------------------------

func newTestServerWithProfile(prof *profile.Store) *httptest.Server {
	mgr := newTestManagerWithProfile(
		&fakeSnapshotter{},
		&fakeLauncher{handle: &fakeVMHandle{}},
		&fakeBridgeFactory{runner: &capturingSetupRunner{}},
		prof,
	)
	mux := http.NewServeMux()
	registerHandlers(mux, mgr, context.Background())
	return httptest.NewServer(mux)
}

func TestProfileAPI_GetEmpty(t *testing.T) {
	store := profile.NewStore(filepath.Join(t.TempDir(), "profile.json"))
	srv := newTestServerWithProfile(store)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/profile")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/profile status = %d, want 200", resp.StatusCode)
	}

	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["claude_oauth_token"] != "" {
		t.Errorf("expected empty token, got %q", body["claude_oauth_token"])
	}
}

func TestProfileAPI_PutAndGet(t *testing.T) {
	store := profile.NewStore(filepath.Join(t.TempDir(), "profile.json"))
	srv := newTestServerWithProfile(store)
	defer srv.Close()

	payload, _ := json.Marshal(map[string]string{
		"claude_oauth_token": "sk-ant-oat01-realtoken",
		"github_token":       "ghp_realgithubtoken",
	})
	req, _ := http.NewRequest(http.MethodPut, srv.URL+"/api/profile", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("PUT /api/profile status = %d, want 204", resp.StatusCode)
	}

	resp, err = http.Get(srv.URL + "/api/profile")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var body map[string]string
	json.NewDecoder(resp.Body).Decode(&body) //nolint:errcheck

	claudeTok := body["claude_oauth_token"]
	if !strings.HasSuffix(claudeTok, "oken") {
		t.Errorf("claude_oauth_token = %q, want masked ending in 'oken'", claudeTok)
	}
	if claudeTok == "sk-ant-oat01-realtoken" {
		t.Error("GET returned unmasked claude token")
	}

	ghTok := body["github_token"]
	if !strings.HasSuffix(ghTok, "oken") {
		t.Errorf("github_token = %q, want masked ending in 'oken'", ghTok)
	}
	if ghTok == "ghp_realgithubtoken" {
		t.Error("GET returned unmasked github token")
	}
}

func TestBoot_UsesProfileGitHubToken(t *testing.T) {
	runner := &capturingSetupRunner{}
	store := profile.NewStore(filepath.Join(t.TempDir(), "profile.json"))
	if err := store.Save(profile.Profile{GitHubToken: "ghp_profiletoken"}); err != nil {
		t.Fatal(err)
	}

	mgr := newTestManagerWithProfile(
		&fakeSnapshotter{},
		&fakeLauncher{handle: &fakeVMHandle{}},
		&fakeBridgeFactory{runner: runner},
		store,
	)

	sess, err := mgr.Create(context.Background(), "https://github.com/example/repo", "", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	waitState(t, mgr, sess.ID, session.StateReady, 3*time.Second)

	found := false
	for _, cmd := range runner.allCmds() {
		if strings.Contains(cmd, "ghp_profiletoken") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected profile GH token in setup commands; got: %v", runner.allCmds())
	}
}

func TestProfileAPI_PutBadJSON(t *testing.T) {
	store := profile.NewStore(filepath.Join(t.TempDir(), "profile.json"))
	srv := newTestServerWithProfile(store)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPut, srv.URL+"/api/profile", bytes.NewReader([]byte("notjson")))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("PUT with bad JSON status = %d, want 400", resp.StatusCode)
	}
}
