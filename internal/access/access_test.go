package access

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Adding someone before their first login is the normal case (that is how they
// learn the session exists), so an invite must survive the login: the key the
// inviter typed stays the member key, and the entry stops being "invited".
func TestInviteIsUpgradedOnLogin(t *testing.T) {
	s, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddMember("s1", "Alice@Example.com"); err != nil {
		t.Fatal(err)
	}
	u, ok := s.Lookup("alice@example.com")
	if !ok || !u.Invited {
		t.Fatalf("invite not recorded: %+v ok=%v", u, ok)
	}

	got := s.Seen("sub-1", "alice@example.com", "Alice", "alice")
	if got.Key != "alice@example.com" {
		t.Fatalf("key changed on login: %q", got.Key)
	}
	if got.Invited {
		t.Fatal("still marked invited after login")
	}
	if !s.ACL("s1").Allows(Principals("sub-1", "alice@example.com", "alice")) {
		t.Fatal("membership lost across login")
	}
}

// An invite may be spelled as a username or a sub; the member list must still
// match the login, whatever claim the provider fills in.
func TestMembershipMatchesAnyPrincipal(t *testing.T) {
	s, _ := Open("")
	if _, err := s.AddMember("s1", "bob"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddMember("s2", "sub-xyz"); err != nil {
		t.Fatal(err)
	}
	bob := Principals("sub-bob", "bob@corp.example", "bob")
	if !s.ACL("s1").Allows(bob) {
		t.Fatal("username invite does not match login")
	}
	if !s.ACL("s2").Allows(Principals("sub-xyz", "x@corp.example", "")) {
		t.Fatal("sub invite does not match login")
	}
	if s.ACL("s1").Allows(Principals("sub-eve", "eve@corp.example", "eve")) {
		t.Fatal("unrelated user allowed")
	}
	// The upgraded entry keeps the invited key, so Users() lists one bob.
	s.Seen("sub-bob", "bob@corp.example", "Bob", "bob")
	if n := len(s.Users()); n != 2 { // bob + the sub-xyz invite
		t.Fatalf("directory has %d users, want 2: %+v", n, s.Users())
	}
}

// The owner is not duplicated into the member list, and a removed member loses
// access immediately.
func TestOwnerAndMemberBookkeeping(t *testing.T) {
	s, _ := Open("")
	if err := s.SetOwner("s1", "Owner@example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddMember("s1", "owner@example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddMember("s1", "guest@example.com"); err != nil {
		t.Fatal(err)
	}
	if got := s.ACL("s1").Members; len(got) != 1 || got[0] != "guest@example.com" {
		t.Fatalf("members=%v want [guest@example.com]", got)
	}
	if err := s.RemoveMember("s1", "guest@example.com"); err != nil {
		t.Fatal(err)
	}
	if s.ACL("s1").Allows(Principals("sub-g", "guest@example.com", "")) {
		t.Fatal("removed member still allowed")
	}
	if !s.ACL("s1").Allows(Principals("sub-o", "owner@example.com", "")) {
		t.Fatal("owner lost access")
	}
}

// State is the dashboard's memory of who may see what: it has to survive a
// restart, which is the whole reason it lives outside the tmpfs spool.
func TestPersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "access.json")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s.Seen("sub-1", "alice@example.com", "Alice", "alice")
	if err := s.SetOwner("s1", "alice@example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddMember("s1", "bob@example.com"); err != nil {
		t.Fatal(err)
	}

	again, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	acl := again.ACL("s1")
	if acl.Owner != "alice@example.com" || len(acl.Members) != 1 || acl.Members[0] != "bob@example.com" {
		t.Fatalf("acl not persisted: %+v", acl)
	}
	if u, ok := again.Lookup("alice@example.com"); !ok || u.Name != "Alice" || u.Invited {
		t.Fatalf("user not persisted: %+v ok=%v", u, ok)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("access file mode: %v %v", fi.Mode(), err)
	}
}

// Prune drops ACLs of sessions that are gone, keeps freshly created ones (their
// spool record may not exist yet) and never touches the user directory.
func TestPruneRespectsGraceAndKeepsUsers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "access.json")
	old := time.Now().Add(-time.Hour)
	seed := state{
		Users:    map[string]User{"alice@example.com": {Key: "alice@example.com", Email: "alice@example.com"}},
		Sessions: map[string]Session{"gone": {Owner: "alice@example.com", AddedAt: old}},
	}
	data, err := json.Marshal(seed)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetOwner("fresh", "alice@example.com"); err != nil {
		t.Fatal(err)
	}

	s.Prune(map[string]bool{}, 5*time.Minute)

	if got := s.ACL("gone"); got.Owner != "" {
		t.Fatalf("stale acl kept: %+v", got)
	}
	if got := s.ACL("fresh"); got.Owner != "alice@example.com" {
		t.Fatal("acl inside grace was pruned")
	}
	if _, ok := s.Lookup("alice@example.com"); !ok {
		t.Fatal("directory entry pruned")
	}
	// The prune is durable, not just in-memory.
	again, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := again.ACL("gone"); got.Owner != "" {
		t.Fatalf("prune not persisted: %+v", got)
	}
}
