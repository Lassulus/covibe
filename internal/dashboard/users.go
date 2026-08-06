package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/lassulus/covibe/internal/access"
)

// aclGrace is how long a session's access list survives without a matching
// spool record. A session is authorized the moment it is created, a heartbeat
// before its wrapper writes the record, so a young orphan is normal — but the
// window must also outlast any downtime a session can return from. A suspended
// laptop's record is gone all night while the wrapper reclaims its id in the
// morning (see sessionIDForRoom); pruning in between would silently unshare the
// session and hand it back ownerless.
const aclGrace = 30 * 24 * time.Hour

// memberView is one entry of a session's member list as the dashboard shows it.
type memberView struct {
	Key   string `json:"key"`
	Label string `json:"label"`
}

// label renders a directory handle for display, falling back to the raw handle
// for someone who was invited but has never logged in.
func (s *Server) label(handle string) string {
	if u, ok := s.cfg.Access.Lookup(handle); ok {
		return u.Label()
	}
	return handle
}

// caller is the authorized principal behind a request: an API key — either an
// unattributed machine credential with full access, or one bound to a covibe
// user and acting as them — a logged-in browser identity, or nobody. It is
// computed once by requireAPI and carried in the request context so handlers
// authorize without re-parsing cookies.
type caller struct {
	id      Identity
	machine bool
	// user is set when the credential is a per-user key; the caller then has
	// that user's reach and nothing more.
	user       string
	admin      bool
	principals []string
}

type callerCtxKey struct{}

func withCaller(r *http.Request, c caller) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), callerCtxKey{}, c))
}

// callerOf returns the request's caller, deriving it when the handler was not
// wrapped by requireAPI (the HTML index).
func (s *Server) callerOf(r *http.Request) caller {
	if c, ok := r.Context().Value(callerCtxKey{}).(caller); ok {
		return c
	}
	return s.newCaller(r)
}

func (s *Server) newCaller(r *http.Request) caller {
	if user, ok := s.cfg.APIKeys.Lookup(bearerToken(r)); ok {
		if user == "" {
			// An unattributed key is the operator's own credential: it manages
			// every session, the same way the CLI on the host does.
			return caller{machine: true, admin: true}
		}
		// A user key is that user's credential on another machine: it sees and
		// manages exactly what they would, so a laptop registering a session
		// gets a session they own rather than an ownerless one.
		return caller{
			machine:    true,
			user:       user,
			admin:      s.cfg.Auth.adminHandle([]string{user}),
			principals: []string{user},
		}
	}
	id, ok := s.cfg.Auth.Current(r)
	if !ok {
		return caller{}
	}
	return caller{id: id, admin: s.cfg.Auth.IsAdmin(id), principals: id.principals()}
}

// key is the caller's canonical directory handle, used when recording session
// ownership. Machine callers have none: sessions they create stay unowned and
// therefore admin-visible.
func (c caller) key() string {
	if c.user != "" {
		return c.user
	}
	if c.machine {
		return ""
	}
	if k := access.Key(c.id.Email); k != "" {
		return k
	}
	return strings.TrimSpace(c.id.Sub)
}

// canSee reports whether the caller may see a session and its join links:
// admins and API keys see everything, everyone else only what they own or were
// added to. A session with no owner and no members (started from the CLI, or
// registered by a remote wrapper) is admin-only until someone is added.
func (c caller) canSee(acl access.Session) bool {
	return c.admin || acl.Allows(c.principals)
}

// canManage reports whether the caller may kill a session and change its member
// list: its owner, or an admin.
func (c caller) canManage(acl access.Session) bool {
	if c.admin {
		return true
	}
	if acl.Owner == "" {
		return false
	}
	for _, p := range c.principals {
		if p == acl.Owner {
			return true
		}
	}
	return false
}

// userView is one entry of the member picker's directory.
type userView struct {
	Key      string    `json:"key"`
	Label    string    `json:"label"`
	Email    string    `json:"email,omitempty"`
	Name     string    `json:"name,omitempty"`
	Admin    bool      `json:"admin,omitempty"`
	Invited  bool      `json:"invited,omitempty"`
	LastSeen time.Time `json:"lastSeen,omitempty"`
}

// handleUsers serves the known-user directory: everyone who has logged in, plus
// invited-but-unseen entries. Admin-only — a regular owner adds people by
// typing their address, and has no business enumerating the others.
func (s *Server) handleUsers(w http.ResponseWriter, r *http.Request) {
	c := s.callerOf(r)
	if !c.admin {
		http.Error(w, "admin only", http.StatusForbidden)
		return
	}
	users := s.cfg.Access.Users()
	out := make([]userView, 0, len(users))
	for _, u := range users {
		out = append(out, userView{
			Key:      u.Key,
			Label:    u.Label(),
			Email:    u.Email,
			Name:     u.Name,
			Admin:    s.cfg.Auth.adminHandle(access.Principals(u.Sub, u.Email, u.Username)),
			Invited:  u.Invited,
			LastSeen: u.LastSeen,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// safeHandle constrains a member handle to what an OIDC provider can hand us:
// an email, a username, or an opaque sub. It is stored and echoed back into
// HTML, so junk is rejected at the door.
var safeHandle = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._@+-]{0,127}$`)

// manageable resolves the session in the request path and authorizes the caller
// to change it. A caller who may not even see the session gets 404, so the
// endpoint never confirms that an id exists.
func (s *Server) manageable(w http.ResponseWriter, r *http.Request) (string, bool) {
	c := s.callerOf(r)
	rec, ok := s.liveRecord(r.PathValue("id"))
	if !ok {
		http.Error(w, "no such session", http.StatusNotFound)
		return "", false
	}
	acl := s.cfg.Access.ACL(rec.ID)
	if !c.canSee(acl) {
		http.Error(w, "no such session", http.StatusNotFound)
		return "", false
	}
	if !c.canManage(acl) {
		http.Error(w, "not your session", http.StatusForbidden)
		return "", false
	}
	return rec.ID, true
}

// handleAddMember grants a user access to a session, so they find it (and its
// join link) on their own dashboard after logging in. Unknown handles are
// invited: adding someone before their first login is the normal case.
func (s *Server) handleAddMember(w http.ResponseWriter, r *http.Request) {
	id, ok := s.manageable(w, r)
	if !ok {
		return
	}
	var req struct {
		User string `json:"user"`
	}
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
			http.Error(w, "invalid JSON body", http.StatusBadRequest)
			return
		}
	} else {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "invalid form", http.StatusBadRequest)
			return
		}
		req.User = r.FormValue("user")
	}
	handle := strings.TrimSpace(req.User)
	if handle == "" {
		http.Error(w, "user is required", http.StatusBadRequest)
		return
	}
	if !safeHandle.MatchString(handle) {
		http.Error(w, "invalid user", http.StatusBadRequest)
		return
	}
	u, err := s.cfg.Access.AddMember(id, handle)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"key": u.Key, "label": u.Label()})
}

// handleRemoveMember revokes a member's access.
func (s *Server) handleRemoveMember(w http.ResponseWriter, r *http.Request) {
	id, ok := s.manageable(w, r)
	if !ok {
		return
	}
	if err := s.cfg.Access.RemoveMember(id, r.PathValue("key")); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "removed"})
}
