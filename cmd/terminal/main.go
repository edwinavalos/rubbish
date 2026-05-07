package main

import (
	_ "embed"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/edwinavalos/rubbish/internal/session"
	"github.com/edwinavalos/rubbish/internal/sessionstore"
	"github.com/edwinavalos/rubbish/internal/terminal"
	"github.com/edwinavalos/rubbish/internal/vm"
	"golang.org/x/crypto/ssh"
)

//go:embed static/terminal.html
var terminalHTML []byte

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

	mux := http.NewServeMux()

	mux.HandleFunc("/terminal/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write(terminalHTML)
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

		vmAddr := fmt.Sprintf("%s:%d", vm.SlotIP(row.Slot), vm.VMSSHPort)
		bridge := terminal.NewBridge(vmAddr, "claude", signer)
		bridge.ServeWS(w, r, sessionStartCmd(row.RepoURL))
	})

	log.Printf("rubbish-terminal listening on %s", *addr)
	if err := http.ListenAndServe(*addr, mux); err != nil {
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
