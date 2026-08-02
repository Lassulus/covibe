package dashboard

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"sync"

	"github.com/coder/websocket"

	"github.com/lassulus/covibe/internal/spool"
)

// A remote session's terminal lives on another machine, and the dashboard never
// dials out to it: the wrapper connects here and stays connected, exactly like
// the omp collab host does on /r/. This registry is the meeting point — the
// browser endpoint looks up the host by session id and relays.
//
// Frames match the local terminal socket so the browser side is identical:
// binary carries terminal bytes, text carries control JSON.
type termHosts struct {
	mu    sync.Mutex
	hosts map[string]*termHost
}

func newTermHosts() *termHosts { return &termHosts{hosts: map[string]*termHost{}} }

// termHost is one connected wrapper.
type termHost struct {
	sessionID string
	write     bool // the wrapper accepts input (false for a view-only session)
	cols      int
	rows      int

	ws *websocket.Conn

	mu       sync.Mutex
	closed   bool
	viewers  map[*termViewer]struct{}
	sendCtx  context.Context
	sendStop context.CancelFunc
}

// termViewer is one attached browser, fed by the host's output fan-out.
type termViewer struct {
	out chan []byte
}

// viewerQueue bounds a browser's backlog. Dropping frames for a slow viewer is
// better than stalling the fan-out, which would stall the remote session.
const viewerQueue = 256

// add registers a host, displacing any previous one for that session (a wrapper
// that reconnected after a network drop is the normal case, and the old socket
// may not have been noticed as dead yet).
func (t *termHosts) add(h *termHost) {
	t.mu.Lock()
	old := t.hosts[h.sessionID]
	t.hosts[h.sessionID] = h
	t.mu.Unlock()
	if old != nil && old != h {
		old.close()
	}
}

func (t *termHosts) remove(h *termHost) {
	t.mu.Lock()
	if t.hosts[h.sessionID] == h {
		delete(t.hosts, h.sessionID)
	}
	t.mu.Unlock()
	h.close()
}

func (t *termHosts) get(sessionID string) (*termHost, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	h, ok := t.hosts[sessionID]
	return h, ok
}

// live reports whether a wrapper is currently hosting this session's terminal.
func (t *termHosts) live(sessionID string) bool {
	h, ok := t.get(sessionID)
	return ok && !h.isClosed()
}

func (h *termHost) isClosed() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.closed
}

func (h *termHost) close() {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.closed = true
	viewers := make([]*termViewer, 0, len(h.viewers))
	for v := range h.viewers {
		viewers = append(viewers, v)
	}
	h.viewers = map[*termViewer]struct{}{}
	stop := h.sendStop
	h.mu.Unlock()
	for _, v := range viewers {
		close(v.out)
	}
	if stop != nil {
		stop()
	}
	h.ws.CloseNow()
}

// attach subscribes a viewer and asks the wrapper for a repaint, so a browser
// joining a session that has been running for an hour sees the screen at once.
func (h *termHost) attach() (*termViewer, bool) {
	v := &termViewer{out: make(chan []byte, viewerQueue)}
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil, false
	}
	h.viewers[v] = struct{}{}
	h.mu.Unlock()
	_ = h.control(map[string]any{"t": "snapshot"})
	return v, true
}

func (h *termHost) detach(v *termViewer) {
	h.mu.Lock()
	if _, ok := h.viewers[v]; ok {
		delete(h.viewers, v)
		close(v.out)
	}
	h.mu.Unlock()
}

// broadcast fans terminal bytes out to every viewer.
func (h *termHost) broadcast(data []byte) {
	h.mu.Lock()
	viewers := make([]*termViewer, 0, len(h.viewers))
	for v := range h.viewers {
		viewers = append(viewers, v)
	}
	h.mu.Unlock()
	for _, v := range viewers {
		select {
		case v.out <- data:
		default:
		}
	}
}

// control sends one JSON frame to the wrapper.
func (h *termHost) control(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	h.mu.Lock()
	closed, ctx := h.closed, h.sendCtx
	h.mu.Unlock()
	if closed {
		return context.Canceled
	}
	wctx, cancel := context.WithTimeout(ctx, termWriteTimeout)
	defer cancel()
	return h.ws.Write(wctx, websocket.MessageText, data)
}

func (h *termHost) sendInput(data []byte) error {
	if !h.write {
		return nil
	}
	return h.control(map[string]any{"t": "input", "b64": base64.StdEncoding.EncodeToString(data)})
}

func (h *termHost) resize(cols, rows int) error {
	return h.control(map[string]any{"t": "resize", "cols": cols, "rows": rows})
}

// hostHello is the wrapper's opening frame.
type hostHello struct {
	T     string `json:"t"`
	Cols  int    `json:"cols"`
	Rows  int    `json:"rows"`
	Write bool   `json:"write"`
}

