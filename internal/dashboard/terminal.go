package dashboard

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/lassulus/covibe/internal/spool"
	"github.com/lassulus/covibe/internal/tmuxctl"
)

// A session is drivable when it runs in tmux on a socket covibe owns: the
// wrapper records that socket, so a CLI-started session on the default server
// (or a zellij session, or a remote one) simply has no terminal in the web UI.
func terminalServer(r spool.Record) (tmuxctl.Server, bool) {
	if r.Remote || r.Mux != "tmux" || r.MuxSocket == "" || r.MuxSession == "" {
		return tmuxctl.Server{}, false
	}
	return tmuxctl.Server{Socket: r.MuxSocket}, true
}

// terminalTarget resolves the session for a terminal request, applying the same
// visibility rule as everything else (404 when the caller may not see it) plus
// the write rule: a view-only session accepts input from nobody but the people
// who could kill it anyway.
func (s *Server) terminalTarget(w http.ResponseWriter, r *http.Request) (spool.Record, tmuxctl.Server, caller, bool) {
	rec, c, ok := s.visibleRecord(w, r)
	if !ok {
		return spool.Record{}, tmuxctl.Server{}, c, false
	}
	srv, ok := terminalServer(rec)
	if !ok {
		http.Error(w, "session has no covibe-driven terminal", http.StatusConflict)
		return spool.Record{}, tmuxctl.Server{}, c, false
	}
	return rec, srv, c, true
}

// canWrite reports whether a caller may type into a session. View-only sessions
// are watch-only for members; the owner and admins keep control because they can
// end the session regardless.
func (s *Server) canWrite(rec spool.Record, c caller) bool {
	if !rec.ViewOnly {
		return true
	}
	return c.canManage(s.cfg.Access.ACL(rec.ID))
}

// screenLimit bounds a scrollback read so one request cannot pull a whole
// session's history into memory.
const screenLimit = 5000

// handleScreen returns the rendered contents of a session's terminal. tmux is
// the emulator here: this is its grid, not a replay of every byte ever written,
// so a full-screen TUI reads as what a human sees.
func (s *Server) handleScreen(w http.ResponseWriter, r *http.Request) {
	rec, srv, _, ok := s.terminalTarget(w, r)
	if !ok {
		return
	}
	format := r.URL.Query().Get("format")
	if format == "" {
		format = "text"
	}
	lines, _ := strconv.Atoi(r.URL.Query().Get("lines"))
	if lines < 0 || lines > screenLimit {
		lines = screenLimit
	}
	opts := tmuxctl.CaptureOpts{Lines: lines, Alt: r.URL.Query().Get("alt") == "1"}
	opts.Escapes = format != "text"
	out, err := srv.Capture(rec.MuxSession, opts)
	if err != nil {
		http.Error(w, "screen unavailable: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	switch format {
	case "text", "ansi":
		writeJSON(w, http.StatusOK, map[string]any{"format": format, "screen": string(out)})
	case "cells":
		writeJSON(w, http.StatusOK, map[string]any{"format": "cells", "rows": sgrRows(string(out))})
	default:
		http.Error(w, "format must be text, ansi or cells", http.StatusBadRequest)
	}
}

// sgrRun is a stretch of screen text sharing one set of SGR parameters.
type sgrRun struct {
	Text string   `json:"text"`
	SGR  []string `json:"sgr,omitempty"`
}

// capture-pane -e emits SGR sequences and nothing else (no cursor motion: the
// grid is already flat), so structured output needs a run splitter rather than a
// terminal emulator.
var sgrRe = regexp.MustCompile(`\x1b\[([0-9;]*)m`)

func sgrRows(screen string) [][]sgrRun {
	lines := strings.Split(strings.TrimRight(screen, "\n"), "\n")
	rows := make([][]sgrRun, 0, len(lines))
	for _, line := range lines {
		var runs []sgrRun
		var sgr []string
		pos := 0
		for _, m := range sgrRe.FindAllStringSubmatchIndex(line, -1) {
			if m[0] > pos {
				runs = append(runs, sgrRun{Text: line[pos:m[0]], SGR: sgr})
			}
			sgr = parseSGR(line[m[2]:m[3]])
			pos = m[1]
		}
		if pos < len(line) {
			runs = append(runs, sgrRun{Text: line[pos:], SGR: sgr})
		}
		rows = append(rows, runs)
	}
	return rows
}

// parseSGR keeps the parameters of one SGR sequence, dropping resets: an empty
// list means default styling.
func parseSGR(params string) []string {
	var out []string
	for _, p := range strings.Split(params, ";") {
		if p == "" || p == "0" {
			continue
		}
		out = append(out, p)
	}
	return out
}

// inputRequest is the REST input body: raw text, or tmux key names for the keys
// a browser cannot express as bytes ("C-c" is a byte, but "Up" or "PageDown"
// are terminfo-dependent sequences tmux knows better than we do).
type inputRequest struct {
	Text string   `json:"text"`
	Keys []string `json:"keys"`
}

// handleInput types into a session's terminal.
func (s *Server) handleInput(w http.ResponseWriter, r *http.Request) {
	rec, srv, c, ok := s.terminalTarget(w, r)
	if !ok {
		return
	}
	if !s.canWrite(rec, c) {
		http.Error(w, "session is view-only", http.StatusForbidden)
		return
	}
	var req inputRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if req.Text == "" && len(req.Keys) == 0 {
		http.Error(w, "text or keys required", http.StatusBadRequest)
		return
	}
	if req.Text != "" {
		if err := srv.SendKeys(rec.MuxSession, []byte(req.Text)); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
	}
	for _, k := range req.Keys {
		if err := srv.SendNamedKey(rec.MuxSession, k); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "sent"})
}

