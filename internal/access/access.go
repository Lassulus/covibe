// Package access is covibe's user directory and per-session member list.
//
// Identity itself comes from the OIDC provider (pocket-id); this package only
// records who has been seen and who may see which session. It is deliberately
// separate from the spool: spool records are owned (and rewritten wholesale) by
// the session wrappers, so a dashboard-side ACL stored there would be clobbered
// on the wrapper's next write — and the user directory has to outlive the
// tmpfs spool anyway.
package access

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// User is one known principal. Key is the canonical handle used in member
// lists: the lowercased email when the provider issues one, else the sub.
// Membership checks never rely on Key alone (see Session.Allows) — a user
// invited under any of their spellings still matches on login.
type User struct {
	Key       string    `json:"key"`
	Sub       string    `json:"sub,omitempty"`
	Email     string    `json:"email,omitempty"`
	Name      string    `json:"name,omitempty"`
	Username  string    `json:"username,omitempty"`
	Invited   bool      `json:"invited,omitempty"` // added to a session but never logged in
	FirstSeen time.Time `json:"firstSeen,omitempty"`
	LastSeen  time.Time `json:"lastSeen,omitempty"`
}

// Label is the human-facing name for a user: real name, else email, else key.
func (u User) Label() string {
	if u.Name != "" {
		return u.Name
	}
	if u.Email != "" {
		return u.Email
	}
	return u.Key
}

// Session is the access list of one covibe session.
type Session struct {
	Owner   string    `json:"owner,omitempty"`
	Members []string  `json:"members,omitempty"`
	AddedAt time.Time `json:"addedAt,omitempty"` // when the ACL appeared; grace for Prune
}

// Allows reports whether any of principals owns or was added to the session.
// Principals are the lowercased email, the sub and the lowercased username of
// one identity: an invite spelled as an email still matches a login that only
// carries a sub, and vice versa.
func (a Session) Allows(principals []string) bool {
	for _, p := range principals {
		if p == "" {
			continue
		}
		if a.Owner == p {
			return true
		}
		for _, m := range a.Members {
			if m == p {
				return true
			}
		}
	}
	return false
}

