package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lassulus/covibe/internal/access"
	"github.com/lassulus/covibe/internal/spool"
)

// aliceID/bobID/carolID are three pocket-id style logins; boss is configured as
// an admin.
var (
	aliceID = Identity{Sub: "sub-alice", Email: "alice@example.com", Name: "Alice", Username: "alice"}
	bobID   = Identity{Sub: "sub-bob", Email: "bob@example.com", Name: "Bob", Username: "bob"}
	carolID = Identity{Sub: "sub-carol", Email: "carol@example.com", Name: "Carol", Username: "carol"}
	bossID  = Identity{Sub: "sub-boss", Email: "boss@example.com", Name: "Boss", Username: "lassulus"}
)

// aclServer builds a dashboard with three live sessions and a real (cookie
// based) authenticator whose admin is "lassulus":
//
//	s-alice   owned by alice
//	s-shared  owned by bob, alice added
//	s-bob     owned by bob
func aclServer(t *testing.T) *Server {
	t.Helper()
	store, err := spool.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// Remote records: their liveness is the heartbeat TTL, so a kill in this test
	// tears down nothing local (a local record would carry a real pid, and the
	// only pid a test can honestly claim is its own).
	for _, id := range []string{"s-alice", "s-shared", "s-bob"} {
		rec := &spool.Record{ID: id, Name: id, Status: spool.StatusLive, Remote: true, StartedAt: time.Now()}
		if err := store.Save(rec); err != nil {
			t.Fatal(err)
		}
	}
	acl, err := access.Open("")
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(Config{
		Store:  store,
		Access: acl,
		Auth:   testAuth(OIDCConfig{Admins: []string{"lassulus"}}),
	})
	for _, id := range []Identity{aliceID, bobID, carolID, bossID} {
		s.cfg.Auth.OnLogin(id)
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(acl.SetOwner("s-alice", "alice@example.com"))
	must(acl.SetOwner("s-shared", "bob@example.com"))
	must(acl.SetOwner("s-bob", "bob@example.com"))
	_, err = acl.AddMember("s-shared", "alice@example.com")
	must(err)
	return s
}

// as issues a request carrying id's session cookie, exactly as a browser would.
func as(t *testing.T, s *Server, id Identity, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	if id.Sub != "" {
		id.Exp = time.Now().Add(time.Hour).Unix()
		jar := httptest.NewRecorder()
		s.cfg.Auth.setSigned(jar, authCookie, id, time.Hour)
		r.Header.Set("Cookie", jar.Header().Get("Set-Cookie"))
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, r)
	return rec
}

func listNames(t *testing.T, rec *httptest.ResponseRecorder) []string {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}
	var views []sessionView
	if err := json.Unmarshal(rec.Body.Bytes(), &views); err != nil {
		t.Fatalf("decode list: %v (%s)", err, rec.Body.String())
	}
	out := make([]string, 0, len(views))
	for _, v := range views {
		out = append(out, v.ID)
	}
	return out
}

// The listing is the whole point of membership: a user sees the sessions they
// own or were added to, an admin sees everything, and a stranger sees nothing —
// including no join links they could scan.
func TestListIsScopedToTheCaller(t *testing.T) {
	s := aclServer(t)

	if got := listNames(t, as(t, s, aliceID, "GET", "/api/v1/sessions", "")); len(got) != 2 ||
		!contains(got, "s-alice") || !contains(got, "s-shared") {
		t.Fatalf("alice sees %v, want s-alice + s-shared", got)
	}
	if got := listNames(t, as(t, s, bobID, "GET", "/api/v1/sessions", "")); len(got) != 2 ||
		!contains(got, "s-shared") || !contains(got, "s-bob") {
		t.Fatalf("bob sees %v, want his two", got)
	}
	if got := listNames(t, as(t, s, carolID, "GET", "/api/v1/sessions", "")); len(got) != 0 {
		t.Fatalf("outsider sees %v, want nothing", got)
	}
	if got := listNames(t, as(t, s, bossID, "GET", "/api/v1/sessions", "")); len(got) != 3 {
		t.Fatalf("admin sees %v, want all three", got)
	}
}

// canManage is what the UI keys the kill/share controls off: only the owner and
// admins get it, a mere member does not.
func TestCanManageOnlyForOwnerAndAdmin(t *testing.T) {
	s := aclServer(t)
	manage := func(id Identity, session string) bool {
		t.Helper()
		rec := as(t, s, id, "GET", "/api/v1/sessions/"+session, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("get %s as %s: %d", session, id.Email, rec.Code)
		}
		var v sessionView
		if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
			t.Fatal(err)
		}
		return v.CanManage
	}
	if manage(aliceID, "s-shared") {
		t.Fatal("member may manage someone else's session")
	}
	if !manage(bobID, "s-shared") {
		t.Fatal("owner may not manage own session")
	}
	if !manage(bossID, "s-shared") {
		t.Fatal("admin may not manage")
	}
}

// A session nobody added you to must not even acknowledge its existence, or the
// dashboard leaks who is working on what.
func TestOutsiderGetsNotFound(t *testing.T) {
	s := aclServer(t)
	for _, path := range []string{"/api/v1/sessions/s-alice", "/api/v1/sessions/s-alice/screen"} {
		if rec := as(t, s, carolID, "GET", path, ""); rec.Code != http.StatusNotFound {
			t.Fatalf("%s: %d want 404", path, rec.Code)
		}
	}
	// A member reaches the session they were added to, which proves the ACL and
	// not the record decides who sees what.
	if rec := as(t, s, aliceID, "GET", "/api/v1/sessions/s-shared", ""); rec.Code != http.StatusOK {
		t.Fatalf("member denied a session they were added to: %d", rec.Code)
	}
}

