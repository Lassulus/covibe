package session

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/creack/pty"
	"golang.org/x/term"

	"github.com/lassulus/covibe/internal/tmuxctl"
)

// The dashboard cannot dial this machine: a remote session sits behind whatever
// NAT its laptop is on, and covibe's remote mode is deliberately outbound-only.
// So the wrapper dials the dashboard's host socket and the dashboard relays
// browser <-> wrapper, mirroring how the collab relay pairs a host with guests.
//
// Wire contract (dashboard side in internal/dashboard):
//
//   - wrapper -> dashboard, once on connect: {"t":"hello","cols":N,"rows":N,"write":bool}
//   - dashboard -> wrapper, text only: {"t":"snapshot"} | {"t":"input","b64":...} |
//     {"t":"resize","cols":N,"rows":N}
//   - wrapper -> dashboard: binary frames carry raw terminal bytes, both the live
//     stream and the snapshot repaint. Text frames from us are the hello only.

// termHostMsg is a control frame in either direction.
type termHostMsg struct {
	T    string `json:"t"`
	B64  string `json:"b64,omitempty"`
	Cols int    `json:"cols,omitempty"`
	Rows int    `json:"rows,omitempty"`
}

const (
	termHostWriteTimeout = 10 * time.Second
	termHostDialTimeout  = 20 * time.Second
	termHostPingInterval = 30 * time.Second
	termHostMinBackoff   = time.Second
	termHostMaxBackoff   = 30 * time.Second
	// A connection that survives this long is treated as healthy, so the next
	// drop retries fast instead of inheriting the previous backoff.
	termHostStable = 30 * time.Second
	// Bounded outbound queue. Full means the socket is not draining; the
	// connection is dropped rather than dropping bytes, because a gap in a
	// terminal stream corrupts the screen while a reconnect repaints it.
	termHostQueue = 256
)

// dashboardSink is a Sink backed by a covibe dashboard that can relay a
// terminal. The session id is read per dial, not cached: RemoteSink re-registers
// (and adopts a new id) whenever the dashboard forgets the session.
type dashboardSink interface {
	DashboardBase() string
	SessionID() string
}

// termSource is the wrapper's terminal, whichever way it can reach it: tmux when
// the session runs on a socket covibe owns, otherwise the omp PTY it proxies.
type termSource interface {
	// start begins streaming at the given size. The returned channel yields raw
	// terminal bytes and is closed when the source ends.
	start(ctx context.Context, cols, rows int) (<-chan []byte, error)
	// snapshot returns a full screen, ready to paint (CRLF line endings).
	snapshot() []byte
	send(b []byte) error
	resize(cols, rows int) error
	// stop releases whatever start acquired. Safe to call without start.
	stop()
}

// termHost keeps the host WebSocket up for as long as the session lives.
type termHost struct {
	sink  dashboardSink
	token string
	write bool // false for a view-only session: we refuse input regardless
	ptmx  *os.File
	src   termSource

	cancel context.CancelFunc
	done   chan struct{}
}

func newTermHost(sink dashboardSink, token string, write bool, ptmx *os.File, src termSource) *termHost {
	return &termHost{sink: sink, token: token, write: write, ptmx: ptmx, src: src, done: make(chan struct{})}
}

func (h *termHost) start() {
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	go h.run(ctx)
}

// stop tears the host down and waits for its goroutines, so a finished session
// never leaves a dialer or a tmux control client behind.
func (h *termHost) stop() {
	h.cancel()
	<-h.done
}

