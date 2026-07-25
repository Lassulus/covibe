package dashboard

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"regexp"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// Relay is covibe's self-hosted omp collab relay: a content-blind WebSocket hub
// that speaks the exact contract omp's host, `omp join` guests, and collab-web
// expect (see omp packages/collab-web/scripts/local-relay.ts).
//
//   - GET /r/<roomId>?role=host|guest upgrades to a WebSocket.
//   - The host creates the room; a second host is rejected (close 4009); a guest
//     joining a missing room is rejected (close 4004).
//   - Binary frames carry a 4-byte big-endian peerId envelope + sealed payload.
//     Host peerId 0 broadcasts to every guest; peerId N targets guest N. Guest
//     frames have their first 4 bytes rewritten to the sender's id, then go to
//     the host.
//   - TEXT control to the host: {"t":"peer-joined","peer":N}/{"t":"peer-left"}.
//   - Host disconnect: TEXT {"t":"room-closed"} to guests, then close 4001.
//
// Payloads are sealed end to end; the relay never sees plaintext. Possession of
// the room key (carried in the link fragment, never sent here) is the trust
// boundary — so /r/ is intentionally unauthenticated, like the public relay.
type Relay struct {
	mu    sync.Mutex
	rooms map[string]*relayRoom
	// Keepalive: omp and the coder/websocket lib send no application-level
	// pings, so an idle collab connection behind a reverse proxy (nginx's
	// default proxy_read_timeout is 60s) gets severed, triggering omp's
	// reconnect loop. Pinging every peer keeps intermediaries warm and detects
	// dead/half-open peers so their room is freed promptly (a stale host
	// otherwise blocks reconnect with close 4009). Overridable in tests.
	pingInterval time.Duration
	pingTimeout  time.Duration
}

func newRelay() *Relay {
	return &Relay{
		rooms:        map[string]*relayRoom{},
		pingInterval: 30 * time.Second,
		pingTimeout:  10 * time.Second,
	}
}

const relayEnvHeader = 4

var relayRoomRe = regexp.MustCompile(`^/r/([A-Za-z0-9_-]{10,64})$`)

type relayFrame struct {
	typ  websocket.MessageType
	data []byte
}

type relayConn struct {
	ws     *websocket.Conn
	send   chan relayFrame
	peerID uint32
}

func newRelayConn(ws *websocket.Conn) *relayConn {
	return &relayConn{ws: ws, send: make(chan relayFrame, 256)}
}

func (c *relayConn) enqueue(f relayFrame) {
	select {
	case c.send <- f:
	default:
		_ = c.ws.Close(websocket.StatusPolicyViolation, "slow consumer")
	}
}

func (c *relayConn) writeLoop(ctx context.Context) {
	for f := range c.send {
		wctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		err := c.ws.Write(wctx, f.typ, f.data)
		cancel()
		if err != nil {
			return
		}
	}
}

// keepAlive pings the peer on a fixed interval, tearing the connection down
// when a ping fails (write error or no pong within the timeout). CloseNow makes
// the blocked Read in the serve loop return, running its cleanup. Ping is safe
// concurrently with the in-flight Read/Write (see coder/websocket docs).
func (c *relayConn) keepAlive(ctx context.Context, interval, timeout time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			pctx, cancel := context.WithTimeout(ctx, timeout)
			err := c.ws.Ping(pctx)
			cancel()
			if err != nil {
				_ = c.ws.CloseNow()
				return
			}
		}
	}
}

type relayRoom struct {
	mu     sync.Mutex
	host   *relayConn
	guests map[uint32]*relayConn
	next   uint32
}

func ctrlFrame(t string, peer uint32) relayFrame {
	b, _ := json.Marshal(map[string]any{"t": t, "peer": peer})
	return relayFrame{typ: websocket.MessageText, data: b}
}

func ctrlFrameNoPeer(t string) relayFrame {
	b, _ := json.Marshal(map[string]any{"t": t})
	return relayFrame{typ: websocket.MessageText, data: b}
}

