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

	// Web-initiated session creation. Enabled only when both WorkspaceRoot is
	// set and Create is non-nil. Create launches a new omp covibe session named
	// `name` in directory `dir` (already validated + clamped to WorkspaceRoot).
	WorkspaceRoot string
	Create        func(name, dir string) error
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

	// Protected endpoints.
	protected := http.NewServeMux()
	protected.HandleFunc("/", s.handleIndex)
	protected.HandleFunc("/api/sessions", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			s.handleCreate(w, r)
			return
		}
		s.handleSessions(w, r)
	})
	protected.HandleFunc("/qr", s.handleQR)
	mux.Handle("/", s.cfg.Auth.Middleware(protected))

	return mux
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

func (s *Server) views() ([]sessionView, error) {
	recs, err := s.cfg.Store.Live(s.cfg.KeepEnded)
	if err != nil {
		return nil, err
	}
	out := make([]sessionView, 0, len(recs))
	for _, r := range recs {
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
		out = append(out, v)
	}
	return out, nil
}

func (s *Server) handleSessions(w http.ResponseWriter, _ *http.Request) {
	views, err := s.views()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(views)
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
