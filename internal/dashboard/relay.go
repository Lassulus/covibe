package dashboard

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/lassulus/covibe/internal/spool"
)

// Relay is covibe's own collab hub: one room per live session. The omp plugin
// connects as the single host and streams normalized events; browser viewers
// connect as guests, receive a replay of recent events, then the live stream.
// Guests may send prompt/abort frames, which are forwarded to the host.
//
// There is no end-to-end crypto here (unlike omp's public relay): the trust
// boundary is TLS + OIDC. The relay lives in the dashboard process and shares
// its auth; the host endpoint is gated by a per-session token.
type Relay struct {
	mu    sync.Mutex
	rooms map[string]*room
	store *spool.Store
}

func newRelay(store *spool.Store) *Relay {
	return &Relay{rooms: map[string]*room{}, store: store}
}

const (
	relayRingFrames = 2000
	relayRingBytes  = 1 << 20 // 1 MiB replay budget per room
	relaySendBuffer = 512
	relayWriteWait  = 10 * time.Second
)

// conn is one websocket peer (host or guest) with a buffered writer so a slow
// peer never blocks the hub.
type conn struct {
	ws   *websocket.Conn
	send chan []byte
}

func newConn(ws *websocket.Conn) *conn {
	return &conn{ws: ws, send: make(chan []byte, relaySendBuffer)}
}

// enqueue queues a frame without blocking; false means the peer is too slow.
func (c *conn) enqueue(msg []byte) bool {
	select {
	case c.send <- msg:
		return true
	default:
		return false
	}
}

func (c *conn) writeLoop(ctx context.Context) {
	for msg := range c.send {
		wctx, cancel := context.WithTimeout(ctx, relayWriteWait)
		err := c.ws.Write(wctx, websocket.MessageText, msg)
		cancel()
		if err != nil {
			return
		}
	}
}

type ringItem struct {
	raw []byte // a complete server->guest {"t":"ev",...} frame
}

type room struct {
	id string

	mu     sync.Mutex
	seq    int
	ring   []ringItem
	bytes  int
	guests map[*conn]struct{}
	host   *conn
	name   string
	dir    string
	status string
}

func (rl *Relay) room(id string) *room {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	rm := rl.rooms[id]
	if rm == nil {
		rm = &room{id: id, guests: map[*conn]struct{}{}, status: spool.StatusStarting}
		rl.rooms[id] = rm
	}
	return rm
}

func (rl *Relay) dropIfEmpty(rm *room) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	rm.mu.Lock()
	empty := rm.host == nil && len(rm.guests) == 0
	rm.mu.Unlock()
	if empty && rl.rooms[rm.id] == rm {
		delete(rl.rooms, rm.id)
	}
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

func (rm *room) metaFrame() []byte {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	return mustJSON(map[string]any{
		"t": "meta", "name": rm.name, "dir": rm.dir, "status": rm.status, "guests": len(rm.guests),
	})
}

func (rm *room) broadcastToGuests(frame []byte) {
	rm.mu.Lock()
	gs := make([]*conn, 0, len(rm.guests))
	for g := range rm.guests {
		gs = append(gs, g)
	}
	rm.mu.Unlock()
	for _, g := range gs {
		if !g.enqueue(frame) {
			_ = g.ws.Close(websocket.StatusPolicyViolation, "slow consumer")
		}
	}
}

// broadcastEv assigns a seq, buffers the ev in the ring, and fans it out.
func (rm *room) broadcastEv(ev json.RawMessage) {
	rm.mu.Lock()
	rm.seq++
	frame := mustJSON(struct {
		T   string          `json:"t"`
		Seq int             `json:"seq"`
		E   json.RawMessage `json:"e"`
	}{"ev", rm.seq, ev})
	rm.ring = append(rm.ring, ringItem{raw: frame})
	rm.bytes += len(frame)
	for len(rm.ring) > 1 && (len(rm.ring) > relayRingFrames || rm.bytes > relayRingBytes) {
		rm.bytes -= len(rm.ring[0].raw)
		rm.ring = rm.ring[1:]
	}
	gs := make([]*conn, 0, len(rm.guests))
	for g := range rm.guests {
		gs = append(gs, g)
	}
	rm.mu.Unlock()
	for _, g := range gs {
		if !g.enqueue(frame) {
			_ = g.ws.Close(websocket.StatusPolicyViolation, "slow consumer")
		}
	}
}

// addGuest registers a guest and returns the replay frames (meta + ring),
// snapshotted under the lock so it interleaves cleanly with live broadcasts.
func (rm *room) addGuest(c *conn) [][]byte {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	rm.guests[c] = struct{}{}
	out := make([][]byte, 0, len(rm.ring)+1)
	out = append(out, mustJSON(map[string]any{
		"t": "meta", "name": rm.name, "dir": rm.dir, "status": rm.status, "guests": len(rm.guests),
	}))
	for _, it := range rm.ring {
		out = append(out, it.raw)
	}
	return out
}