// ServeRelay handles GET /r/<roomId>?role=host|guest.
func (rl *Relay) ServeRelay(w http.ResponseWriter, r *http.Request) {
	m := relayRoomRe.FindStringSubmatch(r.URL.Path)
	role := r.URL.Query().Get("role")
	if m == nil || (role != "host" && role != "guest") {
		http.NotFound(w, r)
		return
	}
	roomID := m[1]
	// Content-blind + credential-free: the link key is the trust boundary, and
	// neither omp hosts nor browser guests present a same-origin/cookie the
	// default check could use.
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	ws.SetReadLimit(32 << 20)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := newRelayConn(ws)

	if role == "host" {
		rl.serveHost(ctx, roomID, c)
	} else {
		rl.serveGuest(ctx, roomID, c)
	}
}

func (rl *Relay) serveHost(ctx context.Context, roomID string, c *relayConn) {
	rl.mu.Lock()
	if _, exists := rl.rooms[roomID]; exists {
		rl.mu.Unlock()
		_ = c.ws.Close(4009, "a host is already connected for this room")
		return
	}
	room := &relayRoom{host: c, guests: map[uint32]*relayConn{}, next: 1}
	rl.rooms[roomID] = room
	rl.mu.Unlock()

	go c.writeLoop(ctx)
	go c.keepAlive(ctx, rl.pingInterval, rl.pingTimeout)
	defer func() {
		rl.mu.Lock()
		if rl.rooms[roomID] == room {
			delete(rl.rooms, roomID)
		}
		rl.mu.Unlock()
		room.mu.Lock()
		guests := make([]*relayConn, 0, len(room.guests))
		for _, g := range room.guests {
			guests = append(guests, g)
		}
		room.guests = map[uint32]*relayConn{}
		room.mu.Unlock()
		for _, g := range guests {
			g.enqueue(ctrlFrameNoPeer("room-closed"))
			_ = g.ws.Close(4001, "room closed")
		}
		close(c.send)
	}()

	for {
		typ, data, err := c.ws.Read(ctx)
		if err != nil {
			return
		}
		if typ != websocket.MessageBinary || len(data) < relayEnvHeader {
			continue
		}
		peer := binary.BigEndian.Uint32(data[:relayEnvHeader])
		room.mu.Lock()
		if peer == 0 {
			for _, g := range room.guests {
				g.enqueue(relayFrame{typ: websocket.MessageBinary, data: data})
			}
		} else if g := room.guests[peer]; g != nil {
			g.enqueue(relayFrame{typ: websocket.MessageBinary, data: data})
		}
		room.mu.Unlock()
	}
}

func (rl *Relay) serveGuest(ctx context.Context, roomID string, c *relayConn) {
	rl.mu.Lock()
	room := rl.rooms[roomID]
	rl.mu.Unlock()
	if room == nil {
		_ = c.ws.Close(4004, "no such room")
		return
	}
	room.mu.Lock()
	peerID := room.next
	room.next++
	c.peerID = peerID
	room.guests[peerID] = c
	host := room.host
	room.mu.Unlock()

	go c.writeLoop(ctx)
	go c.keepAlive(ctx, rl.pingInterval, rl.pingTimeout)
	host.enqueue(ctrlFrame("peer-joined", peerID))
	defer func() {
		room.mu.Lock()
		_, stillHere := room.guests[peerID]
		delete(room.guests, peerID)
		h := room.host
		room.mu.Unlock()
		if stillHere && h != nil {
			h.enqueue(ctrlFrame("peer-left", peerID))
		}
		close(c.send)
	}()

	for {
		typ, data, err := c.ws.Read(ctx)
		if err != nil {
			return
		}
		if typ != websocket.MessageBinary || len(data) < relayEnvHeader {
			continue
		}
		binary.BigEndian.PutUint32(data[:relayEnvHeader], peerID)
		room.mu.Lock()
		h := room.host
		room.mu.Unlock()
		if h != nil {
			h.enqueue(relayFrame{typ: websocket.MessageBinary, data: data})
		}
	}
}

// roomLive reports whether a host is currently connected for roomID.
func (rl *Relay) roomLive(roomID string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	_, ok := rl.rooms[roomID]
	return ok
}
