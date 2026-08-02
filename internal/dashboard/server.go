package dashboard

import (
	"encoding/json"
	"net/http"
	"os"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/lassulus/covibe/internal/access"
	"github.com/lassulus/covibe/internal/mux"
	"github.com/lassulus/covibe/internal/spool"
)

// Config configures the dashboard server.
type Config struct {
	Store     *spool.Store
	Access    *access.Store // user directory + per-session member lists
	Auth      *Authenticator
	RelayHost string        // public host for guest collab links, e.g. "covibe.lassul.us"
	WebClient string        // collab-web client base for browser links, e.g. "https://my.omp.sh"
	WebRoot   string        // dir of collab-web static assets served at /c/ (self-hosted client); empty uses WebClient (e.g. my.omp.sh)
	KeepEnded time.Duration // how long ended sessions linger in the list
	// Web/API-initiated session creation. Enabled only when both WorkspaceRoot
	// is set and Create is non-nil. Create launches a new omp covibe session
	// with the given id, named `name`, in `dir` (validated + clamped to
	// WorkspaceRoot). The id lets callers immediately GET the new session.
	WorkspaceRoot string
	Create        func(CreateSpec) error
	// MaxSessions caps concurrent live sessions creatable through the API/UI;
	// 0 means unlimited.
	MaxSessions int
	// Models optionally restricts the create-form dropdown to these selectors
	// (COVIBE_MODELS); empty offers every auth-resolvable omp model.
	Models []string
	// OmpBin is the omp binary used to enumerate available models.
	OmpBin string

	// APIKeys authorizes the machine-facing /api/v1 surface. Empty means no key
	// is accepted there (only a logged-in browser session is).
	APIKeys APIKeys
}

// Server serves the OIDC-protected session dashboard.
type Server struct {
	cfg      Config
	fails    *failLimiter
	relay    *Relay
	killed   *killRegistry
	mmu      sync.Mutex
	models   []ModelOption
	modelsAt time.Time
}