// Terminal WebSocket framing. Terminal bytes ride binary frames straight into
// xterm.js; everything else is a JSON text frame, so the two never need
// escaping or a length prefix.
type termClientMsg struct {
	T    string `json:"t"`
	B64  string `json:"b64,omitempty"`
	Cols int    `json:"cols,omitempty"`
	Rows int    `json:"rows,omitempty"`
}

// termSize clamps a browser-reported size to something a pane can be.
func termSize(cols, rows int) (int, int) {
	if cols < 20 || cols > 500 {
		cols = 120
	}
	if rows < 5 || rows > 200 {
		rows = 40
	}
	return cols, rows
}

const (
	termWriteTimeout = 10 * time.Second
	termIdleTimeout  = 24 * time.Hour
)

// screenCRLF turns capture-pane's line-separated grid into something a terminal
// can paint: a bare LF moves down a row without returning to column 0, so the
// snapshot would render as a staircase.
func screenCRLF(screen []byte) []byte {
	out := make([]byte, 0, len(screen)+bytes.Count(screen, []byte("\n")))
	for i, b := range screen {
		if b == '\n' && (i == 0 || screen[i-1] != '\r') {
			out = append(out, '\r')
		}
		out = append(out, b)
	}
	return out
}

// handleTerminalWS streams a session's terminal to the browser and types the
// browser's keystrokes back. The first binary frame is a full-screen snapshot
// from capture-pane, so a late joiner sees the current screen before any new
// output arrives — the same reason tmux needs no emulator on our side.
func (s *Server) handleTerminalWS(w http.ResponseWriter, r *http.Request) {
	rec, srv, c, ok := s.terminalTarget(w, r)
	if !ok {
		return
	}
	write := s.canWrite(rec, c)
	cols, _ := strconv.Atoi(r.URL.Query().Get("cols"))
	rows, _ := strconv.Atoi(r.URL.Query().Get("rows"))
	cols, rows = termSize(cols, rows)

	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: []string{"*"}})
	if err != nil {
		return
	}
	defer ws.CloseNow()
	ctx, cancel := context.WithTimeout(r.Context(), termIdleTimeout)
	defer cancel()

	client, err := srv.Attach(ctx, rec.MuxSession, cols, rows)
	if err != nil {
		writeTermJSON(ctx, ws, map[string]any{"t": "error", "msg": err.Error()})
		return
	}
	defer client.Close()

	if err := writeTermJSON(ctx, ws, map[string]any{"t": "hello", "write": write, "cols": cols, "rows": rows}); err != nil {
		return
	}
	// Snapshot first: clear the client's screen, then paint tmux's grid.
	if snap, err := srv.Capture(rec.MuxSession, tmuxctl.CaptureOpts{Escapes: true}); err == nil {
		if err := writeTermBinary(ctx, ws, append([]byte("\x1b[2J\x1b[H"), screenCRLF(snap)...)); err != nil {
			return
		}
	}

	go s.termReadLoop(ctx, cancel, ws, client, write)

	for {
		select {
		case <-ctx.Done():
			return
		case data, open := <-client.Output():
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

// termReadLoop applies browser messages. A read-only client's input frames are
// dropped here rather than at the tmux end: the socket stays useful for resize.
func (s *Server) termReadLoop(ctx context.Context, cancel context.CancelFunc, ws *websocket.Conn, client *tmuxctl.Client, write bool) {
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
			if !write {
				continue
			}
			raw, err := base64.StdEncoding.DecodeString(msg.B64)
			if err != nil {
				continue
			}
			_ = client.Send(raw)
		case "resize":
			cols, rows := termSize(msg.Cols, msg.Rows)
			_ = client.Resize(cols, rows)
		}
	}
}

func writeTermJSON(ctx context.Context, ws *websocket.Conn, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, termWriteTimeout)
	defer cancel()
	return ws.Write(wctx, websocket.MessageText, data)
}

func writeTermBinary(ctx context.Context, ws *websocket.Conn, data []byte) error {
	wctx, cancel := context.WithTimeout(ctx, termWriteTimeout)
	defer cancel()
	return ws.Write(wctx, websocket.MessageBinary, data)
}
