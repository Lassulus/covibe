package dashboard

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/lassulus/covibe/internal/access"
	"github.com/lassulus/covibe/internal/spool"
	"github.com/lassulus/covibe/internal/tmuxctl"
)

// Only a local tmux session on a socket covibe owns can be driven: a zellij
// session, a remote wrapper's session and a CLI session on the default server
// all lack the handle, and the UI must not offer a terminal it cannot open.
func TestTerminalServerRequiresOwnedTmuxSocket(t *testing.T) {
	base := spool.Record{Mux: "tmux", MuxSocket: "/run/covibe/tmux/alice.sock", MuxSession: "s1"}
	if _, ok := terminalServer(base); !ok {
		t.Fatal("a local tmux session on a covibe socket should be drivable")
	}
	for name, rec := range map[string]spool.Record{
		"zellij":     {Mux: "zellij", MuxSocket: base.MuxSocket, MuxSession: "s1"},
		"no socket":  {Mux: "tmux", MuxSession: "s1"},
		"no session": {Mux: "tmux", MuxSocket: base.MuxSocket},
		"remote":     {Mux: "tmux", MuxSocket: base.MuxSocket, MuxSession: "s1", Remote: true},
	} {
		if _, ok := terminalServer(rec); ok {
			t.Errorf("%s: should not be drivable", name)
		}
	}
}

// A view-only session is watch-only for the people it was shared with; the
// owner keeps control, since they can end it regardless.
func TestCanWriteFollowsViewOnly(t *testing.T) {
	acl, _ := access.Open("")
	s := NewServer(Config{Access: acl, Auth: testAuth(OIDCConfig{Admins: []string{"lassulus"}})})
	if err := acl.SetOwner("s1", "bob@example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := acl.AddMember("s1", "alice@example.com"); err != nil {
		t.Fatal(err)
	}
	member := caller{id: aliceID, principals: aliceID.principals()}
	owner := caller{id: bobID, principals: bobID.principals()}

	shared := spool.Record{ID: "s1"}
	if !s.canWrite(shared, member) {
		t.Fatal("member cannot type into a normal shared session")
	}
	viewOnly := spool.Record{ID: "s1", ViewOnly: true}
	if s.canWrite(viewOnly, member) {
		t.Fatal("member may type into a view-only session")
	}
	if !s.canWrite(viewOnly, owner) {
		t.Fatal("owner locked out of their own view-only session")
	}
}

// capture-pane -e emits SGR runs and nothing else, so structured output is a
// run split rather than terminal emulation.
func TestSGRRows(t *testing.T) {
	rows := sgrRows("\x1b[31mRED\x1b[39m plain\n\x1b[1mBOLD\x1b[0m\n")
	if len(rows) != 2 {
		t.Fatalf("got %d rows: %+v", len(rows), rows)
	}
	first := rows[0]
	if len(first) != 2 || first[0].Text != "RED" || len(first[0].SGR) != 1 || first[0].SGR[0] != "31" {
		t.Fatalf("first row runs wrong: %+v", first)
	}
	if first[1].Text != " plain" || len(first[1].SGR) != 1 || first[1].SGR[0] != "39" {
		t.Fatalf("second run wrong: %+v", first[1])
	}
	// A reset carries no parameters, so the trailing run is unstyled.
	second := rows[1]
	if len(second) != 1 || second[0].Text != "BOLD" || len(second[0].SGR) != 1 {
		t.Fatalf("second row wrong: %+v", second)
	}
}

// termServer builds a dashboard whose one live session is a real tmux session
// on a private socket, owned by bob and shared with alice.
func termServer(t *testing.T, paneCmd string) (*Server, string) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not in PATH")
	}
	store, err := spool.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(t.TempDir(), "t.sock")
	srv := tmuxctl.Server{Socket: sock}
	const muxSession = "covibe-test"
	cmd := exec.Command("tmux", "-S", sock, "new-session", "-d", "-s", muxSession, "-x", "80", "-y", "24", paneCmd)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("new-session: %v: %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("tmux", "-S", sock, "kill-server").Run() })
	deadline := time.Now().Add(3 * time.Second)
	for !srv.HasSession(muxSession) {
		if time.Now().After(deadline) {
			t.Fatal("tmux session never appeared")
		}
		time.Sleep(20 * time.Millisecond)
	}

	rec := &spool.Record{
		ID: "term-1", Name: "termy", Status: spool.StatusLive, PID: os.Getpid(),
		Mux: "tmux", MuxSession: muxSession, MuxSocket: sock, StartedAt: time.Now(),
	}
	if err := store.Save(rec); err != nil {
		t.Fatal(err)
	}
	acl, _ := access.Open("")
	s := NewServer(Config{Store: store, Access: acl, Auth: testAuth(OIDCConfig{Admins: []string{"lassulus"}})})
	if err := acl.SetOwner(rec.ID, "bob@example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := acl.AddMember(rec.ID, "alice@example.com"); err != nil {
		t.Fatal(err)
	}
	return s, rec.ID
}

