package dashboard

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

const (
	authCookie = "covibe_auth"
	flowCookie = "covibe_flow"
	sessionTTL = 12 * time.Hour
	flowTTL    = 10 * time.Minute
)

// OIDCConfig configures the in-app auth-code + PKCE login.
type OIDCConfig struct {
	Issuer       string
	ClientID     string
	ClientSecret string // optional (public clients rely on PKCE only)
	RedirectURL  string
	Scopes       []string
	// Allowlists; if all are empty any successfully authenticated user passes.
	AllowedEmails  []string
	AllowedDomains []string
	AllowedSubs    []string
	// CookieSecret signs session/flow cookies. Random when empty (logins do
	// not survive a restart in that case).
	CookieSecret []byte
	// Insecure allows cookies over plain http (localhost/dev only).
	Insecure bool
	// NoAuth disables authentication entirely (loopback dev/testing).
	NoAuth bool
}

// Identity is the authenticated principal carried in the session cookie.
type Identity struct {
	Sub   string `json:"sub"`
	Email string `json:"email"`
	Name  string `json:"name,omitempty"`
	Exp   int64  `json:"exp"`
}

// Authenticator performs the OIDC dance and gates handlers.
type Authenticator struct {
	cfg      OIDCConfig
	secret   []byte
	provider *oidc.Provider
	verifier *oidc.IDTokenVerifier
	oauth    oauth2.Config
}

// NewAuthenticator discovers the issuer and builds the authenticator. In NoAuth
// mode discovery is skipped.
func NewAuthenticator(ctx context.Context, cfg OIDCConfig) (*Authenticator, error) {
	secret := cfg.CookieSecret
	if len(secret) == 0 {
		secret = make([]byte, 32)
		if _, err := rand.Read(secret); err != nil {
			return nil, err
		}
	}
	a := &Authenticator{cfg: cfg, secret: secret}
	if cfg.NoAuth {
		return a, nil
	}
	if cfg.Issuer == "" || cfg.ClientID == "" || cfg.RedirectURL == "" {
		return nil, fmt.Errorf("oidc: issuer, client id and redirect url are required (or set no-auth)")
	}
	provider, err := oidc.NewProvider(ctx, cfg.Issuer)
	if err != nil {
		return nil, fmt.Errorf("oidc discovery: %w", err)
	}
	scopes := cfg.Scopes
	if len(scopes) == 0 {
		scopes = []string{oidc.ScopeOpenID, "profile", "email"}
	}
	a.provider = provider
	a.verifier = provider.Verifier(&oidc.Config{ClientID: cfg.ClientID})
	a.oauth = oauth2.Config{
		ClientID:     cfg.ClientID,
		ClientSecret: cfg.ClientSecret,
		Endpoint:     provider.Endpoint(),
		RedirectURL:  cfg.RedirectURL,
		Scopes:       scopes,
	}
	return a, nil
}

// flowState is the transient state stored in the flow cookie during login.
type flowState struct {
	State    string `json:"s"`
	Verifier string `json:"v"`
	Nonce    string `json:"n"`
	Next     string `json:"r"`
}

// Login starts the auth-code + PKCE flow.
func (a *Authenticator) Login(w http.ResponseWriter, r *http.Request) {
	if a.cfg.NoAuth {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	verifier := oauth2.GenerateVerifier()
	fs := flowState{
		State:    randToken(),
		Verifier: verifier,
		Nonce:    randToken(),
		Next:     sanitizeNext(r.URL.Query().Get("next")),
	}
	a.setSigned(w, flowCookie, fs, flowTTL)
	url := a.oauth.AuthCodeURL(fs.State,
		oauth2.S256ChallengeOption(verifier),
		oidc.Nonce(fs.Nonce),
	)
	http.Redirect(w, r, url, http.StatusFound)
}

// Callback completes the flow, validating state, code, id-token and nonce.
func (a *Authenticator) Callback(w http.ResponseWriter, r *http.Request) {
	if a.cfg.NoAuth {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	var fs flowState
	if !a.getSigned(r, flowCookie, &fs) {
		http.Error(w, "login expired, retry", http.StatusBadRequest)
		return
	}
	a.clearCookie(w, flowCookie)
	if q := r.URL.Query().Get("state"); subtle.ConstantTimeCompare([]byte(q), []byte(fs.State)) != 1 {
		http.Error(w, "state mismatch", http.StatusBadRequest)
		return
	}
	if e := r.URL.Query().Get("error"); e != "" {
		http.Error(w, "provider error: "+e, http.StatusUnauthorized)
		return
	}
	ctx := r.Context()
	tok, err := a.oauth.Exchange(ctx, r.URL.Query().Get("code"), oauth2.VerifierOption(fs.Verifier))
	if err != nil {
		http.Error(w, "token exchange failed", http.StatusUnauthorized)
		return
	}
	rawID, _ := tok.Extra("id_token").(string)
	if rawID == "" {
		http.Error(w, "no id_token in response", http.StatusUnauthorized)
		return
	}
	idToken, err := a.verifier.Verify(ctx, rawID)
	if err != nil {
		http.Error(w, "id_token verification failed", http.StatusUnauthorized)
		return
	}
	if idToken.Nonce != fs.Nonce {
		http.Error(w, "nonce mismatch", http.StatusUnauthorized)
		return
	}
	var claims struct {
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
		Name          string `json:"name"`
	}
	_ = idToken.Claims(&claims)
	if !a.allowed(idToken.Subject, claims.Email) {
		http.Error(w, "account not permitted", http.StatusForbidden)
		return
	}
	id := Identity{
		Sub:   idToken.Subject,
		Email: claims.Email,
		Name:  claims.Name,
		Exp:   time.Now().Add(sessionTTL).Unix(),
	}
	a.setSigned(w, authCookie, id, sessionTTL)
	http.Redirect(w, r, orDefault(fs.Next, "/"), http.StatusFound)
}

// Logout clears the session cookie.
func (a *Authenticator) Logout(w http.ResponseWriter, r *http.Request) {
	a.clearCookie(w, authCookie)
	http.Redirect(w, r, "/", http.StatusFound)
}

// Middleware gates a handler, redirecting unauthenticated browser requests to
// login and returning 401 for API/XHR requests.
func (a *Authenticator) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := a.Current(r); ok {
			next.ServeHTTP(w, r)
			return
		}
		if wantsJSON(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		http.Redirect(w, r, "/auth/login?next="+sanitizeNext(r.URL.RequestURI()), http.StatusFound)
	})
}

