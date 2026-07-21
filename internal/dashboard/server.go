package dashboard

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/lassulus/covibe/internal/collab"
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

	// APIKeys authorizes the machine-facing /api/v1 surface. Empty means no key
	// is accepted there (only a logged-in browser session is).
	APIKeys APIKeys
}

// Server serves the OIDC-protected session dashboard.
type Server struct {
	cfg Config
}

// NewServer builds the dashboard server.
func NewServer(cfg Config) *Server {
	if cfg.KeepEnded == 0 {
		cfg.KeepEnded = 20 * time.Second
	}
	return &Server{cfg: cfg}
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

	// Browser endpoints, gated by OIDC (redirects to login).
	protected := http.NewServeMux()
	protected.HandleFunc("/", s.handleIndex)
	protected.HandleFunc("/qr", s.handleQR)
	mux.Handle("/", s.cfg.Auth.Middleware(protected))

	return mux
}

// requireAPI gates a handler on a valid API key or an authenticated session.
func (s *Server) requireAPI(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.APIKeys.Valid(bearerToken(r)) {
			next(w, r)
			return
		}
		if _, ok := s.cfg.Auth.Current(r); ok {
			next(w, r)
			return
		}
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

// viewOf projects a spool record into the API/UI shape, filling defaults.
func (s *Server) viewOf(r spool.Record) sessionView {
	v := sessionView{
		ID:         r.ID,
		Name:       r.Name,
		Dir:        r.Dir,
		Status:     r.Status,
		Mux:        r.Mux,
		MuxSession: r.MuxSession,
		Relay:      orDefault(r.Relay, s.cfg.Relay),
		JoinLink:   r.JoinLink,
		BrowserURL: r.BrowserURL,
		ViewOnly:   r.ViewOnly,
		StartedAt:  r.StartedAt,
	}
	// Fill in a browser URL from server defaults if the wrapper had no
	// relay/web configured but we do.
	if v.BrowserURL == "" && r.JoinLink != "" {
		v.BrowserURL = collab.BrowserURL(r.JoinLink, v.Relay, s.cfg.WebURL)
	}
	if r.JoinLink != "" {
		v.Room = collab.RoomID(r.JoinLink)
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

// handleGetOne returns a single session including its omp remote key (joinLink).
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
		out = collab.StripANSI(out)
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
	_, _ = w.Write(png)
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	id, _ := s.cfg.Auth.Current(r)
	data := struct {
		User      Identity
		Relay     string
		CanCreate bool
	}{User: id, Relay: s.cfg.Relay, CanCreate: s.cfg.Create != nil && s.cfg.WorkspaceRoot != ""}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := indexTmpl.Execute(w, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