func (h *termHost) run(ctx context.Context) {
	defer close(h.done)
	backoff := termHostMinBackoff
	for ctx.Err() == nil {
		started := time.Now()
		fatal, err := h.serve(ctx)
		if fatal {
			// Bad or removed token: retrying cannot help, and hammering a
			// dashboard with rejected handshakes helps even less.
			fmt.Fprintf(os.Stderr, "covibe: remote terminal disabled: %v\r\n", err)
			return
		}
		if time.Since(started) >= termHostStable {
			backoff = termHostMinBackoff
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > termHostMaxBackoff {
			backoff = termHostMaxBackoff
		}
	}
}

// serve runs one connection to exhaustion. fatal reports an authentication
// failure, the only error worth giving up on.
func (h *termHost) serve(parent context.Context) (fatal bool, err error) {
	id := h.sink.SessionID()
	if id == "" {
		return false, nil // not registered (yet): wait out the backoff
	}

	dialCtx, cancelDial := context.WithTimeout(parent, termHostDialTimeout)
	ws, resp, err := websocket.Dial(dialCtx, termHostURL(h.sink.DashboardBase(), id), &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + h.token}},
	})
	cancelDial()
	if err != nil {
		if resp != nil && (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) {
			return true, err
		}
		return false, err
	}

	ctx, cancel := context.WithCancel(parent)
	var wg sync.WaitGroup
	defer func() {
		cancel()
		_ = ws.CloseNow()
		h.src.stop()
		wg.Wait()
	}()

	// Hello first, on this goroutine: it must be the first frame on the wire,
	// and after this the writer goroutine below is the socket's only writer.
	cols, rows := h.size()
	hello, _ := json.Marshal(map[string]any{"t": "hello", "cols": cols, "rows": rows, "write": h.write})
	hctx, hcancel := context.WithTimeout(ctx, termHostWriteTimeout)
	err = ws.Write(hctx, websocket.MessageText, hello)
	hcancel()
	if err != nil {
		return false, err
	}

	out, err := h.src.start(ctx, cols, rows)
	if err != nil {
		return false, err
	}

	send := make(chan []byte, termHostQueue)
	// enqueue never blocks the producer: a stalled socket must not stall omp or
	// tmux. Overflow drops the connection; the reconnect repaints from scratch.
	enqueue := func(b []byte) {
		select {
		case send <- b:
		default:
			cancel()
		}
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-ctx.Done():
				return
			case b := <-send:
				wctx, wcancel := context.WithTimeout(ctx, termHostWriteTimeout)
				werr := ws.Write(wctx, websocket.MessageBinary, b)
				wcancel()
				if werr != nil {
					cancel()
					return
				}
			}
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-ctx.Done():
				return
			case b, open := <-out:
				if !open {
					cancel() // terminal ended: drop the socket, retry or exit
					return
				}
				enqueue(b)
			}
		}
	}()

	// Ping keeps the relay honest about a half-open connection; a dead peer
	// unblocks the Read below via CloseNow. Safe alongside an in-flight write.
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(termHostPingInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				pctx, pcancel := context.WithTimeout(ctx, termHostWriteTimeout)
				perr := ws.Ping(pctx)
				pcancel()
				if perr != nil {
					_ = ws.CloseNow()
					return
				}
			}
		}
	}()

	return false, h.readLoop(ctx, ws, enqueue)
}

// readLoop applies dashboard messages until the socket dies. Text frames only;
// the dashboard never sends us binary.
func (h *termHost) readLoop(ctx context.Context, ws *websocket.Conn, enqueue func([]byte)) error {
	for {
		typ, data, err := ws.Read(ctx)
		if err != nil {
			return err
		}
		if typ != websocket.MessageText {
			continue
		}
		var msg termHostMsg
		if json.Unmarshal(data, &msg) != nil {
			continue
		}
		switch msg.T {
		case "snapshot":
			// A browser attached: clear its screen, then paint ours.
			enqueue(append([]byte("\x1b[2J\x1b[H"), h.src.snapshot()...))
		case "input":
			if !h.write {
				continue
			}
			raw, derr := base64.StdEncoding.DecodeString(msg.B64)
			if derr != nil {
				continue
			}
			_ = h.src.send(raw)
		case "resize":
			if !h.write {
				continue
			}
			cols, rows := clampTermSize(msg.Cols, msg.Rows)
			_ = h.src.resize(cols, rows)
		}
	}
}

// size reports the wrapper's current terminal size, which is what the session
// renders at until a browser resizes it.
func (h *termHost) size() (int, int) {
	if h.ptmx != nil {
		if rows, cols, err := pty.Getsize(h.ptmx); err == nil {
			return clampTermSize(cols, rows)
		}
	}
	return clampTermSize(0, 0)
}

// clampTermSize keeps a dashboard-reported size to something a pane can be.
func clampTermSize(cols, rows int) (int, int) {
	if cols < 20 || cols > 500 {
		cols = 120
	}
	if rows < 5 || rows > 200 {
		rows = 40
	}
	return cols, rows
}

// termHostURL builds the host endpoint: ws:// for an http dashboard, wss:// for
// https, so a plain-http dev dashboard still works.
func termHostURL(base, id string) string {
	base = strings.TrimSuffix(base, "/")
	switch {
	case strings.HasPrefix(base, "https://"):
		base = "wss://" + strings.TrimPrefix(base, "https://")
	case strings.HasPrefix(base, "http://"):
		base = "ws://" + strings.TrimPrefix(base, "http://")
	}
	return base + "/api/v1/sessions/" + url.PathEscape(id) + "/terminal/host"
}

// screenToCRLF turns a line-separated grid into something a terminal can paint:
// a bare LF moves down a row without returning to column 0, so the snapshot
// would render as a staircase.
func screenToCRLF(screen []byte) []byte {
	out := make([]byte, 0, len(screen)+bytes.Count(screen, []byte("\n")))
	for i, b := range screen {
		if b == '\n' && (i == 0 || screen[i-1] != '\r') {
			out = append(out, '\r')
		}
		out = append(out, b)
	}
	return out
}

