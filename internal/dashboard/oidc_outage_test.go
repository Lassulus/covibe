package dashboard

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/lassulus/covibe/internal/spool"
)

// The dashboard hosts the collab relay, so an unreachable identity provider must
// not stop it from starting: exiting would take every live session's room down
// with it, and under systemd's restart loop it wipes the room map every few
// seconds. Only login may fail.
func TestUnreachableIssuerStillStarts(t *testing.T) {
	// An issuer that answers 502 (exactly what nginx returns when the IdP is
	// down) must not be fatal.
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "bad gateway", http.StatusBadGateway)
	}))
	defer dead.Close()

	auth, err := NewAuthenticator(context.Background(), OIDCConfig{
		Issuer:      dead.URL,
		ClientID:    "covibe",
		RedirectURL: "https://covibe.example/auth/callback",
	})
	if err != nil {
		t.Fatalf("a 502 from the issuer must not fail startup: %v", err)
	}
	if auth == nil {
		t.Fatal("expected a usable authenticator")
	}

	// The relay and the API surface stay served.
	store, err := spool.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	keys, _ := LoadAPIKeys("k")
	h := NewServer(Config{Store: store, Auth: auth, APIKeys: keys}).Handler()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/v1/sessions", nil)
	req.Header.Set("X-API-Key", "k")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("API must work without the issuer: got %d", rec.Code)
	}

	// Login is the one thing that degrades, and it says so rather than 500ing.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/auth/login", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("login with a dead issuer: got %d want 503", rec.Code)
	}
}

// A missing issuer/client id is a deployment mistake, not an outage: still fatal.
func TestMissingOIDCConfigIsFatal(t *testing.T) {
	if _, err := NewAuthenticator(context.Background(), OIDCConfig{}); err == nil {
		t.Fatal("expected an error when issuer/client id/redirect are unset")
	}
}
