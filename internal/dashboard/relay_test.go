package dashboard

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func newTestRelay(interval, timeout time.Duration) *Relay {
	rl := newRelay()
	rl.pingInterval, rl.pingTimeout = interval, timeout
	return rl
}

func dialRole(ctx context.Context, t *testing.T, srvURL, roomID, role string) *websocket.Conn {
	t.Helper()
	wsURL := "ws" + strings.TrimPrefix(srvURL, "http") + "/r/" + roomID + "?role=" + role
	c, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial %s: %v", role, err)
	}
	return c
}

// A peer that keeps reading auto-responds to the relay's pings, so its room must
// stay live across several ping intervals rather than being torn down.
func TestRelayKeepAliveKeepsHealthyHost(t *testing.T) {
	rl := newTestRelay(20*time.Millisecond, 100*time.Millisecond)
	srv := httptest.NewServer(http.HandlerFunc(rl.ServeRelay))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	const room = "keepalivehost"
	c := dialRole(ctx, t, srv.URL, room, "host")
	defer c.CloseNow()

	readErr := make(chan error, 1)
	go func() {
		for {
			if _, _, err := c.Read(ctx); err != nil {
				readErr <- err
				return
			}
		}
	}()

	select {
	case err := <-readErr:
		t.Fatalf("healthy host disconnected across ping cycles: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	if !rl.roomLive(room) {
		t.Fatal("healthy host room should still be live")
	}
}

// A peer that never reads cannot auto-respond to pings (models a half-open/stuck
// connection behind a silent proxy). The relay's ping must time out and tear the
// connection down, freeing the room so a reconnect is not rejected with 4009.
func TestRelayKeepAliveDropsUnresponsiveHost(t *testing.T) {
	rl := newTestRelay(20*time.Millisecond, 60*time.Millisecond)
	srv := httptest.NewServer(http.HandlerFunc(rl.ServeRelay))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	const room = "unresponsivehost"
	c := dialRole(ctx, t, srv.URL, room, "host")
	defer c.CloseNow()

	deadline := time.Now().Add(3 * time.Second)
	for rl.roomLive(room) {
		if time.Now().After(deadline) {
			t.Fatal("unresponsive host room was not freed by keepalive")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