// NewServer builds the dashboard server.
func NewServer(cfg Config) *Server {
	if cfg.KeepEnded == 0 {
		cfg.KeepEnded = 20 * time.Second
	}
	// An in-memory directory keeps the no-auth dev dashboard and the tests
	// working without a state file; Open never fails for the empty path.
	if cfg.Access == nil {
		cfg.Access, _ = access.Open("")
	}
	// Throttle a client after 10 failed auth attempts per minute.
	s := &Server{cfg: cfg, fails: newFailLimiter(10, time.Minute), relay: newRelay(), killed: newKillRegistry()}
	if cfg.Auth != nil {
		cfg.Auth.OnLogin = func(id Identity) {
			s.cfg.Access.Seen(id.Sub, id.Email, id.Name, id.Username)
		}
	}
	return s
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
	mux.HandleFunc("DELETE /api/v1/sessions/{id}", s.requireAPI(s.handleKill))
	mux.HandleFunc("POST /api/v1/sessions/{id}/members", s.requireAPI(s.handleAddMember))
	mux.HandleFunc("DELETE /api/v1/sessions/{id}/members/{key}", s.requireAPI(s.handleRemoveMember))
	mux.HandleFunc("GET /api/v1/users", s.requireAPI(s.handleUsers))
	mux.HandleFunc("GET /api/v1/models", s.requireAPI(s.handleModels))
	// Open announce surface (no key): any machine can register the session it is
	// hosting (name + collab links) and push its pane. An announcement is kept
	// only while the announcer keeps heartbeating (pane push) and GC'd when it
	// stops; the server-minted session id is the capability for pushing a given
	// session's pane. Killing (DELETE) stays gated — only the dashboard owner.
	mux.HandleFunc("POST /api/v1/sessions/register", s.handleRegister)
	mux.HandleFunc("POST /api/v1/sessions/{id}/pane", s.handleRemotePane)

	// Content-blind collab relay: omp host + `omp join`/collab-web guests. No
	// auth — possession of the room key (in the link fragment) is the trust
	// boundary, exactly like omp's public relay.
	mux.HandleFunc("/r/{roomId}", s.relay.ServeRelay)

	// Self-hosted collab-web client (static SPA) at /c/. Public: the client is
	// content-blind and the room key rides only in the URL fragment.
	if s.cfg.WebRoot != "" {
		mux.Handle("/c/", http.StripPrefix("/c/", http.FileServer(http.Dir(s.cfg.WebRoot))))
	}

	// Browser endpoints, gated by OIDC (redirects to login).
	protected := http.NewServeMux()
	protected.HandleFunc("/", s.handleIndex)
	protected.HandleFunc("/qr", s.handleQR)
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
// throttling clients that keep failing. The resolved caller rides in the
// request context: every handler below authorizes against it rather than
// re-deriving who is asking.
func (s *Server) requireAPI(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ip := clientIP(r)
		if s.fails.blocked(ip) {
			http.Error(w, "too many failed attempts", http.StatusTooManyRequests)
			return
		}
		// An identity always carries at least a sub, so a caller with no
		// principals and no key is simply not authenticated.
		if c := s.newCaller(r); c.machine || len(c.principals) > 0 {
			next(w, withCaller(r, c))
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
	Host       string    `json:"host,omitempty"`
	Status     string    `json:"status"`
	Mux        string    `json:"mux,omitempty"`
	MuxSession string    `json:"muxSession,omitempty"`
	Model      string    `json:"model,omitempty"`
	Thinking   string    `json:"thinking,omitempty"`
	Relay      string    `json:"relay,omitempty"`
	JoinLink   string    `json:"joinLink,omitempty"`
	BrowserURL string    `json:"browserUrl,omitempty"`
	ViewOnly   bool      `json:"viewOnly"`
	Room       string    `json:"room,omitempty"`
	StartedAt  time.Time `json:"startedAt"`
	// Access control, resolved per caller: Owner/Members describe who may see
	// the session, CanManage whether this caller may change that (and kill it).
	Owner     string       `json:"owner,omitempty"`
	Members   []memberView `json:"members,omitempty"`
	CanManage bool         `json:"canManage"`
}

func (s *Server) viewOf(r spool.Record, c caller) sessionView {
	v := sessionView{
		ID:         r.ID,
		Name:       r.Name,
		Dir:        r.Dir,
		Host:       r.Host,
		Status:     r.Status,
		Mux:        r.Mux,
		MuxSession: r.MuxSession,
		Model:      r.Model,
		Thinking:   r.Thinking,
		ViewOnly:   r.ViewOnly,
		Room:       r.RoomID,
		StartedAt:  r.StartedAt,
	}
	// A host connected to the relay is the true "live" signal (the record itself
	// only flips to ended when the wrapper exits) — and the only state in which
	// the join links actually work: the relay rejects a guest for a room with no
	// host ("session ended: no such room"). So the links are published only while
	// a host is on the relay; a registered-but-hostless session shows as waiting
	// instead of handing out a dead link. The QR is not part of the payload: the
	// overview renders it on demand from /qr, so a hidden QR costs nothing.
	if r.Status != spool.StatusEnded && r.RoomID != "" && s.relay.roomLive(r.RoomID) {
		v.Status = spool.StatusLive
		v.JoinLink = r.JoinLink
		v.BrowserURL = r.BrowserURL
	}
	acl := s.cfg.Access.ACL(r.ID)
	v.CanManage = c.canManage(acl)
	if acl.Owner != "" {
		v.Owner = s.label(acl.Owner)
	}
	for _, m := range s.cfg.Access.Members(r.ID) {
		v.Members = append(v.Members, memberView{Key: m.Key, Label: m.Label()})
	}
	return v
}

// handleKill terminates a session: signal the wrapper process, which tears down
// omp (and thus the native collab host) and marks the record ended.
func (s *Server) handleKill(w http.ResponseWriter, r *http.Request) {
	c := s.callerOf(r)
	rec, ok := s.liveRecord(r.PathValue("id"))
	if !ok {
		http.Error(w, "no such session", http.StatusNotFound)
		return
	}
	// Members can join a session; ending it belongs to its owner (and admins).
	acl := s.cfg.Access.ACL(rec.ID)
	if !c.canSee(acl) {
		http.Error(w, "no such session", http.StatusNotFound)
		return
	}
	if !c.canManage(acl) {
		http.Error(w, "not your session", http.StatusForbidden)
		return
	}
	if !rec.Remote {
		if rec.PID > 0 {
			_ = syscall.Kill(rec.PID, syscall.SIGTERM)
		}
		mux.Kill(rec.Mux, rec.MuxSession)
	}
	// A remote wrapper lives on another machine: signaling rec.PID here could hit
	// an unrelated local process. The wrapper's next heartbeat sees the ended
	// record (or the tombstone below, once the record is pruned) and stops omp.
	s.killed.remember(rec.ID, rec.RoomID)
	rec.Status = spool.StatusEnded
	_ = s.cfg.Store.Save(&rec)
	writeJSON(w, http.StatusOK, map[string]string{"status": "killed", "id": rec.ID})
}

// views returns the sessions this caller may see. Listing is also when ACLs of
// long-gone sessions are dropped: it runs on every dashboard tick, and the
// live set is right here.
func (s *Server) views(c caller) ([]sessionView, error) {
	recs, err := s.cfg.Store.Live(s.cfg.KeepEnded)
	if err != nil {
		return nil, err
	}
	live := make(map[string]bool, len(recs))
	for _, r := range recs {
		live[r.ID] = true
	}
	s.cfg.Access.Prune(live, aclGrace)
	out := make([]sessionView, 0, len(recs))
	for _, r := range recs {
		if !c.canSee(s.cfg.Access.ACL(r.ID)) {
			continue
		}
		out = append(out, s.viewOf(r, c))
	}
	return out, nil
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	views, err := s.views(s.callerOf(r))
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

// visibleRecord resolves the session in the request path for callers allowed to
// see it. An unauthorized caller gets the same 404 as a missing id, so the
// endpoint never confirms that someone else's session exists.
func (s *Server) visibleRecord(w http.ResponseWriter, r *http.Request) (spool.Record, caller, bool) {
	c := s.callerOf(r)
	rec, ok := s.liveRecord(r.PathValue("id"))
	if !ok || !c.canSee(s.cfg.Access.ACL(rec.ID)) {
		http.Error(w, "no such session", http.StatusNotFound)
		return spool.Record{}, c, false
	}
	return rec, c, true
}

// handleGetOne returns a single session including its browser viewer URL + QR.
func (s *Server) handleGetOne(w http.ResponseWriter, r *http.Request) {
	rec, c, ok := s.visibleRecord(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, s.viewOf(rec, c))
}

// handlePane returns a snapshot of the session's terminal output, captured by
// the session wrapper. ?strip=1 removes ANSI escapes for plain text.
func (s *Server) handlePane(w http.ResponseWriter, r *http.Request) {
	rec, _, ok := s.visibleRecord(w, r)
	if !ok {
		return
	}
	var out []byte
	var err error
	if rec.Remote {
		out, err = os.ReadFile(s.cfg.Store.PaneFilePath(rec.ID))
	} else {
		out, err = readPane(s.cfg.Store.PanePath(rec.ID))
	}
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
		IsAdmin   bool
		Relay     string
		CanCreate bool
		Models    []string
		Nonce     string
	}{
		User:      id,
		IsAdmin:   s.cfg.Auth.IsAdmin(id),
		Relay:     s.cfg.RelayHost,
		CanCreate: s.cfg.Create != nil && s.cfg.WorkspaceRoot != "",
		Models:    s.cfg.Models,
		Nonce:     nonce,
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy",
		"default-src 'self'; script-src 'nonce-"+nonce+"'; style-src 'nonce-"+nonce+"'; "+
			"img-src 'self' data:; connect-src 'self'; base-uri 'none'; "+
			"frame-ancestors 'none'; object-src 'none'; form-action 'self'")
	if err := indexTmpl.Execute(w, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