// Key normalizes a user handle: emails and usernames are case-insensitive, an
// opaque sub is left alone apart from surrounding space.
func Key(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// Principals returns the normalized handles an identity may be addressed by.
func Principals(sub, email, username string) []string {
	out := make([]string, 0, 3)
	for _, p := range []string{Key(email), strings.TrimSpace(sub), Key(username)} {
		if p == "" {
			continue
		}
		if !contains(out, p) {
			out = append(out, p)
		}
	}
	return out
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

type state struct {
	Users    map[string]User    `json:"users"`
	Sessions map[string]Session `json:"sessions"`
}

// Store is the persistent directory + ACL map. All methods are safe for
// concurrent use; every mutation rewrites the whole file atomically, which is
// fine for the handful of users and sessions a covibe host carries.
type Store struct {
	path string // empty: in-memory only (tests, no-auth dev)
	mu   sync.Mutex
	st   state
}

// Open loads the store from path, creating neither file nor parent silently:
// the parent directory must exist (the module's tmpfiles rule creates it). An
// empty path yields an in-memory store that persists nothing.
func Open(path string) (*Store, error) {
	s := &Store{path: path, st: state{Users: map[string]User{}, Sessions: map[string]Session{}}}
	if path == "" {
		return s, nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		// First run: prove now that we can actually write there, instead of
		// failing on the first login hours later.
		if err := s.save(); err != nil {
			return nil, err
		}
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read access file: %w", err)
	}
	if err := json.Unmarshal(data, &s.st); err != nil {
		return nil, fmt.Errorf("parse access file %s: %w", path, err)
	}
	if s.st.Users == nil {
		s.st.Users = map[string]User{}
	}
	if s.st.Sessions == nil {
		s.st.Sessions = map[string]Session{}
	}
	return s, nil
}

// save writes the state atomically. Callers hold s.mu.
func (s *Store) save() error {
	if s.path == "" {
		return nil
	}
	data, err := json.MarshalIndent(s.st, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, ".access.*.tmp")
	if err != nil {
		return fmt.Errorf("write access file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, s.path)
}

// Seen records a successful login, upgrading an invited placeholder in place:
// the key stays what the inviter typed, so existing memberships keep matching
// while the entry gains the real sub/name.
func (s *Store) Seen(sub, email, name, username string) User {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	u := User{Sub: sub, Email: email, Name: name, Username: username, FirstSeen: now, LastSeen: now}
	u.Key = Key(email)
	if u.Key == "" {
		u.Key = strings.TrimSpace(sub)
	}
	// An invite may have been spelled as sub or username; adopt that key so the
	// member list does not need rewriting.
	for _, p := range Principals(sub, email, username) {
		if old, ok := s.st.Users[p]; ok {
			u.Key = old.Key
			u.FirstSeen = old.FirstSeen
			if u.FirstSeen.IsZero() {
				u.FirstSeen = now
			}
			break
		}
	}
	if u.Key == "" {
		return User{}
	}
	s.st.Users[u.Key] = u
	_ = s.save()
	return u
}

// Users returns the directory, invited-but-unseen entries included, ordered by
// label so the picker is stable.
func (s *Store) Users() []User {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]User, 0, len(s.st.Users))
	for _, u := range s.st.Users {
		out = append(out, u)
	}
	sort.Slice(out, func(i, j int) bool {
		li, lj := strings.ToLower(out[i].Label()), strings.ToLower(out[j].Label())
		if li != lj {
			return li < lj
		}
		return out[i].Key < out[j].Key
	})
	return out
}

// Lookup resolves a handle to a known user.
func (s *Store) Lookup(handle string) (User, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.st.Users[Key(handle)]
	return u, ok
}

// ACL returns the access list of a session (zero value when it has none).
func (s *Store) ACL(sessionID string) Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.st.Sessions[sessionID]
}

// SetOwner records who created a session. A session without an owner (started
// from the CLI, or registered by a remote wrapper) is visible to admins only.
func (s *Store) SetOwner(sessionID, owner string) error {
	if sessionID == "" {
		return errors.New("no session id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	acl := s.st.Sessions[sessionID]
	acl.Owner = Key(owner)
	if acl.AddedAt.IsZero() {
		acl.AddedAt = time.Now()
	}
	s.st.Sessions[sessionID] = acl
	return s.save()
}

// AddMember grants a handle access to a session. An unknown handle becomes an
// invited directory entry: the point of adding someone is that they can log in
// and find the session waiting, which necessarily happens before first login.
func (s *Store) AddMember(sessionID, handle string) (User, error) {
	key := Key(handle)
	if sessionID == "" || key == "" {
		return User{}, errors.New("session id and user are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	u, known := s.st.Users[key]
	if !known {
		u = User{Key: key, Invited: true}
		if strings.Contains(key, "@") {
			u.Email = key
		} else {
			u.Username = key
		}
		s.st.Users[key] = u
	}
	acl := s.st.Sessions[sessionID]
	if acl.AddedAt.IsZero() {
		acl.AddedAt = time.Now()
	}
	if !contains(acl.Members, key) && acl.Owner != key {
		acl.Members = append(acl.Members, key)
	}
	s.st.Sessions[sessionID] = acl
	return u, s.save()
}

// RemoveMember revokes access. Removing the owner is not possible; a caller
// that wants the session gone kills it.
func (s *Store) RemoveMember(sessionID, handle string) error {
	key := Key(handle)
	s.mu.Lock()
	defer s.mu.Unlock()
	acl, ok := s.st.Sessions[sessionID]
	if !ok {
		return errors.New("no such session")
	}
	kept := acl.Members[:0]
	for _, m := range acl.Members {
		if m != key {
			kept = append(kept, m)
		}
	}
	acl.Members = kept
	s.st.Sessions[sessionID] = acl
	return s.save()
}

// Members resolves a session's member keys to directory entries, in the order
// they were added.
func (s *Store) Members(sessionID string) []User {
	s.mu.Lock()
	defer s.mu.Unlock()
	acl := s.st.Sessions[sessionID]
	out := make([]User, 0, len(acl.Members))
	for _, m := range acl.Members {
		if u, ok := s.st.Users[m]; ok {
			out = append(out, u)
			continue
		}
		out = append(out, User{Key: m})
	}
	return out
}

// Prune drops ACLs of sessions that no longer exist. Entries younger than grace
// are kept: an ACL is written when a session is created, a moment before the
// wrapper writes its spool record, so a fresh entry is not yet orphaned.
// Directory entries are never pruned — the user list is the point.
func (s *Store) Prune(live map[string]bool, grace time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := false
	for id, acl := range s.st.Sessions {
		if live[id] || time.Since(acl.AddedAt) < grace {
			continue
		}
		delete(s.st.Sessions, id)
		changed = true
	}
	if changed {
		_ = s.save()
	}
}

// Drop forgets a session's access list, for a creation that never launched.
func (s *Store) Drop(sessionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.st.Sessions[sessionID]; !ok {
		return nil
	}
	delete(s.st.Sessions, sessionID)
	return s.save()
}
