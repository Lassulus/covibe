package dashboard

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lassulus/covibe/internal/spool"
)

func TestAPIKeysValid(t *testing.T) {
	ks, err := LoadAPIKeys("alpha, ci=bravo\ncharlie")
	if err != nil {
		t.Fatal(err)
	}
	if !ks.Enabled() {
		t.Fatal("expected keys enabled")
	}
	for _, good := range []string{"alpha", "bravo", "charlie"} {
		if !ks.Valid(good) {
			t.Errorf("key %q should be valid", good)
		}
	}
	for _, bad := range []string{"", "delta", "ci=bravo", "ci"} {
		if ks.Valid(bad) {
			t.Errorf("key %q should be invalid", bad)
		}
	}
	var empty APIKeys
	if empty.Enabled() || empty.Valid("anything") {
		t.Fatal("empty key set must reject everything")
	}
}

func TestLoadAPIKeysFile(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "keys")
	if err := os.WriteFile(f, []byte("# comment\nfoo\nlabel: bar\n\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ks, err := LoadAPIKeys("", f)
	if err != nil {
		t.Fatal(err)
	}
	if !ks.Valid("foo") || !ks.Valid("bar") {
		t.Fatal("file keys should be valid")
	}
	if ks.Valid("# comment") {
		t.Fatal("comment line must not become a key")
	}
}

func TestBearerToken(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Bearer  tok123 ")
	if got := bearerToken(r); got != "tok123" {
		t.Fatalf("bearer got %q", got)
	}
	r2 := httptest.NewRequest("GET", "/", nil)
	r2.Header.Set("X-API-Key", "xk")
	if got := bearerToken(r2); got != "xk" {
		t.Fatalf("x-api-key got %q", got)
	}
}

// newTestServer builds a server backed by a temp spool with one live record.
func newTestServer(t *testing.T, keys APIKeys, noAuth bool) (*Server, *spool.Store) {
	t.Helper()
	store, err := spool.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rec := &spool.Record{
		ID: "s1", Name: "demo", Dir: "/tmp/demo", Status: spool.StatusLive,
		PID: os.Getpid(), BrowserURL: "https://covibe.example/s/s1",
		StartedAt: time.Now(),
	}
	if err := store.Save(rec); err != nil {
		t.Fatal(err)
	}
	s := NewServer(Config{
		Store:   store,
		Auth:    testAuth(OIDCConfig{NoAuth: noAuth}),
		APIKeys: keys,
	})
	return s, store
}

func TestV1AuthGating(t *testing.T) {
	keys, _ := LoadAPIKeys("secret-key")
	s, _ := newTestServer(t, keys, false) // OIDC on, no session cookie
	h := s.Handler()

	// No credentials → 401.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/v1/sessions", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no auth: got %d want 401", rec.Code)
	}

	// Wrong key → 401.
	rec = httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/v1/sessions", nil)
	req.Header.Set("Authorization", "Bearer nope")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong key: got %d want 401", rec.Code)
	}

	// Correct key → 200 with the session listed.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/api/v1/sessions", nil)
	req.Header.Set("Authorization", "Bearer secret-key")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("good key: got %d want 200", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, `"name":"demo"`) {
		t.Fatalf("listing missing session: %s", body)
	}
}

func TestV1GetOne(t *testing.T) {
	keys, _ := LoadAPIKeys("k")
	s, _ := newTestServer(t, keys, false)
	h := s.Handler()

	// The single-session endpoint exposes the session's browser viewer URL.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/v1/sessions/s1", nil)
	req.Header.Set("X-API-Key", "k")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"browserUrl":"https://covibe.example/s/s1"`) {
		t.Fatalf("missing browser url: %s", body)
	}

	// Unknown id → 404.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/api/v1/sessions/nope", nil)
	req.Header.Set("X-API-Key", "k")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown id: got %d want 404", rec.Code)
	}
}

func TestV1PaneReadsSocket(t *testing.T) {
	keys, _ := LoadAPIKeys("k")
	s, store := newTestServer(t, keys, false)

	// Stand up a fake wrapper pane socket serving a snapshot with ANSI.
	sock := store.PanePath("s1")
	ln := listenPane(t, sock, "\x1b[1mhello\x1b[0m world")
	defer ln.Close()

	h := s.Handler()

	// Raw includes escapes.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/v1/sessions/s1/pane", nil)
	req.Header.Set("X-API-Key", "k")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "hello") {
		t.Fatalf("pane raw: code=%d body=%q", rec.Code, rec.Body.String())
	}

	// strip=1 removes escapes.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/api/v1/sessions/s1/pane?strip=1", nil)
	req.Header.Set("X-API-Key", "k")
	h.ServeHTTP(rec, req)
	if got := rec.Body.String(); got != "hello world" {
		t.Fatalf("pane stripped: %q", got)
	}
}

// listenPane serves a fixed snapshot on a unix socket, mimicking the session
// wrapper's pane server.
func listenPane(t *testing.T, sock, data string) net.Listener {
	t.Helper()
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_, _ = c.Write([]byte(data))
			_ = c.Close()
		}
	}()
	return ln
}
