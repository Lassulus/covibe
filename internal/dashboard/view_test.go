package dashboard

import (
	"testing"

	"github.com/lassulus/covibe/internal/spool"
)

// A session is only joinable while its host is connected to the relay: the
// relay answers a guest for a hostless room with "session ended: no such room".
// The view must therefore withhold the links/QR until a host is present, so the
// dashboard never hands out a dead link (remote sessions can heartbeat happily
// while their omp host is gone).
func TestViewOfPublishesLinksOnlyWithHostOnRelay(t *testing.T) {
	s := NewServer(Config{Auth: testAuth(OIDCConfig{NoAuth: true})})
	rec := spool.Record{
		ID:         "s1",
		Name:       "demo",
		Status:     spool.StatusStarting,
		RoomID:     "room1",
		JoinLink:   "covibe.example/r/room1.secret",
		BrowserURL: "https://covibe.example/c/#covibe.example/r/room1.secret",
	}

	v := s.viewOf(rec)
	if v.JoinLink != "" || v.BrowserURL != "" {
		t.Fatalf("hostless room advertised: join=%q browser=%q", v.JoinLink, v.BrowserURL)
	}
	if v.Status == spool.StatusLive {
		t.Fatalf("status=%q must not be live without a host", v.Status)
	}
	if v.Room != rec.RoomID {
		t.Fatalf("room id should still be reported, got %q", v.Room)
	}

	// Host connects: the room exists on the relay, so the links become real.
	s.relay.rooms[rec.RoomID] = &relayRoom{}
	v = s.viewOf(rec)
	if v.JoinLink != rec.JoinLink || v.BrowserURL != rec.BrowserURL {
		t.Fatalf("live room not advertised: join=%q browser=%q", v.JoinLink, v.BrowserURL)
	}
	if v.Status != spool.StatusLive {
		t.Fatalf("status=%q want %q", v.Status, spool.StatusLive)
	}
}

// An ended session never advertises links, even if its room somehow lingers.
func TestViewOfEndedSessionNeverAdvertises(t *testing.T) {
	s := NewServer(Config{Auth: testAuth(OIDCConfig{NoAuth: true})})
	rec := spool.Record{
		ID:         "s2",
		Status:     spool.StatusEnded,
		RoomID:     "room2",
		JoinLink:   "covibe.example/r/room2.secret",
		BrowserURL: "https://covibe.example/c/#covibe.example/r/room2.secret",
	}
	s.relay.rooms[rec.RoomID] = &relayRoom{}
	v := s.viewOf(rec)
	if v.JoinLink != "" || v.BrowserURL != "" {
		t.Fatalf("ended session advertised: join=%q browser=%q", v.JoinLink, v.BrowserURL)
	}
	if v.Status != spool.StatusEnded {
		t.Fatalf("status=%q want %q", v.Status, spool.StatusEnded)
	}
}