// tmuxSource reads the session straight from tmux, which is both the terminal
// emulator (capture-pane returns the rendered grid) and a second view onto the
// pane, so hosting costs the wrapper's own I/O path nothing.
type tmuxSource struct {
	srv     tmuxctl.Server
	session string

	mu     sync.Mutex
	client *tmuxctl.Client
}

func (t *tmuxSource) start(ctx context.Context, cols, rows int) (<-chan []byte, error) {
	c, err := t.srv.Attach(ctx, t.session, cols, rows)
	if err != nil {
		return nil, err
	}
	t.mu.Lock()
	t.client = c
	t.mu.Unlock()
	return c.Output(), nil
}

func (t *tmuxSource) stop() {
	t.mu.Lock()
	c := t.client
	t.client = nil
	t.mu.Unlock()
	if c != nil {
		c.Close()
	}
}

func (t *tmuxSource) snapshot() []byte {
	screen, err := t.srv.Capture(t.session, tmuxctl.CaptureOpts{Escapes: true})
	if err != nil {
		return nil
	}
	return screenToCRLF(screen)
}

func (t *tmuxSource) send(b []byte) error {
	t.mu.Lock()
	c := t.client
	t.mu.Unlock()
	if c == nil {
		return nil
	}
	return c.Send(b)
}

func (t *tmuxSource) resize(cols, rows int) error {
	t.mu.Lock()
	c := t.client
	t.mu.Unlock()
	if c == nil {
		return nil
	}
	return c.Resize(cols, rows)
}

// ptySource is the fallback for a session with no tmux socket: the wrapper is
// already the only reader of omp's PTY, so the live stream is teed off that copy
// and the snapshot is the pane ring the dashboard's preview already uses.
// Snapshot bytes come from the pty as the terminal wrote them, already CRLF
// under the tty's ONLCR; the conversion is a no-op guard for output that is not.
type ptySource struct {
	ptmx *os.File
	pane *paneBuffer
	fan  *fanout

	mu  sync.Mutex
	sub chan []byte
}

func (p *ptySource) start(_ context.Context, _, _ int) (<-chan []byte, error) {
	ch := p.fan.subscribe()
	p.mu.Lock()
	p.sub = ch
	p.mu.Unlock()
	return ch, nil
}

func (p *ptySource) stop() {
	p.mu.Lock()
	ch := p.sub
	p.sub = nil
	p.mu.Unlock()
	if ch != nil {
		p.fan.unsubscribe(ch)
	}
}

func (p *ptySource) snapshot() []byte { return screenToCRLF(p.pane.snapshot()) }

func (p *ptySource) send(b []byte) error {
	_, err := p.ptmx.Write(b)
	return err
}

// resize only applies when nobody is sitting at the wrapper's own terminal.
// Otherwise the pty mirrors that terminal — reshaping it from a browser would
// reflow the screen under the person watching, and the next SIGWINCH would undo
// it anyway.
func (p *ptySource) resize(cols, rows int) error {
	if term.IsTerminal(int(os.Stdin.Fd())) {
		return nil
	}
	return pty.Setsize(p.ptmx, &pty.Winsize{Rows: uint16(rows), Cols: uint16(cols)}) // #nosec G115 -- clampTermSize bounds both
}

// fanout tees terminal output to the host client without adding a second reader
// on the pty: it sits beside paneBuffer in the wrapper's existing MultiWriter.
// It never blocks omp. A subscriber that is not draining is already a dying
// connection — the host's own send queue overflows first and drops the socket,
// which is what gets the browser a clean repaint.
type fanout struct {
	mu   sync.Mutex
	subs map[chan []byte]struct{}
}

func newFanout() *fanout { return &fanout{subs: map[chan []byte]struct{}{}} }

func (f *fanout) Write(b []byte) (int, error) {
	f.mu.Lock()
	if len(f.subs) > 0 {
		// io.Copy reuses its buffer, so subscribers need their own copy.
		cp := make([]byte, len(b))
		copy(cp, b)
		for ch := range f.subs {
			select {
			case ch <- cp:
			default:
			}
		}
	}
	f.mu.Unlock()
	return len(b), nil
}

func (f *fanout) subscribe() chan []byte {
	ch := make(chan []byte, termHostQueue)
	f.mu.Lock()
	f.subs[ch] = struct{}{}
	f.mu.Unlock()
	return ch
}

// unsubscribe closes the channel under the same lock Write sends on, so a
// consumer that goes away can never race a send onto a closed channel.
func (f *fanout) unsubscribe(ch chan []byte) {
	f.mu.Lock()
	if _, ok := f.subs[ch]; ok {
		delete(f.subs, ch)
		close(ch)
	}
	f.mu.Unlock()
}
