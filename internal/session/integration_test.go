package session

import (
	"context"
	"net/http/httptest"
	"os"
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

	// Heartbeat with a pane snapshot: not stopped, and the pane is stored.
	stop, err := sink.heartbeat(context.Background(), rec.ID, []byte("live-pane"))
	if err != nil || stop {
		t.Fatalf("heartbeat: stop=%v err=%v", stop, err)
	}
	pane, err := os.ReadFile(store.PaneFilePath(rec.ID))
	if err != nil || string(pane) != "live-pane" {
		t.Fatalf("pane file = %q err=%v", pane, err)
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
