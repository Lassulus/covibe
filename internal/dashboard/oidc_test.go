package dashboard

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func testAuth(cfg OIDCConfig) *Authenticator {
	return &Authenticator{cfg: cfg, secret: []byte("test-secret-0123456789")}
}

func TestSignRoundTrip(t *testing.T) {
	a := testAuth(OIDCConfig{})
	want := Identity{Sub: "u1", Email: "a@b.c", Exp: time.Now().Add(time.Hour).Unix()}
	rec := httptest.NewRecorder()
	a.setSigned(rec, authCookie, want, time.Hour)

	req := httptest.NewRequest("GET", "/", nil)
	for _, c := range rec.Result().Cookies() {
		req.AddCookie(c)
	}
	var got Identity
	if !a.getSigned(req, authCookie, &got) {
		t.Fatal("failed to read back signed cookie")
	}
	if got.Sub != want.Sub || got.Email != want.Email {
		t.Fatalf("got %+v want %+v", got, want)
	}
}

func TestTamperedCookieRejected(t *testing.T) {
	a := testAuth(OIDCConfig{})
	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(&http.Cookie{Name: authCookie, Value: "eyJzdWIiOiJoYXgifQ.deadbeef"})
	var got Identity
	if a.getSigned(req, authCookie, &got) {
		t.Fatal("tampered cookie should be rejected")
	}
}

func TestAllowed(t *testing.T) {
	cases := []struct {
		name  string
		cfg   OIDCConfig
		sub   string
		email string
		want  bool
	}{
		{"open when no lists", OIDCConfig{}, "x", "any@wherever", true},
		{"email match", OIDCConfig{AllowedEmails: []string{"me@x.io"}}, "s", "me@x.io", true},
		{"email miss", OIDCConfig{AllowedEmails: []string{"me@x.io"}}, "s", "you@x.io", false},
		{"domain match", OIDCConfig{AllowedDomains: []string{"x.io"}}, "s", "anyone@x.io", true},
		{"domain match with @", OIDCConfig{AllowedDomains: []string{"@x.io"}}, "s", "anyone@x.io", true},
		{"domain miss", OIDCConfig{AllowedDomains: []string{"x.io"}}, "s", "anyone@y.io", false},
		{"sub match", OIDCConfig{AllowedSubs: []string{"admin"}}, "admin", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := testAuth(tc.cfg).allowed(tc.sub, tc.email); got != tc.want {
				t.Fatalf("allowed=%v want %v", got, tc.want)
			}
		})
	}
}

func TestMiddlewareGating(t *testing.T) {
	a := testAuth(OIDCConfig{})
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	h := a.Middleware(next)

	// Unauthenticated browser request → redirect to login.
	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("browser: got %d want 302", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc == "" || loc[:11] != "/auth/login" {
		t.Fatalf("expected redirect to /auth/login, got %q", loc)
	}

	// Unauthenticated API request → 401.
	apiReq := httptest.NewRequest("GET", "/api/sessions", nil)
	apiRec := httptest.NewRecorder()
	h.ServeHTTP(apiRec, apiReq)
	if apiRec.Code != http.StatusUnauthorized {
		t.Fatalf("api: got %d want 401", apiRec.Code)
	}

	// Authenticated request → passes through.
	authRec := httptest.NewRecorder()
	a.setSigned(authRec, authCookie, Identity{Sub: "u", Exp: time.Now().Add(time.Hour).Unix()}, time.Hour)
	goodReq := httptest.NewRequest("GET", "/", nil)
	for _, c := range authRec.Result().Cookies() {
		goodReq.AddCookie(c)
	}
	goodRec := httptest.NewRecorder()
	h.ServeHTTP(goodRec, goodReq)
	if goodRec.Code != http.StatusOK {
		t.Fatalf("authenticated: got %d want 200", goodRec.Code)
	}
}

func TestNoAuthAlwaysCurrent(t *testing.T) {
	a := testAuth(OIDCConfig{NoAuth: true})
	if _, ok := a.Current(httptest.NewRequest("GET", "/", nil)); !ok {
		t.Fatal("no-auth mode should always yield an identity")
	}
}

func TestExpiredSessionRejected(t *testing.T) {
	a := testAuth(OIDCConfig{})
	rec := httptest.NewRecorder()
	a.setSigned(rec, authCookie, Identity{Sub: "u", Exp: time.Now().Add(-time.Minute).Unix()}, time.Hour)
	req := httptest.NewRequest("GET", "/", nil)
	for _, c := range rec.Result().Cookies() {
		req.AddCookie(c)
	}
	if _, ok := a.Current(req); ok {
		t.Fatal("expired session should be rejected")
	}
}

func TestQRPNG(t *testing.T) {
	png, err := qrPNG("https://my.omp.sh/#room.key", 128)
	if err != nil || len(png) < 8 || string(png[1:4]) != "PNG" {
		t.Fatalf("expected PNG bytes, err=%v len=%d", err, len(png))
	}
}
