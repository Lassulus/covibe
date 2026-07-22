package dashboard

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/lassulus/covibe/internal/spool"
)

// Config configures the dashboard server.
type Config struct {
	Store     *spool.Store
	Auth      *Authenticator
	Relay     string        // default relay shown in the UI
	WebURL    string        // default browser UI base for deep links
	KeepEnded time.Duration // how long ended sessions linger in the list
	// Web/API-initiated session creation. Enabled only when both WorkspaceRoot
	// is set and Create is non-nil. Create launches a new omp covibe session
	// with the given id, named `name`, in `dir` (validated + clamped to
	// WorkspaceRoot). The id lets callers immediately GET the new session.
	WorkspaceRoot string
	Create        func(id, name, dir string) error
	// MaxSessions caps concurrent live sessions creatable through the API/UI;
	// 0 means unlimited.
	MaxSessions int

	// APIKeys authorizes the machine-facing /api/v1 surface. Empty means no key
	// is accepted there (only a logged-in browser session is).
	APIKeys APIKeys
}

// Server serves the OIDC-protected session dashboard.
type Server struct {
	cfg   Config
	fails *failLimiter
	relay *Relay
}

// NewServer builds the dashboard server.
func NewServer(cfg Config) *Server {
	if cfg.KeepEnded == 0 {
		cfg.KeepEnded = 20 * time.Second
	}
	originHost := ""
	if cfg.WebURL != "" {
		if u, err := url.Parse(cfg.WebURL); err == nil {
			originHost = u.Host
		}
	}
	// Throttle a client after 10 failed auth attempts per minute.
	return &Server{cfg: cfg, fails: newFailLimiter(10, time.Minute), relay: newRelay(cfg.Store, originHost)}
}

// Handler returns the fully wired http.Handler (auth + routes).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Public endpoints.
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("/auth/login", s.cfg.Auth.Login)
	mux.HandleFunc("/auth/callback", s.cfg.Auth.Callback)
	mux.HandleFunc("/logout", s.cfg.Auth.Logout)

	// Machine-facing REST API: API key (bearer / X-API-Key) OR a logged-in
	// browser session. Never redirects — always 401 JSON on failure.
	mux.HandleFunc("GET /api/v1/sessions", s.requireAPI(s.handleList))
	mux.HandleFunc("POST /api/v1/sessions", s.requireAPI(s.handleCreate))
	mux.HandleFunc("GET /api/v1/sessions/{id}", s.requireAPI(s.handleGetOne))
	mux.HandleFunc("GET /api/v1/sessions/{id}/pane", s.requireAPI(s.handlePane))

	// Collab relay: host = the omp plugin (per-session token, validated inside);
	// guest = the browser viewer (OIDC session or API key). Neither redirects.
	mux.HandleFunc("/collab/host/{id}", s.relay.ServeHost)
	mux.HandleFunc("/collab/guest/{id}", s.requireAPI(s.relay.ServeGuest))

	// Browser endpoints, gated by OIDC (redirects to login).
	protected := http.NewServeMux()
	protected.HandleFunc("/", s.handleIndex)
	protected.HandleFunc("/qr", s.handleQR)
	protected.HandleFunc("/s/{id}", s.handleViewer)
	mux.Handle("/", s.cfg.Auth.Middleware(protected))

	return securityHeaders(mux)
}

// securityHeaders sets conservative headers on every response. The strict CSP
// (with a per-request nonce) is added by the HTML handler; here we set the
// transport-agnostic defenses.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

