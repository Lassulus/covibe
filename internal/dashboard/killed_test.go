package dashboard

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/lassulus/covibe/internal/spool"
)

// registerRemote announces a remote session the way a wrapper does and returns
// its id.
func registerRemote(t *testing.T, h http.Handler, name, room string) string {
	t.Helper()
	body := `{"name":"` + name + `","dir":"/tmp/` + name + `","roomId":"` + room + `"}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v1/sessions/register", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("register: got %d want 201; body=%s", rec.Code, rec.Body.String())
	}
	var out struct{ ID string }
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("register reply: %v", err)
	}
	if out.ID == "" {
		t.Fatal("register returned no id")
	}
	return out.ID
}

// A kill must stick even if the wrapper only checks in after the ended record
// has been pruned: otherwise it reads the 404 as "the dashboard forgot me",
// re-registers under a fresh id, and the session outlives the kill.
func TestKillSticksAfterRecordPruned(t *testing.T) {
	store, err := spool.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	keys, _ := LoadAPIKeys("k")
	s := NewServer(Config{
		Store: store, Auth: testAuth(OIDCConfig{NoAuth: true}), APIKeys: keys,
		KeepEnded: time.Nanosecond, // prune the tombstone record immediately
	})
	h := s.Handler()

	const room = "KilledRoom0123456789"
	id := registerRemote(t, h, "killme", room)

	// Kill it, then let the ended record be pruned (KeepEnded is ~0 and any
	// listing prunes), exactly as happens while the wrapper is between beats.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("DELETE", "/api/v1/sessions/"+id, nil)
	req.Header.Set("X-API-Key", "k")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("kill: got %d want 200", rec.Code)
	}
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/api/v1/sessions", nil)
	req.Header.Set("X-API-Key", "k")
	h.ServeHTTP(rec, req)
	if body := rec.Body.String(); body != "[]\n" && body != "null\n" {
		t.Fatalf("killed session should be gone from the listing, got %s", body)
	}

	// The late heartbeat must be told to stop, not 404'd.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/api/v1/sessions/"+id+"/pane", bytes.NewBufferString("")))
	if rec.Code != http.StatusOK {
		t.Fatalf("late heartbeat: got %d want 200", rec.Code)
	}
	var hb struct{ Stop bool }
	if err := json.Unmarshal(rec.Body.Bytes(), &hb); err != nil {
		t.Fatalf("heartbeat reply: %v", err)
	}
	if !hb.Stop {
		t.Fatalf("late heartbeat must report stop, got %s", rec.Body.String())
	}

	// And a re-register on the same room must be refused rather than resurrecting.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/api/v1/sessions/register",
		bytes.NewBufferString(`{"name":"killme","dir":"/tmp/killme","roomId":"`+room+`"}`))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusGone {
		t.Fatalf("re-register of a killed room: got %d want 410", rec.Code)
	}
}

// An unrelated unknown id is still a plain 404, so a wrapper whose record was
// pruned for going quiet keeps its re-register path.
func TestUnknownHeartbeatStill404(t *testing.T) {
	store, err := spool.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(Config{Store: store, Auth: testAuth(OIDCConfig{NoAuth: true})})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/api/v1/sessions/covibe-nope/pane", bytes.NewBufferString("")))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown id: got %d want 404", rec.Code)
	}
}
