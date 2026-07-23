package dashboard

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/lassulus/covibe/internal/spool"
)

func TestSanitizeNext(t *testing.T) {
	ok := map[string]string{
		"/":          "/",
		"/dashboard": "/dashboard",
		"/a/b?x=1":   "/a/b?x=1",
	}
	for in, want := range ok {
		if got := sanitizeNext(in); got != want {
			t.Errorf("sanitizeNext(%q)=%q want %q", in, got, want)
		}
	}
	// Anything that could redirect off-origin collapses to "/".
	for _, bad := range []string{
		"", "//evil.com", "/\\evil.com", "/%5cevil.com", "/%2f/evil",
		"https://evil.com", "javascript:alert(1)", "\\\\evil",
	} {
		if got := sanitizeNext(bad); got != "/" {
			t.Errorf("sanitizeNext(%q)=%q want \"/\"", bad, got)
		}
	}
}

func TestFailLimiter(t *testing.T) {
	l := newFailLimiter(3, time.Minute)
	ip := "10.0.0.1"
	for i := range 3 {
		if l.blocked(ip) {
			t.Fatalf("blocked too early at attempt %d", i)
		}
		l.fail(ip)
	}
	if !l.blocked(ip) {
		t.Fatal("should be blocked after exceeding the limit")
	}
	// A different client is unaffected.
	if l.blocked("10.0.0.2") {
		t.Fatal("other IP must not be blocked")
	}
	// Window expiry clears the block.
	l2 := newFailLimiter(1, time.Millisecond)
	l2.fail(ip)
	time.Sleep(3 * time.Millisecond)
	if l2.blocked(ip) {
		t.Fatal("block should expire after the window")
	}
	// nil limiter is a no-op.
	var nilL *failLimiter
	nilL.fail(ip)
	if nilL.blocked(ip) {
		t.Fatal("nil limiter never blocks")
	}
}

func TestRequireAPIThrottles(t *testing.T) {
	keys, _ := LoadAPIKeys("good")
	store, _ := spool.Open(t.TempDir())
	s := NewServer(Config{Store: store, Auth: testAuth(OIDCConfig{}), APIKeys: keys})
	s.fails = newFailLimiter(2, time.Minute)
	h := s.Handler()

	call := func() int {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/api/v1/sessions", nil)
		req.RemoteAddr = "10.9.9.9:1234"
		req.Header.Set("Authorization", "Bearer bad")
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if c := call(); c != http.StatusUnauthorized {
		t.Fatalf("first bad attempt: got %d want 401", c)
	}
	call() // second failure hits the limit
	if c := call(); c != http.StatusTooManyRequests {
		t.Fatalf("after limit: got %d want 429", c)
	}
}

func TestSecurityHeaders(t *testing.T) {
	store, _ := spool.Open(t.TempDir())
	s := NewServer(Config{Store: store, Auth: testAuth(OIDCConfig{NoAuth: true})})
	h := s.Handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("nosniff missing: %q", got)
	}
	if got := rec.Header().Get("X-Frame-Options"); got != "DENY" {
		t.Fatalf("frame-options: %q", got)
	}

	// The HTML index carries a nonce-based CSP matching its script/style nonce.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	csp := rec.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "script-src 'nonce-") || !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Fatalf("weak/missing CSP: %q", csp)
	}
	// Extract the nonce and confirm the served HTML uses the same one.
	start := strings.Index(csp, "'nonce-") + len("'nonce-")
	nonce := csp[start : start+strings.IndexByte(csp[start:], '\'')]
	if nonce == "" || !strings.Contains(rec.Body.String(), `<script nonce="`+nonce+`">`) {
		t.Fatalf("HTML nonce does not match CSP nonce %q", nonce)
	}
}

func TestSessionCap(t *testing.T) {
	dir := t.TempDir()
	store, _ := spool.Open(t.TempDir())
	// Seed one live session.
	_ = store.Save(&spool.Record{ID: "a", Status: spool.StatusLive, PID: os.Getpid(), StartedAt: time.Now()})
	s := NewServer(Config{
		Store: store, Auth: testAuth(OIDCConfig{NoAuth: true}),
		WorkspaceRoot: dir, MaxSessions: 1,
		Create: func(CreateSpec) error { return nil },
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v1/sessions", strings.NewReader(`{"name":"x"}`))
	req.Header.Set("Content-Type", "application/json")
	s.handleCreate(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("at cap: got %d want 429", rec.Code)
	}
}
