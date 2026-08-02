package dashboard

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/lassulus/covibe/internal/access"
	"github.com/lassulus/covibe/internal/spool"
)

// A user key is that user's credential on another machine: it must reach
// exactly what they reach, unlike a machine key which reaches everything.
func TestUserKeyActsAsThatUser(t *testing.T) {
	var keys APIKeys
	if err := keys.AddUserKeys("alice@example.com:alice-token, lassulus:boss-token"); err != nil {
		t.Fatal(err)
	}
	machine, err := LoadAPIKeys("machine-token")
	if err != nil {
		t.Fatal(err)
	}
	keys.digests = append(keys.digests, machine.digests...)
	keys.users = append(keys.users, machine.users...)

	if user, ok := keys.Lookup("alice-token"); !ok || user != "alice@example.com" {
		t.Fatalf("alice token resolved to %q, %v", user, ok)
	}
	if user, ok := keys.Lookup("machine-token"); !ok || user != "" {
		t.Fatalf("machine token resolved to %q, %v", user, ok)
	}
	if _, ok := keys.Lookup("nope"); ok {
		t.Fatal("unknown token accepted")
	}
	if err := keys.AddUserKeys("missing-colon"); err == nil {
		t.Fatal("malformed pair accepted")
	} else if strings.Contains(err.Error(), "missing-colon") {
		t.Fatalf("error echoes the token: %v", err)
	}

	acl, _ := access.Open("")
	s := NewServer(Config{Access: acl, Auth: testAuth(OIDCConfig{Admins: []string{"lassulus"}}), APIKeys: keys})
	req := func(token string) caller {
		r := httptest.NewRequest("GET", "/api/v1/sessions", nil)
		r.Header.Set("Authorization", "Bearer "+token)
		return s.newCaller(r)
	}
	alice := req("alice-token")
	if !alice.machine || alice.user != "alice@example.com" || alice.admin {
		t.Fatalf("alice caller: %+v", alice)
	}
	if alice.key() != "alice@example.com" {
		t.Fatalf("alice key(): %q", alice.key())
	}
	if boss := req("boss-token"); !boss.admin {
		t.Fatalf("a key for a configured admin should be admin: %+v", boss)
	}
	if m := req("machine-token"); m.user != "" || !m.admin {
		t.Fatalf("machine caller: %+v", m)
	}
}

// registerWith announces a remote session, optionally with a token.
func registerWith(t *testing.T, s *Server, token, name string) string {
	t.Helper()
	r := httptest.NewRequest("POST", "/api/v1/sessions/register", strings.NewReader(`{"name":"`+name+`"}`))
	r.Header.Set("Content-Type", "application/json")
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, r)
	if rec.Code != http.StatusCreated {
		t.Fatalf("register %s: %d %s", name, rec.Code, rec.Body.String())
	}
	var out struct{ ID string }
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out.ID
}

func keyedServer(t *testing.T) *Server {
	t.Helper()
	store, err := spool.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	acl, _ := access.Open("")
	var keys APIKeys
	if err := keys.AddUserKeys("alice@example.com:alice-token"); err != nil {
		t.Fatal(err)
	}
	return NewServer(Config{
		Store:   store,
		Access:  acl,
		Auth:    testAuth(OIDCConfig{Admins: []string{"lassulus"}}),
		APIKeys: keys,
	})
}

// The announce surface stays open, but only a keyed announcement belongs to
// someone: an anonymous one is nobody's and therefore admin-only.
func TestRegisterAttributesOnlyWithUserKey(t *testing.T) {
	s := keyedServer(t)
	owned := registerWith(t, s, "alice-token", "owned")
	anon := registerWith(t, s, "", "anon")

	if got := s.cfg.Access.ACL(owned).Owner; got != "alice@example.com" {
		t.Fatalf("keyed register owner = %q", got)
	}
	if got := s.cfg.Access.ACL(anon).Owner; got != "" {
		t.Fatalf("keyless register owner = %q, want none", got)
	}
	if got := listNames(t, as(t, s, aliceID, "GET", "/api/v1/sessions", "")); len(got) != 1 || got[0] != owned {
		t.Fatalf("alice sees %v, want just her own", got)
	}
	if got := listNames(t, as(t, s, bossID, "GET", "/api/v1/sessions", "")); len(got) != 2 {
		t.Fatalf("admin sees %v, want both", got)
	}
	if rec := as(t, s, aliceID, "GET", "/api/v1/sessions/"+anon, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("alice reaching the unowned session: %d want 404", rec.Code)
	}
}

// keyedWS dials a WebSocket with an API key.
func keyedWS(t *testing.T, ctx context.Context, url, token string) *websocket.Conn {
	t.Helper()
	opts := &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": []string{"Bearer " + token}}}
	ws, _, err := websocket.Dial(ctx, url, opts)
	if err != nil {
		t.Fatalf("dial %s: %v", url, err)
	}
	return ws
}