// handleTerminalHost accepts a remote wrapper offering its session's terminal.
// It requires an API key: the announce surface (register/pane) is deliberately
// open because a read-only row is harmless, but an interactive terminal is not —
// without this gate anyone could put a shell of their own into the dashboard and
// wait for someone to type a secret into it.
func (s *Server) handleTerminalHost(w http.ResponseWriter, r *http.Request) {
	c := s.callerOf(r)
	if !c.machine {
		http.Error(w, "terminal hosting requires an API key", http.StatusForbidden)
		return
	}
	rec, ok := s.liveRecord(r.PathValue("id"))
	if !ok {
		http.Error(w, "no such session", http.StatusNotFound)
		return
	}
	// An ended record lingers briefly for the UI; a wrapper that redials into
	// that window must not re-advertise a terminal for a session that is over.
	if rec.Status == spool.StatusEnded || s.killed.has(rec.RoomID) {
		http.Error(w, "session ended", http.StatusGone)
		return
	}
	// The key must be allowed to see the session it offers: an unattributed
	// machine key may host anything, a user key only their own sessions.
	if !c.canSee(s.cfg.Access.ACL(rec.ID)) {
		http.Error(w, "no such session", http.StatusNotFound)
		return
	}
	if !rec.Remote {
		http.Error(w, "session is local; the dashboard drives its tmux directly", http.StatusConflict)
		return
	}

	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: []string{"*"}})
	if err != nil {
		return
	}
	// A wrapper streams a whole session; never let the HTTP layer time it out.
	ctx, cancel := context.WithCancel(context.WithoutCancel(r.Context()))
	defer cancel()

	typ, data, err := ws.Read(ctx)
	if err != nil || typ != websocket.MessageText {
		ws.CloseNow()
		return
	}
	var hello hostHello
	if json.Unmarshal(data, &hello) != nil || hello.T != "hello" {
		ws.CloseNow()
		return
	}
	cols, rows := termSize(hello.Cols, hello.Rows)
	h := &termHost{
		sessionID: rec.ID,
		write:     hello.Write,
		cols:      cols,
		rows:      rows,
		ws:        ws,
		viewers:   map[*termViewer]struct{}{},
		sendCtx:   ctx,
		sendStop:  cancel,
	}
	s.termHosts.add(h)
	defer s.termHosts.remove(h)

	// Read until the wrapper goes away: binary frames are terminal bytes for the
	// viewers, text frames after the hello are ignored.
	ws.SetReadLimit(4 << 20)
	for {
		typ, data, err := ws.Read(ctx)
		if err != nil {
			return
		}
		if typ == websocket.MessageBinary && len(data) > 0 {
			h.broadcast(data)
		}
	}
}

// serveRemoteTerminal relays a browser terminal socket to the wrapper hosting
// that session. The browser side is byte-for-byte the same protocol as a local
// session's terminal, so the frontend does not know the difference.
func (s *Server) serveRemoteTerminal(w http.ResponseWriter, r *http.Request, rec spool.Record, write bool) {
	h, ok := s.termHosts.get(rec.ID)
	if !ok || h.isClosed() {
		http.Error(w, "no terminal host connected for this session", http.StatusConflict)
		return
	}
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: []string{"*"}})
	if err != nil {
		return
	}
	defer ws.CloseNow()
	ctx, cancel := context.WithTimeout(r.Context(), termIdleTimeout)
	defer cancel()

	// A remote session has one size, the wrapper's: a browser cannot resize
	// someone else's terminal out from under them.
	canWrite := write && h.write
	if err := writeTermJSON(ctx, ws, map[string]any{"t": "hello", "write": canWrite, "cols": h.cols, "rows": h.rows}); err != nil {
		return
	}
	v, ok := h.attach()
	if !ok {
		_ = writeTermJSON(ctx, ws, map[string]any{"t": "exit"})
		return
	}
	defer h.detach(v)

	go func() {
		defer cancel()
		for {
			typ, data, err := ws.Read(ctx)
			if err != nil {
				return
			}
			if typ != websocket.MessageText {
				continue
			}
			var msg termClientMsg
			if json.Unmarshal(data, &msg) != nil {
				continue
			}
			switch msg.T {
			case "input":
				if !canWrite {
					continue
				}
				raw, err := base64.StdEncoding.DecodeString(msg.B64)
				if err != nil {
					continue
				}
				_ = h.sendInput(raw)
			case "resize":
				// Only a writer may reshape the pane the others are watching.
				if canWrite {
					cols, rows := termSize(msg.Cols, msg.Rows)
					_ = h.resize(cols, rows)
				}
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case data, open := <-v.out:
			if !open {
				_ = writeTermJSON(ctx, ws, map[string]any{"t": "exit"})
				return
			}
			if err := writeTermBinary(ctx, ws, data); err != nil {
				return
			}
		}
	}
}

// dropTermHost tears down a session's host connection, so killing a session
// closes the browser terminals watching it instead of leaving them hanging on a
// wrapper that is on its way out.
func (s *Server) dropTermHost(sessionID string) {
	if h, ok := s.termHosts.get(sessionID); ok {
		s.termHosts.remove(h)
	}
}
