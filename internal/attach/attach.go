// Package attach joins a covibe session's terminal from another machine, over
// either of the two paths that reach one.
//
// Through the dashboard it speaks the same WebSocket the browser terminal uses,
// so there is no second server surface to keep honest: the dashboard
// authenticates the caller, checks the session's ACL, and relays to whichever
// tmux owns it — here or on a third machine that dialled in.
//
// Peer to peer it hands a ticket to the covibe-p2p sidecar, which dials the
// session's iroh endpoint directly. The dashboard is not in that path at all: the
// ticket is the whole credential, and it stops resolving when the session ends.
// Both paths carry the identical protocol, so the loop below serves both.
package attach

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/lassulus/covibe/internal/termwire"
)

// DetachKey ends an attach without touching the program on the far side. Ctrl-]
// is the telnet escape: TUIs do not use it, and a key had to be picked because
// the far end owns everything else the keyboard produces.
const DetachKey = 0x1d

// Options configures one attach. In/Out are the local terminal; Size reports its
// current dimensions (nil means "let the server decide").
type Options struct {
	Base   string // dashboard base URL, e.g. https://covibe.example
	Token  string // per-user API key (COVIBE_TOKEN)
	Target string // session id, name, or tmux session name
	In     io.Reader
	Out    io.Writer
	Size   func() (cols, rows int)
	// Notify reports lifecycle lines (read-only warning, detach hint, exit) to
	// the operator. Writing them to Out would inject text into the terminal
	// stream, so the caller decides where they go.
	Notify func(string)
}

// Session is one session as the dashboard lists it. Only the fields an attach
// needs to resolve and describe a target are decoded.
type Session struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Dir         string `json:"dir"`
	MuxSession  string `json:"muxSession"`
	HasTerminal bool   `json:"hasTerminal"`
	CanWrite    bool   `json:"canWrite"`
}

// List returns the sessions this token may see.
func List(ctx context.Context, base, token string) ([]Session, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", strings.TrimRight(base, "/")+"/api/v1/sessions", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("dashboard rejected the token (%s); pass --token or set COVIBE_TOKEN", resp.Status)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list sessions: %s", resp.Status)
	}
	var out []Session
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode session list: %w", err)
	}
	return out, nil
}

// Resolve picks the session a target names. An id or tmux session name is
// unique; a display name shared by several sessions is refused rather than
// guessed, because guessing drops someone into the wrong shell.
func Resolve(sessions []Session, target string) (Session, error) {
	var byName []Session
	for _, s := range sessions {
		if s.ID == target || s.MuxSession == target {
			return s, nil
		}
		if s.Name == target {
			byName = append(byName, s)
		}
	}
	switch len(byName) {
	case 1:
		return byName[0], nil
	case 0:
		return Session{}, fmt.Errorf("no session %q here (covibe list --dashboard <url> shows what you can reach)", target)
	default:
		var b strings.Builder
		fmt.Fprintf(&b, "%d sessions are called %q; attach one by id:\n", len(byName), target)
		for _, s := range byName {
			fmt.Fprintf(&b, "  covibe attach %s\t(%s)\n", s.ID, s.Dir)
		}
		return Session{}, errors.New(strings.TrimRight(b.String(), "\n"))
	}
}

const writeTimeout = 10 * time.Second

// clientMsg is the terminal protocol, client side.
type clientMsg struct {
	T    string `json:"t"`
	B64  string `json:"b64,omitempty"`
	Cols int    `json:"cols,omitempty"`
	Rows int    `json:"rows,omitempty"`
}

// msgConn is the terminal protocol as a stream of typed messages: control JSON
// or raw terminal bytes. A WebSocket already frames them; a QUIC stream gets the
// framing from termwire. Everything above this interface is transport-agnostic.
type msgConn interface {
	// ReadMsg returns the next message and whether it is control JSON rather
	// than terminal bytes.
	ReadMsg(ctx context.Context) (text bool, data []byte, err error)
	WriteText(ctx context.Context, data []byte) error
	Close() error
}

