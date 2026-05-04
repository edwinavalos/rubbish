package terminal

import (
	"encoding/json"
	"fmt"
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
	sshKey ssh.Signer
}

func NewBridge(host string, signer ssh.Signer) *Bridge {
	return &Bridge{host: host, sshKey: signer}
}

// RunSetup opens a non-PTY SSH session and runs each command sequentially.
// Stops and returns an error on the first failure.
func (b *Bridge) RunSetup(commands []string) error {
	client, err := dialSSH(b.host, b.sshKey)
	if err != nil {
		return fmt.Errorf("ssh dial: %w", err)
	}
	defer client.Close()

	for _, cmd := range commands {
		sess, err := client.NewSession()
		if err != nil {
			return fmt.Errorf("new session for %q: %w", cmd, err)
		}
		err = sess.Run(cmd)
		sess.Close()
		if err != nil {
			return fmt.Errorf("run %q: %w", cmd, err)
		}
	}
	return nil
}

func (b *Bridge) ServeWS(w http.ResponseWriter, r *http.Request, startCmd string) {
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer ws.Close()

	connStart := time.Now()

	client, err := dialSSH(b.host, b.sshKey)
	if err != nil {
		ws.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf("\r\nSSH dial failed: %v\r\n", err)))
		return
	}
	defer client.Close()

	session, err := client.NewSession()
	if err != nil {
		ws.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf("\r\nSSH session failed: %v\r\n", err)))
		return
	}
	defer session.Close()

	modes := ssh.TerminalModes{
		ssh.ECHO:          1,
		ssh.TTY_OP_ISPEED: 14400,
		ssh.TTY_OP_OSPEED: 14400,
	}
	if err := session.RequestPty("xterm-256color", 40, 120, modes); err != nil {
		ws.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf("\r\nPTY request failed: %v\r\n", err)))
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
		ws.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf("\r\nShell failed: %v\r\n", err)))
		return
	}

	fmt.Printf("[terminal] session ready in %s\n", time.Since(connStart).Round(time.Millisecond))

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
					fmt.Printf("[terminal] first byte from PTY in %s\n", time.Since(connStart).Round(time.Millisecond))
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

func dialSSH(host string, signer ssh.Signer) (*ssh.Client, error) {
	cfg := &ssh.ClientConfig{
		User:            "root",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	}
	return ssh.Dial("tcp", host, cfg)
}
