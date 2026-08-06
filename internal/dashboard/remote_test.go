package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lassulus/covibe/internal/spool"
)

func newRemoteTestServer(t *testing.T) (*Server, *spool.Store) {
	t.Helper()
	store, err := spool.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return NewServer(Config{Auth: testAuth(OIDCConfig{NoAuth: true}), Store: store}), store
}

func TestRemoteSessionRoundtrip(t *testing.T) {
	s, store := newRemoteTestServer(t)

	body := `{"name":"remote-demo","dir":"/home/x/proj","host":"laptop","roomId":"abcdefghij123",` +
		`"browserUrl":"https://covibe.lassul.us/c/#covibe.lassul.us/r/abcdefghij123.deadbeef"}`
	rec := httptest.NewRecorder()
	s.handleRegister(rec, httptest.NewRequest("POST", "/api/v1/sessions/register", strings.NewReader(body)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("register got %d want 201; body=%s", rec.Code, rec.Body.String())
	}
	var reg struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &reg); err != nil || reg.ID == "" {
		t.Fatalf("register response: %v %s", err, rec.Body.String())
	}
	id := reg.ID

	got, err := store.Load(id)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !got.Remote || got.PID != 0 || got.Name != "remote-demo" || got.Host != "laptop" || got.RoomID != "abcdefghij123" {
		t.Fatalf("bad record: %+v", got)
	}

	// Push a pane snapshot (heartbeat + preview) and read it back.
	const pane = "hello from the remote pane"
	pr := httptest.NewRecorder()
	preq := httptest.NewRequest("POST", "/api/v1/sessions/"+id+"/pane", strings.NewReader(pane))
	preq.SetPathValue("id", id)
	s.handleRemotePane(pr, preq)
	if pr.Code != http.StatusOK || strings.Contains(pr.Body.String(), `"stop":true`) {
		t.Fatalf("pane push: code=%d body=%s", pr.Code, pr.Body.String())
	}
	// Kill from the dashboard: marks ended, never signals a (remote) pid.
	kr := httptest.NewRecorder()
	kreq := httptest.NewRequest("DELETE", "/api/v1/sessions/"+id, nil)
	kreq.SetPathValue("id", id)
	s.handleKill(kr, kreq)
	if kr.Code != http.StatusOK {
		t.Fatalf("kill got %d", kr.Code)
	}
	after, err := store.Load(id)
	if err != nil {
		t.Fatalf("load after kill: %v", err)
	}
	if after.Status != spool.StatusEnded {
		t.Fatalf("status after kill = %q want ended", after.Status)
	}

	// A heartbeat after the kill tells the wrapper to stop omp.
	sr := httptest.NewRecorder()
	sreq := httptest.NewRequest("POST", "/api/v1/sessions/"+id+"/pane", strings.NewReader(""))
	sreq.SetPathValue("id", id)
	s.handleRemotePane(sr, sreq)
	if !strings.Contains(sr.Body.String(), `"stop":true`) {
		t.Fatalf("ended session should report stop:true; got %s", sr.Body.String())
	}
}

func TestRegisterRejectsBadName(t *testing.T) {
	s, _ := newRemoteTestServer(t)
	rec := httptest.NewRecorder()
	s.handleRegister(rec, httptest.NewRequest("POST", "/api/v1/sessions/register", strings.NewReader(`{"name":"../etc"}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad name got %d want 400", rec.Code)
	}
}

// registerRoom announces a remote session hosting a specific room.
func registerRoom(t *testing.T, s *Server, token, name, room string) string {
	t.Helper()
	body := `{"name":"` + name + `","roomId":"` + room + `"}`
	r := httptest.NewRequest("POST", "/api/v1/sessions/register", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, r)
	if rec.Code != http.StatusCreated {
		t.Fatalf("register %s: %d %s", name, rec.Code, rec.Body.String())
	}
	var out struct{ ID string }
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out.ID
}

// A session that comes back after downtime must come back *shared*. The wrapper
// re-announces whenever the dashboard forgets it — a suspended laptop's record is
// pruned all night — and the owner and member list are keyed by session id, so a
// freshly minted id would hand the session back to nobody. Deriving the id from
// the room is what keeps them attached.
func TestReregisterKeepsOwnerAndMembers(t *testing.T) {
	s := keyedServer(t)
	const room = "roomkeepsacl123"

	// Keyless announcement: ownerless, then shared by an admin (the CLI case).
	first := registerRoom(t, s, "", "overnight", room)
	if err := s.cfg.Access.SetOwner(first, "alice@example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.cfg.Access.AddMember(first, bobID.Email); err != nil {
		t.Fatal(err)
	}

	// The laptop sleeps: the heartbeat stops and GC drops the record.
	if err := s.cfg.Store.Remove(first); err != nil {
		t.Fatal(err)
	}

	// Morning: the wrapper re-announces the same room.
	second := registerRoom(t, s, "", "overnight", room)
	if second != first {
		t.Fatalf("re-register minted %q, want the room's stable id %q", second, first)
	}
	acl := s.cfg.Access.ACL(second)
	if acl.Owner != "alice@example.com" {
		t.Fatalf("owner = %q, want alice@example.com", acl.Owner)
	}
	if !acl.Allows([]string{bobID.Email}) {
		t.Fatalf("member list lost across re-registration: %+v", acl.Members)
	}
	// The observable effect: bob still finds it on his own dashboard.
	if got := listNames(t, as(t, s, bobID, "GET", "/api/v1/sessions", "")); !contains(got, second) {
		t.Fatalf("bob lost sight of the session after re-registration: %v", got)
	}

	// A different room is a different session, not a collision.
	if other := registerRoom(t, s, "", "elsewhere", "roomotherxyz9"); other == first {
		t.Fatalf("distinct rooms collided on id %q", other)
	}
}