// markStatus best-effort updates the spool record's status (e.g. -> live).
func (rl *Relay) markStatus(id, status string) {
	rec, err := rl.store.Load(id)
	if err != nil || rec.Status == status {
		return
	}
	rec.Status = status
	_ = rl.store.Save(rec)
}

// ServeHost upgrades the omp plugin's host connection. Gated by a per-session
// token (the plugin has no cookie); accepts any Origin since it is not a
// browser and carries no ambient credentials.
func (rl *Relay) ServeHost(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	token := r.URL.Query().Get("token")
	rec, err := rl.store.Load(id)
	if err != nil || rec.HostToken == "" ||
		subtle.ConstantTimeCompare([]byte(token), []byte(rec.HostToken)) != 1 {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	ws.SetReadLimit(8 << 20)
	c := newConn(ws)
	rm := rl.room(id)

	rm.mu.Lock()
	old := rm.host
	rm.host = c
	rm.status = spool.StatusLive
	if rm.name == "" {
		rm.name = rec.Name
	}
	if rm.dir == "" {
		rm.dir = rec.Dir
	}
	rm.mu.Unlock()
	if old != nil {
		_ = old.ws.Close(websocket.StatusNormalClosure, "replaced by new host")
	}
	rl.markStatus(id, spool.StatusLive)
	rm.broadcastToGuests(rm.metaFrame())

	ctx := context.Background()
	go c.writeLoop(ctx)

	for {
		typ, data, err := ws.Read(ctx)
		if err != nil {
			break
		}
		if typ != websocket.MessageText {
			continue
		}
		var f struct {
			T    string          `json:"t"`
			Name string          `json:"name"`
			Dir  string          `json:"dir"`
			E    json.RawMessage `json:"e"`
		}
		if json.Unmarshal(data, &f) != nil {
			continue
		}
		switch f.T {
		case "hello":
			rm.mu.Lock()
			if f.Name != "" {
				rm.name = f.Name
			}
			if f.Dir != "" {
				rm.dir = f.Dir
			}
			rm.mu.Unlock()
			rm.broadcastToGuests(rm.metaFrame())
		case "ev":
			if len(f.E) > 0 {
				rm.broadcastEv(f.E)
			}
		}
	}

	close(c.send)
	rm.mu.Lock()
	if rm.host == c {
		rm.host = nil
		rm.status = spool.StatusEnded
	}
	rm.mu.Unlock()
	rm.broadcastToGuests(mustJSON(map[string]any{"t": "end", "reason": "host disconnected"}))
	rm.broadcastToGuests(rm.metaFrame())
	rl.dropIfEmpty(rm)
}

// ServeGuest upgrades a browser viewer. Auth (OIDC session / API key) is applied
// by the caller wrapper; the default same-origin Origin check still applies.
func (rl *Relay) ServeGuest(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	ws.SetReadLimit(1 << 20)
	c := newConn(ws)
	rm := rl.room(id)
	replay := rm.addGuest(c)
	rm.broadcastToGuests(rm.metaFrame())

	ctx := context.Background()
	// Write the replay synchronously (it can exceed the send buffer), then hand
	// off to the buffered writer for the live stream.
	for _, m := range replay {
		wctx, cancel := context.WithTimeout(ctx, relayWriteWait)
		werr := ws.Write(wctx, websocket.MessageText, m)
		cancel()
		if werr != nil {
			_ = ws.Close(websocket.StatusPolicyViolation, "slow consumer")
			rm.mu.Lock()
			delete(rm.guests, c)
			rm.mu.Unlock()
			rl.dropIfEmpty(rm)
			return
		}
	}
	go c.writeLoop(ctx)

	for {
		typ, data, err := ws.Read(ctx)
		if err != nil {
			break
		}
		if typ != websocket.MessageText {
			continue
		}
		var f struct {
			T    string `json:"t"`
			Text string `json:"text"`
		}
		if json.Unmarshal(data, &f) != nil {
			continue
		}
		var out []byte
		switch f.T {
		case "prompt":
			if f.Text == "" {
				continue
			}
			out = mustJSON(map[string]any{"t": "prompt", "text": f.Text})
		case "abort":
			out = mustJSON(map[string]any{"t": "abort"})
		default:
			continue
		}
		rm.mu.Lock()
		host := rm.host
		rm.mu.Unlock()
		if host != nil {
			host.enqueue(out)
		}
	}

	close(c.send)
	rm.mu.Lock()
	delete(rm.guests, c)
	rm.mu.Unlock()
	rm.broadcastToGuests(rm.metaFrame())
	rl.dropIfEmpty(rm)
}