// Killing is an owner/admin action; a member joins, they do not end the session.
func TestKillRequiresManageRights(t *testing.T) {
	s := aclServer(t)
	if rec := as(t, s, aliceID, "DELETE", "/api/v1/sessions/s-shared", ""); rec.Code != http.StatusForbidden {
		t.Fatalf("member kill: %d want 403", rec.Code)
	}
	if rec := as(t, s, carolID, "DELETE", "/api/v1/sessions/s-shared", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("outsider kill: %d want 404", rec.Code)
	}
	if rec := as(t, s, bobID, "DELETE", "/api/v1/sessions/s-shared", ""); rec.Code != http.StatusOK {
		t.Fatalf("owner kill: %d %s", rec.Code, rec.Body.String())
	}
}

// Adding a member is how a user gets the link: after the owner adds carol, the
// session (and its join link) shows up in carol's own listing.
func TestAddMemberGrantsAccess(t *testing.T) {
	s := aclServer(t)
	if rec := as(t, s, aliceID, "POST", "/api/v1/sessions/s-shared/members", `{"user":"carol@example.com"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("member adding a member: %d want 403", rec.Code)
	}
	rec := as(t, s, bobID, "POST", "/api/v1/sessions/s-shared/members", `{"user":"carol@example.com"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("owner add: %d %s", rec.Code, rec.Body.String())
	}
	if got := listNames(t, as(t, s, carolID, "GET", "/api/v1/sessions", "")); len(got) != 1 || got[0] != "s-shared" {
		t.Fatalf("carol sees %v after being added", got)
	}
	if rec := as(t, s, bobID, "POST", "/api/v1/sessions/s-shared/members", `{"user":"../etc/passwd"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("junk handle: %d want 400", rec.Code)
	}

	del := as(t, s, bobID, "DELETE", "/api/v1/sessions/s-shared/members/carol%40example.com", "")
	if del.Code != http.StatusOK {
		t.Fatalf("remove: %d %s", del.Code, del.Body.String())
	}
	if got := listNames(t, as(t, s, carolID, "GET", "/api/v1/sessions", "")); len(got) != 0 {
		t.Fatalf("carol still sees %v after removal", got)
	}
}

// An admin adds people from the directory of everyone who has logged in; a
// regular user has no business enumerating them and adds by address instead.
func TestUsersDirectoryIsAdminOnly(t *testing.T) {
	s := aclServer(t)
	if rec := as(t, s, bobID, "GET", "/api/v1/users", ""); rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin directory: %d want 403", rec.Code)
	}
	if _, err := s.cfg.Access.AddMember("s-bob", "dave@example.com"); err != nil {
		t.Fatal(err)
	}
	rec := as(t, s, bossID, "GET", "/api/v1/users", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("admin directory: %d %s", rec.Code, rec.Body.String())
	}
	var users []userView
	if err := json.Unmarshal(rec.Body.Bytes(), &users); err != nil {
		t.Fatal(err)
	}
	byKey := map[string]userView{}
	for _, u := range users {
		byKey[u.Key] = u
	}
	if u := byKey["alice@example.com"]; u.Label != "Alice" || u.Invited || u.Admin {
		t.Fatalf("alice entry wrong: %+v", u)
	}
	if u := byKey["boss@example.com"]; !u.Admin {
		t.Fatalf("configured admin not flagged: %+v", u)
	}
	if u := byKey["dave@example.com"]; !u.Invited {
		t.Fatalf("invited user not flagged: %+v", u)
	}
}

// A session created through the web UI belongs to its creator: they must see it
// without an admin sharing it back to them.
func TestCreateRecordsOwner(t *testing.T) {
	acl, _ := access.Open("")
	store, err := spool.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(Config{
		Store:         store,
		Access:        acl,
		Auth:          testAuth(OIDCConfig{Admins: []string{"lassulus"}}),
		WorkspaceRoot: t.TempDir(),
		Create:        func(CreateSpec) error { return nil },
	})
	rec := as(t, s, aliceID, "POST", "/api/v1/sessions", `{"name":"proj"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	var out struct{ ID string }
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if got := acl.ACL(out.ID).Owner; got != "alice@example.com" {
		t.Fatalf("owner=%q want alice@example.com", got)
	}
	if !acl.ACL(out.ID).Allows(aliceID.principals()) {
		t.Fatal("creator cannot see own session")
	}
}

// Logging in is the only way into the directory, so the authenticator must feed
// it: NewServer wires that hook.
func TestLoginPopulatesDirectory(t *testing.T) {
	acl, _ := access.Open("")
	s := NewServer(Config{Access: acl, Auth: testAuth(OIDCConfig{})})
	if s.cfg.Auth.OnLogin == nil {
		t.Fatal("login hook not wired")
	}
	s.cfg.Auth.OnLogin(aliceID)
	u, ok := acl.Lookup("alice@example.com")
	if !ok || u.Sub != "sub-alice" || u.Username != "alice" || u.LastSeen.IsZero() {
		t.Fatalf("directory entry: %+v ok=%v", u, ok)
	}
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}