// The screen endpoint reads tmux's grid, so it answers with what is on screen
// rather than with every byte the program ever wrote.
func TestScreenEndpointReturnsRenderedGrid(t *testing.T) {
	s, id := termServer(t, `printf 'AAAAAAAAAAAA\n'; printf '\033[1A\033[5GBBB\n'; cat`)

	var screen string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		rec := as(t, s, bobID, "GET", "/api/v1/sessions/"+id+"/screen?format=text", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("screen: %d %s", rec.Code, rec.Body.String())
		}
		var out struct{ Screen string }
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		screen = out.Screen
		if strings.Contains(screen, "AAAABBBAAAAA") {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !strings.Contains(screen, "AAAABBBAAAAA") {
		t.Fatalf("screen did not render the overwrite:\n%s", screen)
	}

	// cells is the same grid, split into SGR runs.
	rec := as(t, s, bobID, "GET", "/api/v1/sessions/"+id+"/screen?format=cells", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("cells: %d %s", rec.Code, rec.Body.String())
	}
	var cells struct {
		Format string
		Rows   [][]sgrRun
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &cells); err != nil {
		t.Fatal(err)
	}
	if cells.Format != "cells" || len(cells.Rows) == 0 {
		t.Fatalf("unexpected cells payload: %+v", cells)
	}
}

// The terminal is behind the same ACL as everything else, and a session without
// a covibe-driven tmux server says so instead of pretending.
func TestTerminalEndpointsRespectAccess(t *testing.T) {
	s, id := termServer(t, "cat")
	if rec := as(t, s, carolID, "GET", "/api/v1/sessions/"+id+"/screen", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("outsider screen: %d want 404", rec.Code)
	}
	if rec := as(t, s, carolID, "POST", "/api/v1/sessions/"+id+"/input", `{"text":"x"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("outsider input: %d want 404", rec.Code)
	}
	if rec := as(t, s, aliceID, "GET", "/api/v1/sessions/"+id+"/screen", ""); rec.Code != http.StatusOK {
		t.Fatalf("member screen: %d %s", rec.Code, rec.Body.String())
	}

	// A session with no drivable terminal is a conflict, not a 404: the caller
	// can see it, it just has no terminal.
	plain := &spool.Record{ID: "zj-1", Name: "zellij one", Status: spool.StatusLive, PID: os.Getpid(), Mux: "zellij", StartedAt: time.Now()}
	if err := s.cfg.Store.Save(plain); err != nil {
		t.Fatal(err)
	}
	if rec := as(t, s, bossID, "GET", "/api/v1/sessions/zj-1/screen", ""); rec.Code != http.StatusConflict {
		t.Fatalf("zellij screen: %d want 409", rec.Code)
	}

	// The listing tells the UI which rows may offer a terminal.
	views := as(t, s, bossID, "GET", "/api/v1/sessions", "")
	var list []sessionView
	if err := json.Unmarshal(views.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	for _, v := range list {
		switch v.ID {
		case id:
			if !v.HasTerminal || !v.CanWrite {
				t.Fatalf("tmux session view: %+v", v)
			}
		case "zj-1":
			if v.HasTerminal {
				t.Fatalf("zellij session advertises a terminal: %+v", v)
			}
		}
	}
}

// Typing through the REST endpoint reaches the pane.
func TestInputEndpointTypesIntoThePane(t *testing.T) {
	s, id := termServer(t, "cat")
	rec := as(t, s, aliceID, "POST", "/api/v1/sessions/"+id+"/input", `{"text":"hello-from-rest\n"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("input: %d %s", rec.Code, rec.Body.String())
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		got := as(t, s, aliceID, "GET", "/api/v1/sessions/"+id+"/screen?format=text", "")
		if strings.Contains(got.Body.String(), "hello-from-rest") {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("typed text never reached the pane")
}

// The WebSocket is the interactive path: a snapshot on connect, live output
// after it, and keystrokes going the other way.
func TestTerminalWebSocketRoundTrip(t *testing.T) {
	s, id := termServer(t, "cat")
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	jar := httptest.NewRecorder()
	writer := bobID
	writer.Exp = time.Now().Add(time.Hour).Unix()
	s.cfg.Auth.setSigned(jar, authCookie, writer, time.Hour)
	hdr := http.Header{"Cookie": []string{jar.Header().Get("Set-Cookie")}}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/api/v1/sessions/"+id+"/terminal?cols=90&rows=30", &websocket.DialOptions{HTTPHeader: hdr})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer ws.CloseNow()

	typ, data, err := ws.Read(ctx)
	if err != nil || typ != websocket.MessageText {
		t.Fatalf("hello: %v %v %s", typ, err, data)
	}
	var hello struct {
		T     string
		Write bool
		Cols  int
		Rows  int
	}
	if err := json.Unmarshal(data, &hello); err != nil {
		t.Fatal(err)
	}
	if hello.T != "hello" || !hello.Write || hello.Cols != 90 || hello.Rows != 30 {
		t.Fatalf("unexpected hello: %+v", hello)
	}
	// First binary frame is the snapshot, so a late joiner sees the screen.
	if typ, _, err := ws.Read(ctx); err != nil || typ != websocket.MessageBinary {
		t.Fatalf("snapshot frame: %v %v", typ, err)
	}

	send := struct {
		T   string `json:"t"`
		B64 string `json:"b64"`
	}{T: "input", B64: base64.StdEncoding.EncodeToString([]byte("typed-over-ws\n"))}
	payload, _ := json.Marshal(send)
	if err := ws.Write(ctx, websocket.MessageText, payload); err != nil {
		t.Fatal(err)
	}

	var seen []byte
	for {
		typ, data, err := ws.Read(ctx)
		if err != nil {
			t.Fatalf("read: %v (saw %q)", err, seen)
		}
		if typ != websocket.MessageBinary {
			continue
		}
		seen = append(seen, data...)
		if strings.Contains(string(seen), "typed-over-ws") {
			return
		}
	}
}

// A read-only viewer gets the stream but their keystrokes are dropped before
// they reach tmux.
func TestTerminalWebSocketReadOnlyCannotType(t *testing.T) {
	s, id := termServer(t, "cat")
	// Mark the session view-only: members watch, they do not drive.
	rec, err := s.cfg.Store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	rec.ViewOnly = true
	if err := s.cfg.Store.Save(rec); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	jar := httptest.NewRecorder()
	viewer := aliceID
	viewer.Exp = time.Now().Add(time.Hour).Unix()
	s.cfg.Auth.setSigned(jar, authCookie, viewer, time.Hour)
	hdr := http.Header{"Cookie": []string{jar.Header().Get("Set-Cookie")}}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/api/v1/sessions/"+id+"/terminal", &websocket.DialOptions{HTTPHeader: hdr})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer ws.CloseNow()

	_, data, err := ws.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var hello struct{ Write bool }
	if err := json.Unmarshal(data, &hello); err != nil {
		t.Fatal(err)
	}
	if hello.Write {
		t.Fatal("view-only session offered write access")
	}
	payload, _ := json.Marshal(map[string]string{"t": "input", "b64": base64.StdEncoding.EncodeToString([]byte("must-not-appear\n"))})
	if err := ws.Write(ctx, websocket.MessageText, payload); err != nil {
		t.Fatal(err)
	}
	// REST input is refused with a reason, and the pane stays clean.
	if r := as(t, s, aliceID, "POST", "/api/v1/sessions/"+id+"/input", `{"text":"nope\n"}`); r.Code != http.StatusForbidden {
		t.Fatalf("view-only REST input: %d want 403", r.Code)
	}
	time.Sleep(500 * time.Millisecond)
	screen := as(t, s, aliceID, "GET", "/api/v1/sessions/"+id+"/screen?format=text", "").Body.String()
	if strings.Contains(screen, "must-not-appear") || strings.Contains(screen, "nope") {
		t.Fatalf("read-only input reached the pane:\n%s", screen)
	}
}

// capture-pane separates lines with a bare LF. A terminal treats that as "down
// one row, same column", so an unconverted snapshot paints a staircase instead
// of the screen — the bug this guards against was visible in a real browser.
func TestSnapshotUsesCRLF(t *testing.T) {
	got := string(screenCRLF([]byte("line one\nline two\r\nalready\n")))
	want := "line one\r\nline two\r\nalready\r\n"
	if got != want {
		t.Fatalf("screenCRLF = %q, want %q", got, want)
	}

	s, id := termServer(t, `printf 'top-line\n'; printf 'second-line\n'; cat`)
	// The pane program has to have painted before a snapshot can contain it.
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(as(t, s, bobID, "GET", "/api/v1/sessions/"+id+"/screen?format=text", "").Body.String(), "top-line") {
		if time.Now().After(deadline) {
			t.Fatal("pane never painted")
		}
		time.Sleep(50 * time.Millisecond)
	}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	jar := httptest.NewRecorder()
	owner := bobID
	owner.Exp = time.Now().Add(time.Hour).Unix()
	s.cfg.Auth.setSigned(jar, authCookie, owner, time.Hour)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/api/v1/sessions/"+id+"/terminal",
		&websocket.DialOptions{HTTPHeader: http.Header{"Cookie": []string{jar.Header().Get("Set-Cookie")}}})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer ws.CloseNow()
	if _, _, err := ws.Read(ctx); err != nil { // hello
		t.Fatal(err)
	}
	typ, snap, err := ws.Read(ctx)
	if err != nil || typ != websocket.MessageBinary {
		t.Fatalf("snapshot frame: %v %v", typ, err)
	}
	if !strings.Contains(string(snap), "top-line") {
		t.Fatalf("snapshot missing screen content: %q", snap)
	}
	for i, b := range snap {
		if b == '\n' && (i == 0 || snap[i-1] != '\r') {
			t.Fatalf("snapshot contains a bare LF at %d: %q", i, snap)
		}
	}
}

// The terminal is its own page, opened in a new tab: the dashboard's 4s refresh
// must never be able to disturb a live shell, and a shell wants the viewport.
func TestTerminalPage(t *testing.T) {
	s, id := termServer(t, "cat")

	rec := as(t, s, bobID, "GET", "/t/"+id, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("owner page: %d %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"`+id+`"`) {
		t.Fatalf("page does not carry the session id:\n%s", body[:min(len(body), 400)])
	}
	for _, want := range []string{"/assets/xterm.js", "/assets/terminal.js", "covibeTerminal("} {
		if !strings.Contains(body, want) {
			t.Fatalf("page missing %q", want)
		}
	}
	if strings.Contains(body, "read-only") {
		t.Fatal("writable session marked read-only")
	}
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "style-src-attr 'unsafe-inline'") {
		t.Fatalf("terminal page CSP lacks what xterm needs: %s", csp)
	}

	// A member of a view-only session gets the page, marked read-only.
	live, err := s.cfg.Store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	live.ViewOnly = true
	if err := s.cfg.Store.Save(live); err != nil {
		t.Fatal(err)
	}
	if got := as(t, s, aliceID, "GET", "/t/"+id, "").Body.String(); !strings.Contains(got, "read-only") {
		t.Fatal("view-only session not marked read-only on the page")
	}

	// And someone the session was never shared with cannot open it at all.
	if rec := as(t, s, carolID, "GET", "/t/"+id, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("outsider page: %d want 404", rec.Code)
	}
}

// Fast typing arrives as a burst of one-byte frames. Each becomes its own
// send-keys on the control pipe, so ordering has to hold end to end or a
// command line comes out shuffled.
func TestBurstInputKeepsOrder(t *testing.T) {
	s, id := termServer(t, "cat")
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	jar := httptest.NewRecorder()
	owner := bobID
	owner.Exp = time.Now().Add(time.Hour).Unix()
	s.cfg.Auth.setSigned(jar, authCookie, owner, time.Hour)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/api/v1/sessions/"+id+"/terminal",
		&websocket.DialOptions{HTTPHeader: http.Header{"Cookie": []string{jar.Header().Get("Set-Cookie")}}})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer ws.CloseNow()
	if _, _, err := ws.Read(ctx); err != nil { // hello
		t.Fatal(err)
	}

	// One frame per character, as a browser sends them.
	const line = "abcdefghijklmnopqrstuvwxyz0123456789"
	for _, ch := range []byte(line) {
		payload, _ := json.Marshal(map[string]string{"t": "input", "b64": base64.StdEncoding.EncodeToString([]byte{ch})})
		if err := ws.Write(ctx, websocket.MessageText, payload); err != nil {
			t.Fatal(err)
		}
	}

	deadline := time.Now().Add(10 * time.Second)
	var screen string
	for time.Now().Before(deadline) {
		got := as(t, s, bobID, "GET", "/api/v1/sessions/"+id+"/screen?format=text", "")
		var out struct{ Screen string }
		if err := json.Unmarshal(got.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		screen = out.Screen
		if strings.Contains(screen, line) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("burst arrived shuffled or incomplete; screen:\n%s", screen)
}