// Current returns the authenticated identity for a request.
func (a *Authenticator) Current(r *http.Request) (Identity, bool) {
	if a.cfg.NoAuth {
		return Identity{Sub: "local", Email: "local@covibe"}, true
	}
	var id Identity
	if !a.getSigned(r, authCookie, &id) {
		return Identity{}, false
	}
	if time.Now().Unix() > id.Exp {
		return Identity{}, false
	}
	return id, true
}

func (a *Authenticator) allowed(sub, email string) bool {
	if len(a.cfg.AllowedEmails) == 0 && len(a.cfg.AllowedDomains) == 0 && len(a.cfg.AllowedSubs) == 0 {
		return true
	}
	email = strings.ToLower(email)
	for _, e := range a.cfg.AllowedEmails {
		if strings.EqualFold(e, email) {
			return true
		}
	}
	for _, s := range a.cfg.AllowedSubs {
		if s == sub {
			return true
		}
	}
	if at := strings.LastIndexByte(email, '@'); at >= 0 {
		dom := email[at+1:]
		for _, d := range a.cfg.AllowedDomains {
			if strings.EqualFold(strings.TrimPrefix(d, "@"), dom) {
				return true
			}
		}
	}
	return false
}

// --- signed cookies ---------------------------------------------------------

func (a *Authenticator) setSigned(w http.ResponseWriter, name string, v any, ttl time.Duration) {
	payload, _ := json.Marshal(v)
	value := sign(a.secret, payload)
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		Secure:   !a.cfg.Insecure,
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Now().Add(ttl),
		MaxAge:   int(ttl.Seconds()),
	})
}

func (a *Authenticator) getSigned(r *http.Request, name string, v any) bool {
	c, err := r.Cookie(name)
	if err != nil {
		return false
	}
	payload, ok := unsign(a.secret, c.Value)
	if !ok {
		return false
	}
	return json.Unmarshal(payload, v) == nil
}

func (a *Authenticator) clearCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   !a.cfg.Insecure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

// sign returns base64(payload).base64(hmac(payload)).
func sign(secret, payload []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(payload)
	enc := base64.RawURLEncoding
	return enc.EncodeToString(payload) + "." + enc.EncodeToString(mac.Sum(nil))
}

func unsign(secret []byte, value string) ([]byte, bool) {
	enc := base64.RawURLEncoding
	dot := strings.IndexByte(value, '.')
	if dot < 0 {
		return nil, false
	}
	payload, err := enc.DecodeString(value[:dot])
	if err != nil {
		return nil, false
	}
	want, err := enc.DecodeString(value[dot+1:])
	if err != nil {
		return nil, false
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(payload)
	if !hmac.Equal(want, mac.Sum(nil)) {
		return nil, false
	}
	return payload, true
}

func randToken() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func wantsJSON(r *http.Request) bool {
	return strings.HasPrefix(r.URL.Path, "/api/") ||
		strings.Contains(r.Header.Get("Accept"), "application/json") ||
		r.Header.Get("X-Requested-With") == "XMLHttpRequest"
}

// sanitizeNext keeps only same-origin absolute paths to avoid open redirects.
func sanitizeNext(next string) string {
	if strings.HasPrefix(next, "/") && !strings.HasPrefix(next, "//") {
		return next
	}
	return "/"
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