// Run attaches through the dashboard.
func Run(ctx context.Context, o Options) error {
	sessions, err := List(ctx, o.Base, o.Token)
	if err != nil {
		return err
	}
	target, err := Resolve(sessions, o.Target)
	if err != nil {
		return err
	}
	if !target.HasTerminal {
		return fmt.Errorf("session %q has no terminal to attach: it is not running on a tmux socket covibe drives, and no wrapper is hosting it", target.Name)
	}

	cols, rows := 0, 0
	if o.Size != nil {
		cols, rows = o.Size()
	}
	url := strings.TrimRight(o.Base, "/") + "/api/v1/sessions/" + target.ID + "/terminal"
	if cols > 0 && rows > 0 {
		url += fmt.Sprintf("?cols=%d&rows=%d", cols, rows)
	}
	url = wsScheme(url)

	hdr := http.Header{}
	if o.Token != "" {
		hdr.Set("Authorization", "Bearer "+o.Token)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	ws, resp, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: hdr})
	if err != nil {
		if resp != nil {
			return fmt.Errorf("attach %s: %s", target.Name, resp.Status)
		}
		return fmt.Errorf("attach %s: %w", target.Name, err)
	}
	ws.SetReadLimit(4 << 20)
	c := &wsConn{ws: ws}
	defer c.Close()
	return stream(ctx, cancel, c, o, target.Name)
}

// RunTicket attaches peer to peer through the sidecar, which dials the endpoint
// the ticket names. No dashboard, no token, no session list: the ticket both
// addresses the session and authorizes the attach, and whether it is the
// read-only or the read-write one is decided by which endpoint it points at.
func RunTicket(ctx context.Context, o Options, sidecarBin, ticket string) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	s, err := dialSidecar(ctx, sidecarBin, ticket)
	if err != nil {
		return err
	}
	c := &frameConn{c: termwire.NewConn(s)}
	defer c.Close()

	// Ask for a repaint before reading anything. This is also what makes the far
	// side notice us at all: a QUIC stream does not exist for the peer until the
	// side that opened it writes, and in this protocol the server speaks first —
	// so without a first message from here, the host would sit in accept_bi and
	// this terminal would stay blank until the user happened to press a key.
	if err := writeMsg(ctx, c, clientMsg{T: "snapshot"}); err != nil {
		return fmt.Errorf("attach: %w", err)
	}
	return stream(ctx, cancel, c, o, "session")
}

