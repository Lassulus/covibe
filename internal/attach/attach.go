// Package attach joins a covibe session's terminal from another machine. It
// speaks the same WebSocket the browser terminal uses, so there is no second
// server surface to keep honest: the dashboard authenticates the caller, checks
// the session's ACL, and relays to whichever tmux owns it — here or on a third
// machine that dialled in.
package attach

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"
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

// clientMsg is the browser-terminal protocol, client side.
type clientMsg struct {
	T    string `json:"t"`
	B64  string `json:"b64,omitempty"`
	Cols int    `json:"cols,omitempty"`
	Rows int    `json:"rows,omitempty"`
}

// Run attaches until the session ends, the detach key is pressed, or ctx is
// cancelled. Terminal bytes from the far side go to Out; everything typed into
// In goes back as input.
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
	defer ws.CloseNow()
	ws.SetReadLimit(4 << 20)

	notify := o.Notify
	if notify == nil {
		notify = func(string) {}
	}

	// Input runs in its own goroutine: a blocking read on a terminal cannot be
	// cancelled, so this one outlives Run and dies with the process. It is the
	// reason detaching is a keystroke rather than a context.
	go func() {
		buf := make([]byte, 4<<10)
		for {
			n, err := o.In.Read(buf)
			for i := range n {
				if buf[i] == DetachKey {
					if i > 0 {
						_ = send(ctx, ws, clientMsg{T: "input", B64: base64.StdEncoding.EncodeToString(buf[:i])})
					}
					notify("detached")
					cancel()
					return
				}
			}
			if n > 0 {
				if err := send(ctx, ws, clientMsg{T: "input", B64: base64.StdEncoding.EncodeToString(buf[:n])}); err != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	for {
		typ, data, err := ws.Read(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil // detached, or the caller cancelled
			}
			return nil // the far side hung up; the session usually ended
		}
		switch typ {
		case websocket.MessageBinary:
			if _, err := o.Out.Write(data); err != nil {
				return err
			}
		case websocket.MessageText:
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
				where := fmt.Sprintf("attached to %q (%dx%d)", target.Name, msg.Cols, msg.Rows)
				if !msg.Write {
					where += " read-only"
				}
				notify(where + "; press Ctrl-] to detach")
			case "exit":
				notify("session ended")
				return nil
			case "error":
				return fmt.Errorf("dashboard: %s", msg.Msg)
			}
		}
	}
}

// Resize tells the far side this terminal changed shape.
func Resize(ctx context.Context, ws *websocket.Conn, cols, rows int) error {
	return send(ctx, ws, clientMsg{T: "resize", Cols: cols, Rows: rows})
}

func send(ctx context.Context, ws *websocket.Conn, m clientMsg) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	return ws.Write(wctx, websocket.MessageText, data)
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
