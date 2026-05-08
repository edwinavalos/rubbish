package terminal

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/crypto/ssh"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

type resizeMsg struct {
	Type string `json:"type"`
	Cols uint32 `json:"cols"`
	Rows uint32 `json:"rows"`
}

type Bridge struct {
	host   string
	user   string
	sshKey ssh.Signer
}

func NewBridge(host, user string, signer ssh.Signer) *Bridge {
	return &Bridge{host: host, user: user, sshKey: signer}
}

// RunSetup opens a non-PTY SSH session and runs each command sequentially.
// Stops and returns an error on the first failure.
func (b *Bridge) RunSetup(commands []string) error {
	return b.RunSetupCapture(commands, io.Discard)
}

// RunSetupCapture is like RunSetup but writes each command's combined
// stdout+stderr to w. Used by integration tests to inspect output.
func (b *Bridge) RunSetupCapture(commands []string, w io.Writer) error {
	client, err := dialSSH(b.host, b.user, b.sshKey)
	if err != nil {
		return fmt.Errorf("ssh dial: %w", err)
	}
	defer client.Close()

	for _, cmd := range commands {
		sess, err := client.NewSession()
		if err != nil {
			return fmt.Errorf("new session for %q: %w", cmd, err)
		}
		// Use separate buffers — bytes.Buffer is not goroutine-safe and the SSH
		// library copies stdout and stderr concurrently.
		var stdout, stderr bytes.Buffer
		sess.Stdout = &stdout
		sess.Stderr = &stderr
		err = sess.Run(cmd)
		sess.Close()
		_, _ = w.Write(stdout.Bytes())
		_, _ = w.Write(stderr.Bytes())
		if err != nil {
			return fmt.Errorf("run %q: %w", cmd, err)
		}
	}
	return nil
}

// Upgrade upgrades the HTTP connection to WebSocket and returns it. Use this
// alongside Relay when the caller needs to register the connection with a hub
// before the relay starts (e.g. for graceful-shutdown notification).
func Upgrade(w http.ResponseWriter, r *http.Request) (*websocket.Conn, error) {
	return upgrader.Upgrade(w, r, nil)
}

// Relay runs the terminal relay on an already-upgraded WebSocket connection,
// blocking until the connection closes. ws is closed on return.
func (b *Bridge) Relay(ws *websocket.Conn, startCmd string) {
	defer ws.Close()
	b.relay(ws, startCmd, time.Now())
}

func (b *Bridge) relay(ws *websocket.Conn, startCmd string, connStart time.Time) {
	client, err := dialSSH(b.host, b.user, b.sshKey)
	if err != nil {
		ws.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf("\r\nSSH dial failed: %v\r\n", err))) //nolint:errcheck
		return
	}
	defer client.Close()

	session, err := client.NewSession()
	if err != nil {
		ws.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf("\r\nSSH session failed: %v\r\n", err))) //nolint:errcheck
		return
	}
	defer session.Close()

	modes := ssh.TerminalModes{
		ssh.ECHO:          1,
		ssh.TTY_OP_ISPEED: 14400,
		ssh.TTY_OP_OSPEED: 14400,
	}
	if err := session.RequestPty("xterm-256color", 40, 120, modes); err != nil {
		ws.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf("\r\nPTY request failed: %v\r\n", err))) //nolint:errcheck
		return
	}

	stdin, err := session.StdinPipe()
	if err != nil {
		return
	}
	stdout, err := session.StdoutPipe()
	if err != nil {
		return
	}

	if err := session.Start(startCmd); err != nil {
		ws.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf("\r\nShell failed: %v\r\n", err))) //nolint:errcheck
		return
	}

	log.Printf("[terminal] session ready in %s", time.Since(connStart).Round(time.Millisecond))

	done := make(chan struct{})

	// VM output -> WebSocket
	go func() {
		defer close(done)
		buf := make([]byte, 4096)
		first := true
		for {
			n, err := stdout.Read(buf)
			if n > 0 {
				if first {
					log.Printf("[terminal] first byte from PTY in %s", time.Since(connStart).Round(time.Millisecond))
					first = false
				}
				if err := ws.WriteMessage(websocket.BinaryMessage, buf[:n]); err != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	// WebSocket -> VM stdin (or resize)
	for {
		_, msg, err := ws.ReadMessage()
		if err != nil {
			break
		}

		var resize resizeMsg
		if json.Unmarshal(msg, &resize) == nil && resize.Type == "resize" {
			session.WindowChange(int(resize.Rows), int(resize.Cols))
			continue
		}

		if _, err := stdin.Write(msg); err != nil {
			break
		}
	}

	<-done
}

func dialSSH(host, user string, signer ssh.Signer) (*ssh.Client, error) {
	cfg := &ssh.ClientConfig{
		User:            user,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	}
	return ssh.Dial("tcp", host, cfg)
}
