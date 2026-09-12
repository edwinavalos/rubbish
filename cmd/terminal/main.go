package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/edwinavalos/rubbish/internal/gitutil"
	"github.com/edwinavalos/rubbish/internal/session"
	"github.com/edwinavalos/rubbish/internal/sessionstore"
	"github.com/edwinavalos/rubbish/internal/sessionstore/pgstore"
	"github.com/edwinavalos/rubbish/internal/terminal"
	"github.com/edwinavalos/rubbish/internal/vm"
	"github.com/gorilla/websocket"
	"golang.org/x/crypto/ssh"
)

//go:embed static/terminal.html
var terminalHTML []byte

// connHub tracks active WebSocket connections so they can be notified before
// a graceful shutdown, letting the browser auto-reconnect instead of showing
// a "Connection lost" overlay.
type connHub struct {
	mu    sync.Mutex
	conns map[*websocket.Conn]struct{}
}

func newConnHub() *connHub {
	return &connHub{conns: make(map[*websocket.Conn]struct{})}
}

func (h *connHub) add(c *websocket.Conn) {
	h.mu.Lock()
	h.conns[c] = struct{}{}
	h.mu.Unlock()
}

func (h *connHub) remove(c *websocket.Conn) {
	h.mu.Lock()
	delete(h.conns, c)
	h.mu.Unlock()
}

// notifyReconnect sends {"type":"reconnect"} to every active connection and
// then sends a WebSocket close frame. The browser treats this as a signal to
// auto-reconnect silently rather than showing the "Connection lost" overlay.
func (h *connHub) notifyReconnect() {
	h.mu.Lock()
	defer h.mu.Unlock()
	msg, _ := json.Marshal(map[string]string{"type": "reconnect"})
	for c := range h.conns {
		c.WriteMessage(websocket.TextMessage, msg)                                                              //nolint:errcheck
		c.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseGoingAway, "restart")) //nolint:errcheck
	}
}

func main() {
	keyPath := flag.String("key", "", "path to SSH private key for VM access")
	dbPath := flag.String("db", "/opt/rubbish/sessions.db", "path to sessions JSON store (used when --postgres-dsn is unset)")
	postgresDSN := flag.String("postgres-dsn", os.Getenv("RUBBISH_POSTGRES_DSN"), "Postgres DSN for the sessions store (defaults to RUBBISH_POSTGRES_DSN env var; empty falls back to the JSON store at --db)")
	addr := flag.String("addr", ":8081", "listen address")
	flag.Parse()

	if *keyPath == "" {
		slog.Error("usage: rubbish-terminal --key <path-to-ssh-private-key>"); os.Exit(1)
	}

	keyBytes, err := os.ReadFile(*keyPath)
	if err != nil {
		slog.Error("read SSH key", "err", err); os.Exit(1)
	}
	signer, err := ssh.ParsePrivateKey(keyBytes)
	if err != nil {
		slog.Error("parse SSH key", "err", err); os.Exit(1)
	}

	var store sessionstore.Store
	if *postgresDSN != "" {
		pgStore, err := pgstore.Open(*postgresDSN)
		if err != nil {
			slog.Error("open postgres session store", "err", err); os.Exit(1)
		}
		store = pgStore
	} else {
		jsonStore, err := sessionstore.Open(*dbPath)
		if err != nil {
			slog.Error("open json session store", "err", err); os.Exit(1)
		}
		store = jsonStore
	}
	defer store.Close()

	hub := newConnHub()
	mux := http.NewServeMux()

	mux.HandleFunc("/terminal/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write(terminalHTML) //nolint:errcheck
	})

	mux.HandleFunc("/ws/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/ws/")

		row, ok, err := store.Get(id)
		if err != nil {
			http.Error(w, "db error", http.StatusInternalServerError)
			return
		}
		if !ok {
			http.Error(w, "session not found", http.StatusNotFound)
			return
		}
		if session.State(row.Status) != session.StateReady {
			http.Error(w, fmt.Sprintf("session not ready (status: %s)", row.Status), http.StatusServiceUnavailable)
			return
		}

		ws, err := terminal.Upgrade(w, r)
		if err != nil {
			return
		}
		hub.add(ws)
		defer hub.remove(ws)

		vmAddr := fmt.Sprintf("%s:%d", vm.SlotIP(row.Slot), vm.VMSSHPort)
		bridge := terminal.NewBridge(vmAddr, "claude", signer)
		bridge.Relay(ws, sessionStartCmd(row.RepoURL))
	})

	srv := &http.Server{Addr: *addr, Handler: mux}

	// Graceful shutdown: notify all active terminals to auto-reconnect before
	// the process exits so deploys don't leave users with a dead browser tab.
	go func() {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGTERM, syscall.SIGINT)
		<-ch
		slog.Info("shutdown signal, notifying active terminals to reconnect")
		hub.notifyReconnect()
		// Brief pause so the WebSocket messages are flushed before we stop
		// accepting connections. The browser will retry during this window.
		time.Sleep(300 * time.Millisecond)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(ctx) //nolint:errcheck
	}()

	slog.Info("rubbish-terminal listening", "addr", *addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		slog.Error("listen", "err", err); os.Exit(1)
	}
}

func sessionStartCmd(repoURL string) string {
	inner := "claude --dangerously-skip-permissions; exec bash -l"
	if repoURL != "" {
		if name := gitutil.RepoName(repoURL); name != "" {
			inner = fmt.Sprintf("cd ~/workspace/%s 2>/dev/null || cd ~; %s", name, inner)
		}
	}
	// tmux new-session -A: attach to existing session or create a new one.
	// Keeps claude running through WebSocket disconnects; mouse scrollback is
	// handled by tmux (set -g mouse on in ~/.tmux.conf).
	return fmt.Sprintf("bash -l -c 'tmux new-session -A -s rubbish bash -l -c %q'", inner)
}