// requireAPI gates a handler on a valid API key or an authenticated session,
// throttling clients that keep failing.
func (s *Server) requireAPI(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ip := clientIP(r)
		if s.fails.blocked(ip) {
			http.Error(w, "too many failed attempts", http.StatusTooManyRequests)
			return
		}
		if s.cfg.APIKeys.Valid(bearerToken(r)) {
			next(w, r)
			return
		}
		if _, ok := s.cfg.Auth.Current(r); ok {
			next(w, r)
			return
		}
		s.fails.fail(ip)
		w.Header().Set("WWW-Authenticate", `Bearer realm="covibe"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}
}

// sessionView is the JSON shape returned to the browser.
type sessionView struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Dir        string    `json:"dir"`
	Status     string    `json:"status"`
	Mux        string    `json:"mux,omitempty"`
	MuxSession string    `json:"muxSession,omitempty"`
	Relay      string    `json:"relay,omitempty"`
	JoinLink   string    `json:"joinLink,omitempty"`
	BrowserURL string    `json:"browserUrl,omitempty"`
	ViewOnly   bool      `json:"viewOnly"`
	Room       string    `json:"room,omitempty"`
	QR         string    `json:"qr,omitempty"`
	StartedAt  time.Time `json:"startedAt"`
}

func (s *Server) viewOf(r spool.Record) sessionView {
	v := sessionView{
		ID:         r.ID,
		Name:       r.Name,
		Dir:        r.Dir,
		Status:     r.Status,
		Mux:        r.Mux,
		MuxSession: r.MuxSession,
		Relay:      orDefault(r.Relay, s.cfg.Relay),
		BrowserURL: r.BrowserURL,
		ViewOnly:   r.ViewOnly,
		Room:       r.ID,
		StartedAt:  r.StartedAt,
	}
	// Derive the viewer URL from the server's web base when the wrapper had none.
	if v.BrowserURL == "" && s.cfg.WebURL != "" {
		v.BrowserURL = strings.TrimSuffix(s.cfg.WebURL, "/") + "/s/" + r.ID
	}
	if v.BrowserURL != "" {
		v.QR = qrDataURI(v.BrowserURL, 240)
	}
	return v
}

func (s *Server) views() ([]sessionView, error) {
	recs, err := s.cfg.Store.Live(s.cfg.KeepEnded)
	if err != nil {
		return nil, err
	}
	out := make([]sessionView, 0, len(recs))
	for _, r := range recs {
		out = append(out, s.viewOf(r))
	}
	return out, nil
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) handleList(w http.ResponseWriter, _ *http.Request) {
	views, err := s.views()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, views)
}

// liveRecord returns the live record for an id, or false.
func (s *Server) liveRecord(id string) (spool.Record, bool) {
	recs, err := s.cfg.Store.Live(s.cfg.KeepEnded)
	if err != nil {
		return spool.Record{}, false
	}
	for _, r := range recs {
		if r.ID == id {
			return r, true
		}
	}
	return spool.Record{}, false
}

// handleGetOne returns a single session including its browser viewer URL + QR.
func (s *Server) handleGetOne(w http.ResponseWriter, r *http.Request) {
	rec, ok := s.liveRecord(r.PathValue("id"))
	if !ok {
		http.Error(w, "no such session", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, s.viewOf(rec))
}

// handlePane returns a snapshot of the session's terminal output, captured by
// the session wrapper. ?strip=1 removes ANSI escapes for plain text.
func (s *Server) handlePane(w http.ResponseWriter, r *http.Request) {
	rec, ok := s.liveRecord(r.PathValue("id"))
	if !ok {
		http.Error(w, "no such session", http.StatusNotFound)
		return
	}
	out, err := readPane(s.cfg.Store.PanePath(rec.ID))
	if err != nil {
		http.Error(w, "pane unavailable", http.StatusServiceUnavailable)
		return
	}
	if r.URL.Query().Get("strip") == "1" {
		out = stripANSI(out)
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(out)
}

func (s *Server) handleQR(w http.ResponseWriter, r *http.Request) {
	data := r.URL.Query().Get("data")
	if data == "" {
		http.Error(w, "missing data", http.StatusBadRequest)
		return
	}
	size, _ := strconv.Atoi(r.URL.Query().Get("size"))
	png, err := qrPNG(data, size)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(png) // #nosec G705 -- QR PNG served as image/png with nosniff; not HTML
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	id, _ := s.cfg.Auth.Current(r)
	nonce := randToken()
	data := struct {
		User      Identity
		Relay     string
		CanCreate bool
		Nonce     string
	}{User: id, Relay: s.cfg.Relay, CanCreate: s.cfg.Create != nil && s.cfg.WorkspaceRoot != "", Nonce: nonce}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy",
		"default-src 'self'; script-src 'nonce-"+nonce+"'; style-src 'nonce-"+nonce+"'; "+
			"img-src 'self' data:; connect-src 'self'; base-uri 'none'; "+
			"frame-ancestors 'none'; object-src 'none'; form-action 'self'")
	if err := indexTmpl.Execute(w, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