// The whole point of the host socket: a session on another machine becomes
// interactive in the dashboard, with covibe relaying both directions and never
// dialling out.
func TestRemoteTerminalRelay(t *testing.T) {
	s := keyedServer(t)
	id := registerWith(t, s, "alice-token", "remote-term")
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	wsBase := "ws" + strings.TrimPrefix(srv.URL, "http")

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// The wrapper dials in and offers the terminal.
	host := keyedWS(t, ctx, wsBase+"/api/v1/sessions/"+id+"/terminal/host", "alice-token")
	defer host.CloseNow()
	hello, _ := json.Marshal(map[string]any{"t": "hello", "cols": 100, "rows": 30, "write": true})
	if err := host.Write(ctx, websocket.MessageText, hello); err != nil {
		t.Fatal(err)
	}
	// The listing only advertises a terminal once a host is actually connected.
	deadline := time.Now().Add(5 * time.Second)
	for !s.termHosts.live(id) {
		if time.Now().After(deadline) {
			t.Fatal("host never registered")
		}
		time.Sleep(20 * time.Millisecond)
	}
	views := as(t, s, aliceID, "GET", "/api/v1/sessions", "")
	var list []sessionView
	if err := json.Unmarshal(views.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || !list[0].HasTerminal || !list[0].CanWrite {
		t.Fatalf("remote session view: %+v", list)
	}

	// A browser attaches; the host is asked for a repaint and answers with one.
	jar := httptest.NewRecorder()
	viewer := aliceID
	viewer.Exp = time.Now().Add(time.Hour).Unix()
	s.cfg.Auth.setSigned(jar, authCookie, viewer, time.Hour)
	browser, _, err := websocket.Dial(ctx, wsBase+"/api/v1/sessions/"+id+"/terminal",
		&websocket.DialOptions{HTTPHeader: http.Header{"Cookie": []string{jar.Header().Get("Set-Cookie")}}})
	if err != nil {
		t.Fatalf("browser dial: %v", err)
	}
	defer browser.CloseNow()

	typ, data, err := browser.Read(ctx)
	if err != nil || typ != websocket.MessageText {
		t.Fatalf("browser hello: %v %v", typ, err)
	}
	var bh struct {
		T     string
		Write bool
		Cols  int
	}
	if err := json.Unmarshal(data, &bh); err != nil {
		t.Fatal(err)
	}
	if bh.T != "hello" || !bh.Write || bh.Cols != 100 {
		t.Fatalf("browser hello: %+v", bh)
	}

	// Host receives the snapshot request and answers with terminal bytes.
	typ, data, err = host.Read(ctx)
	if err != nil || typ != websocket.MessageText {
		t.Fatalf("snapshot request: %v %v", typ, err)
	}
	var req struct{ T string }
	if err := json.Unmarshal(data, &req); err != nil || req.T != "snapshot" {
		t.Fatalf("want snapshot request, got %s (%v)", data, err)
	}
	if err := host.Write(ctx, websocket.MessageBinary, []byte("screen-from-laptop")); err != nil {
		t.Fatal(err)
	}
	typ, data, err = browser.Read(ctx)
	if err != nil || typ != websocket.MessageBinary || string(data) != "screen-from-laptop" {
		t.Fatalf("browser did not get the repaint: %v %q %v", typ, data, err)
	}

	// Browser keystrokes reach the wrapper.
	in, _ := json.Marshal(map[string]string{"t": "input", "b64": base64.StdEncoding.EncodeToString([]byte("ls\n"))})
	if err := browser.Write(ctx, websocket.MessageText, in); err != nil {
		t.Fatal(err)
	}
	typ, data, err = host.Read(ctx)
	if err != nil || typ != websocket.MessageText {
		t.Fatalf("host input frame: %v %v", typ, err)
	}
	var got struct {
		T   string
		B64 string
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.T != "input" {
		t.Fatalf("want input, got %+v", got)
	}
	if raw, _ := base64.StdEncoding.DecodeString(got.B64); string(raw) != "ls\n" {
		t.Fatalf("input payload = %q", raw)
	}
}

// Hosting is not part of the open announce surface: a terminal nobody
// authorized could be a stranger's shell waiting for a pasted secret.
func TestTerminalHostRequiresAKey(t *testing.T) {
	s := keyedServer(t)
	id := registerWith(t, s, "alice-token", "guarded")
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, resp, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/api/v1/sessions/"+id+"/terminal/host", nil); err == nil {
		t.Fatal("keyless host accepted")
	} else if resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("keyless host: %v (%v)", resp, err)
	}

	// A browser cannot reach a remote terminal that has no host connected.
	rec := as(t, s, aliceID, "GET", "/api/v1/sessions/"+id+"/terminal", "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("hostless terminal: %d want 409", rec.Code)
	}
}

// Killing a session must close the terminals watching it.
func TestKillClosesTheHostedTerminal(t *testing.T) {
	s := keyedServer(t)
	id := registerWith(t, s, "alice-token", "killme")
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	host := keyedWS(t, ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/api/v1/sessions/"+id+"/terminal/host", "alice-token")
	defer host.CloseNow()
	hello, _ := json.Marshal(map[string]any{"t": "hello", "cols": 80, "rows": 24, "write": true})
	if err := host.Write(ctx, websocket.MessageText, hello); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !s.termHosts.live(id) {
		if time.Now().After(deadline) {
			t.Fatal("host never registered")
		}
		time.Sleep(20 * time.Millisecond)
	}

	if rec := as(t, s, aliceID, "DELETE", "/api/v1/sessions/"+id, ""); rec.Code != http.StatusOK {
		t.Fatalf("kill: %d %s", rec.Code, rec.Body.String())
	}
	if s.termHosts.live(id) {
		t.Fatal("host still registered after kill")
	}
	if _, _, err := host.Read(ctx); err == nil {
		t.Fatal("host socket stayed open after kill")
	}
}