// stream runs one attached terminal to exhaustion: bytes from the far side go to
// Out, everything typed into In goes back as input, and the detach key ends it
// without touching the program on the other end.
func stream(ctx context.Context, cancel func(), c msgConn, o Options, label string) error {
	notify := o.Notify
	if notify == nil {
		notify = func(string) {}
	}

	// Input runs in its own goroutine: a blocking read on a terminal cannot be
	// cancelled, so this one outlives stream and dies with the process. It is the
	// reason detaching is a keystroke rather than a context.
	go func() {
		buf := make([]byte, 4<<10)
		for {
			n, err := o.In.Read(buf)
			for i := range n {
				if buf[i] == DetachKey {
					if i > 0 {
						_ = writeMsg(ctx, c, clientMsg{T: "input", B64: base64.StdEncoding.EncodeToString(buf[:i])})
					}
					notify("detached")
					cancel()
					_ = c.Close() // unblocks a framed read, which ctx cannot
					return
				}
			}
			if n > 0 {
				if err := writeMsg(ctx, c, clientMsg{T: "input", B64: base64.StdEncoding.EncodeToString(buf[:n])}); err != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	for {
		text, data, err := c.ReadMsg(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil // detached, or the caller cancelled
			}
			return nil // the far side hung up; the session usually ended
		}
		if !text {
			if _, err := o.Out.Write(data); err != nil {
				return err
			}
			continue
		}
		var msg struct {
			T     string `json:"t"`
			Write bool   `json:"write"`
			Cols  int    `json:"cols"`
			Rows  int    `json:"rows"`
			Msg   string `json:"msg"`
		}
		if json.Unmarshal(data, &msg) != nil {
			continue
		}
		switch msg.T {
		case "hello":
			where := fmt.Sprintf("attached to %q (%dx%d)", label, msg.Cols, msg.Rows)
			if !msg.Write {
				where += " read-only"
			}
			notify(where + "; press Ctrl-] to detach")
		case "exit":
			notify("session ended")
			return nil
		case "error":
			return fmt.Errorf("far side: %s", msg.Msg)
		}
	}
}

func writeMsg(ctx context.Context, c msgConn, m clientMsg) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return c.WriteText(ctx, data)
}

// wsConn carries the protocol over the dashboard's WebSocket.
type wsConn struct{ ws *websocket.Conn }

func (w *wsConn) ReadMsg(ctx context.Context) (bool, []byte, error) {
	typ, data, err := w.ws.Read(ctx)
	return typ == websocket.MessageText, data, err
}

func (w *wsConn) WriteText(ctx context.Context, data []byte) error {
	wctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	return w.ws.Write(wctx, websocket.MessageText, data)
}

func (w *wsConn) Close() error { return w.ws.CloseNow() }

// frameConn carries the protocol over the sidecar's stdio. The context is
// unused: a read on a pipe cannot be cancelled, so Close is what unblocks it.
type frameConn struct{ c *termwire.Conn }

func (f *frameConn) ReadMsg(context.Context) (bool, []byte, error) {
	kind, data, err := f.c.ReadMsg()
	return kind == termwire.KindText, data, err
}

func (f *frameConn) WriteText(_ context.Context, data []byte) error {
	return f.c.WriteMsg(termwire.KindText, data)
}

func (f *frameConn) Close() error { return f.c.Close() }

// dialSidecar starts the sidecar in dial mode and returns its stdio as one
// stream. Its stderr is passed through to ours so a failure to reach the
// endpoint is visible; stdout carries nothing but stream bytes.
func dialSidecar(ctx context.Context, bin, ticket string) (*sidecarStream, error) {
	if bin == "" {
		return nil, errors.New("no covibe-p2p sidecar configured: set COVIBE_P2P to its path")
	}
	cmd := exec.CommandContext(ctx, bin, "dial", "--ticket", ticket) // #nosec G204 -- operator-configured binary, fixed argv
	cmd.Stderr = os.Stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", bin, err)
	}
	return &sidecarStream{cmd: cmd, out: out, in: in}, nil
}

// sidecarStream is a sidecar process presented as one byte stream.
type sidecarStream struct {
	cmd  *exec.Cmd
	out  io.ReadCloser
	in   io.WriteCloser
	once sync.Once
}

func (s *sidecarStream) Read(p []byte) (int, error)  { return s.out.Read(p) }
func (s *sidecarStream) Write(p []byte) (int, error) { return s.in.Write(p) }

// Close ends the attach: the sidecar is killed rather than asked to finish,
// because a detach should not wait on a far side that may already be gone.
func (s *sidecarStream) Close() error {
	s.once.Do(func() {
		_ = s.in.Close()
		if s.cmd.Process != nil {
			_ = s.cmd.Process.Kill()
		}
		_ = s.out.Close()
		_ = s.cmd.Wait()
	})
	return nil
}

// wsScheme turns a dashboard URL into its WebSocket form.
func wsScheme(u string) string {
	switch {
	case strings.HasPrefix(u, "https://"):
		return "wss://" + strings.TrimPrefix(u, "https://")
	case strings.HasPrefix(u, "http://"):
		return "ws://" + strings.TrimPrefix(u, "http://")
	}
	return u
}
