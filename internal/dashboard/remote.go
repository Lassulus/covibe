package dashboard

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/lassulus/covibe/internal/spool"
)

// safeRoomID matches an omp/collablink room id (base64url, 10-64 chars).
var safeRoomID = regexp.MustCompile(`^[A-Za-z0-9_-]{10,64}$`)

// clip strips CR/LF (these fields land in HTML and copy buttons) and caps length.
func clip(s string, n int) string {
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' {
			return -1
		}
		return r
	}, s)
	if len(s) > n {
		return s[:n]
	}
	return s
}

// sessionIDForRoom derives a stable record id from a collab room id. The room is
// minted once per session and is the one identity that survives re-registration,
// so hashing it yields a record id that is stable for the session's whole life
// while keeping the id's shape identical to newSessionID's.
func sessionIDForRoom(room string) string {
	sum := sha256.Sum256([]byte(room))
	return "covibe-" + hex.EncodeToString(sum[:8])
}

// handleRegister records a session hosted by a wrapper on another machine so it
// shows up in the dashboard. Liveness is then driven by the wrapper's heartbeat
// (POST .../pane) and the relay (roomLive), exactly like a local session's pid +
// relay signal. Never trusts a caller-supplied pid/status.
func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	var req spool.RegisterRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	// A killed session must not come back: the wrapper re-registers under a fresh
	// id when the dashboard forgets it, and the room id is the one identity that
	// survives that, so it is what recognises the resurrection.
	if s.killed.has(req.RoomID) {
		http.Error(w, "session was killed", http.StatusGone)
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if err := validateName(req.Name); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.Model != "" && !safeModel.MatchString(req.Model) {
		http.Error(w, "invalid model", http.StatusBadRequest)
		return
	}
	if req.Thinking != "" && !thinkingLevels[req.Thinking] {
		http.Error(w, "invalid thinking level", http.StatusBadRequest)
		return
	}
	if req.RoomID != "" && !safeRoomID.MatchString(req.RoomID) {
		http.Error(w, "invalid room id", http.StatusBadRequest)
		return
	}
	// Derive the record id from the room so a wrapper that re-registers — after a
	// suspend, a dashboard restart, or a GC'd record — reclaims the same identity
	// instead of minting a new one. Owner, member list and terminal host are all
	// keyed by session id, and a random id silently orphaned every one of them.
	// Rooms are unique per session, so distinct sessions still get distinct ids.
	id := newSessionID()
	if req.RoomID != "" {
		id = sessionIDForRoom(req.RoomID)
	}
	// Reclaiming a record is not a new session: keep the original start time so
	// the age column does not reset on every reconnect, and do not charge the
	// reconnect against the session limit, which would lock a returning session
	// out of a full dashboard.
	startedAt := time.Now()
	existing, _ := s.cfg.Store.Load(id)
	if existing != nil && !existing.StartedAt.IsZero() {
		startedAt = existing.StartedAt
	}
	if s.cfg.MaxSessions > 0 && existing == nil {
		if live, _ := s.cfg.Store.Live(s.cfg.KeepEnded); len(live) >= s.cfg.MaxSessions {
			http.Error(w, "session limit reached", http.StatusTooManyRequests)
			return
		}
	}
	rec := &spool.Record{
		ID:         id,
		Name:       req.Name,
		Dir:        clip(req.Dir, 256),
		Model:      req.Model,
		Thinking:   req.Thinking,
		Host:       clip(req.Host, 64),
		Relay:      clip(req.Relay, 256),
		JoinLink:   clip(req.JoinLink, 512),
		BrowserURL: clip(req.BrowserURL, 512),
		RoomID:     req.RoomID,
		ViewOnly:   req.ViewOnly,
		Remote:     true,
		Status:     spool.StatusStarting,
		StartedAt:  startedAt,
	}
	if err := s.cfg.Store.Save(rec); err != nil {
		http.Error(w, "register failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// A per-user key makes the announcement *theirs*: it shows up in that user's
	// dashboard and they can share it. Ownership comes from the key alone, never
	// from an ambient identity — in no-auth mode every request looks like the
	// local user, and a keyless registration must stay ownerless (admin-visible
	// only), since the announce surface is open to any machine.
	if owner := s.callerOf(r).user; owner != "" {
		_ = s.cfg.Access.SetOwner(rec.ID, owner)
	}
	writeJSON(w, http.StatusCreated, map[string]string{"id": rec.ID})
}

// handleRemotePane is the remote wrapper's periodic heartbeat: it bumps the
// record's liveness clock. The reply tells the wrapper whether the session has
// been killed from the dashboard so it can stop omp. A body is read and
// discarded — the screen of a remote session is served live by its terminal
// host, not by a pushed snapshot.
func (s *Server) handleRemotePane(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	rec, err := s.cfg.Store.Load(id)
	if err != nil || !rec.Remote {
		// Killed and already pruned: keep answering stop so a wrapper that checks
		// in after the record is gone still terminates instead of reading the 404
		// as "the dashboard forgot me" and re-registering.
		if s.killed.has(id) {
			writeJSON(w, http.StatusOK, map[string]bool{"stop": true})
			return
		}
		http.Error(w, "no such remote session", http.StatusNotFound)
		return
	}
	if rec.Status == spool.StatusEnded {
		// Killed from the dashboard: signal stop and do not resurrect the
		// heartbeat clock (which would keep the ended record from being pruned).
		writeJSON(w, http.StatusOK, map[string]bool{"stop": true})
		return
	}
	if _, err := io.Copy(io.Discard, http.MaxBytesReader(w, r.Body, 512<<10)); err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	// Re-save to bump UpdatedAt so Alive() keeps the record live.
	if err := s.cfg.Store.Save(rec); err != nil {
		http.Error(w, "heartbeat failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"stop": false})
}
