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

	"github.com/edwinavalos/rubbish/internal/session"
	"github.com/edwinavalos/rubbish/internal/sessionstore"
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
	dbPath := flag.String("db", "/opt/rubbish/sessions.db", "path to sessions SQLite database")
	addr := flag.String("addr", ":8081", "listen address")
	flag.Parse()

	if *keyPath == "" {
		log.Fatal("usage: rubbish-terminal --key <path-to-ssh-private-key>")
	}

	keyBytes, err := os.ReadFile(*keyPath)
	if err != nil {
		log.Fatalf("read SSH key: %v", err)
	}
	signer, err := ssh.ParsePrivateKey(keyBytes)
	if err != nil {
		log.Fatalf("parse SSH key: %v", err)
	}

	store, err := sessionstore.Open(*dbPath)
	if err != nil {
		log.Fatalf("open session DB: %v", err)
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
		log.Printf("shutdown signal — notifying active terminals to reconnect")
		hub.notifyReconnect()
		// Brief pause so the WebSocket messages are flushed before we stop
		// accepting connections. The browser will retry during this window.
		time.Sleep(300 * time.Millisecond)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(ctx) //nolint:errcheck
	}()

	log.Printf("rubbish-terminal listening on %s", *addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("listen: %v", err)
	}
}

func sessionStartCmd(repoURL string) string {
	inner := "claude --dangerously-skip-permissions; exec bash -l"
	if repoURL != "" {
		if name := repoName(repoURL); name != "" {
			inner = fmt.Sprintf("cd /root/workspace/%s 2>/dev/null || cd ~; %s", name, inner)
		}
	}
	// screen -D -R: reattach to existing session (detaching any other client) or
	// create a new one. This keeps claude running through WebSocket disconnects.
	return fmt.Sprintf("bash -l -c 'screen -D -R -S rubbish bash -l -c %q'", inner)
}

func repoName(rawURL string) string {
	rawURL = strings.TrimRight(rawURL, "/")
	rawURL = strings.TrimSuffix(rawURL, ".git")
	idx := strings.LastIndexAny(rawURL, "/:")
	if idx < 0 || idx == len(rawURL)-1 {
		return ""
	}
	return rawURL[idx+1:]
}
