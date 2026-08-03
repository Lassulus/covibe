package session

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/lassulus/covibe/internal/dashboard"
	"github.com/lassulus/covibe/internal/spool"
)

// TestRemoteSinkAgainstDashboard drives the real RemoteSink client against a
// real dashboard HTTP server: register, heartbeat + pane push, and the stop
// signal after a kill — the full remote contract over HTTP.
func TestRemoteSinkAgainstDashboard(t *testing.T) {
	store, err := spool.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	auth, err := dashboard.NewAuthenticator(context.Background(), dashboard.OIDCConfig{
		NoAuth:       true,
		CookieSecret: []byte("0123456789abcdef0123456789abcdef"),
	})
	if err != nil {
		t.Fatalf("auth: %v", err)
	}
	srv := httptest.NewServer(dashboard.NewServer(dashboard.Config{
		Auth:  auth,
		Store: store,
	}).Handler())
	defer srv.Close()

	sink := NewRemoteSink(srv.URL)
	rec := &spool.Record{Name: "itest", RoomID: "abcdefghij123"}
	if err := sink.Register(rec); err != nil {
		t.Fatalf("register: %v", err)
	}
	if rec.ID == "" || !rec.Remote || rec.PID != 0 {
		t.Fatalf("register mutated record wrong: %+v", rec)
	}
	got, err := store.Load(rec.ID)
	if err != nil || !got.Remote || got.Name != "itest" {
		t.Fatalf("server record: %+v err=%v", got, err)
	}

	// A heartbeat keeps the announcement alive and is not told to stop.
	stop, err := sink.heartbeat(context.Background(), rec.ID, nil)
	if err != nil || stop {
		t.Fatalf("heartbeat: stop=%v err=%v", stop, err)
	}

	// After the session is ended (dashboard kill), the next heartbeat is told
	// to stop.
	ended, _ := store.Load(rec.ID)
	ended.Status = spool.StatusEnded
	if err := store.Save(ended); err != nil {
		t.Fatalf("save ended: %v", err)
	}
	stop, err = sink.heartbeat(context.Background(), rec.ID, nil)
	if err != nil || !stop {
		t.Fatalf("post-kill heartbeat: stop=%v err=%v", stop, err)
	}
}

// TestRemoteSinkReregistersAfterDashboardForgets covers recovery after the
// dashboard drops the record (restart wiped the spool, or GC pruned it while the
// dashboard was down for a redeploy): the heartbeat comes back as errSessionGone
// and the wrapper re-announces to get a fresh id, exactly what the Watch loop
// does so the session reappears in the overview instead of vanishing forever.
func TestRemoteSinkReregistersAfterDashboardForgets(t *testing.T) {
	store, err := spool.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	auth, err := dashboard.NewAuthenticator(context.Background(), dashboard.OIDCConfig{
		NoAuth:       true,
		CookieSecret: []byte("0123456789abcdef0123456789abcdef"),
	})
	if err != nil {
		t.Fatalf("auth: %v", err)
	}
	srv := httptest.NewServer(dashboard.NewServer(dashboard.Config{
		Auth:  auth,
		Store: store,
	}).Handler())
	defer srv.Close()

	sink := NewRemoteSink(srv.URL)
	rec := &spool.Record{Name: "itest", RoomID: "abcdefghij123"}
	if err := sink.Register(rec); err != nil {
		t.Fatalf("register: %v", err)
	}
	oldID := rec.ID

	// Dashboard forgets the session (redeploy wiped the spool / GC pruned it).
	if err := store.Remove(oldID); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := sink.heartbeat(context.Background(), oldID, nil); !errors.Is(err, errSessionGone) {
		t.Fatalf("heartbeat after forget = %v, want errSessionGone", err)
	}

	// Recovery: re-register adopts a fresh id and the session is live again.
	if err := sink.Register(rec); err != nil {
		t.Fatalf("re-register: %v", err)
	}
	if rec.ID == "" || rec.ID == oldID {
		t.Fatalf("re-register kept stale id %q -> %q", oldID, rec.ID)
	}
	stop, err := sink.heartbeat(context.Background(), rec.ID, []byte("live-again"))
	if err != nil || stop {
		t.Fatalf("heartbeat after re-register: stop=%v err=%v", stop, err)
	}
}
