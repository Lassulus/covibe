package session

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"os"
	"sync"

	"github.com/lassulus/covibe/internal/termwire"
)

// A session's terminal is also reachable peer to peer, without the dashboard in
// the data path. The wrapper serves the same protocol the browser terminal
// speaks (see the wire contract at the top of termhost.go) on two unix sockets,
// and a sidecar holding two iroh endpoints pumps bytes between those sockets and
// whoever dialled the matching ticket. The sidecar knows nothing about
// terminals: that is the point, because it means every terminal semantic stays
// here, in the code the test suite already covers.
//
// Read-only is a different keypair, not a flag on a shared one. A ticket is only
// an address, so two access modes sharing one endpoint would be two identical
// tickets — the holder of the read-only one would simply ask for the other
// protocol. Two endpoints means the read-only ticket cannot name the writable
// socket at all. The sidecar additionally never forwards a peer's bytes into the
// read-only socket, and the server below refuses input on it regardless.

// termServer serves the terminal protocol to local connections. One source feeds
// every connection through fan, so N peers watching a session cost one tmux
// control client rather than N.
type termServer struct {
	src  termSource
	ptmx *os.File
	fan  *fanout

	// done is closed when the terminal ends, which tells every connection to
	// report the session gone rather than just dropping the stream.
	done chan struct{}

	mu      sync.Mutex
	started bool
}

func newTermServer(src termSource, ptmx *os.File) *termServer {
	return &termServer{src: src, ptmx: ptmx, fan: newFanout(), done: make(chan struct{})}
}

// start attaches to the terminal and begins broadcasting it. It is idempotent so
// the read-write and read-only listeners can both call it.
func (s *termServer) start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started {
		return nil
	}
	cols, rows := ptmxSize(s.ptmx)
	out, err := s.src.start(ctx, cols, rows)
	if err != nil {
		return err
	}
	s.started = true
	go func() {
		defer close(s.done)
		for {
			select {
			case <-ctx.Done():
				return
			case b, open := <-out:
				if !open {
					return
				}
				_, _ = s.fan.Write(b)
			}
		}
	}()
	return nil
}

// stop releases the terminal source. Safe to call without a preceding start.
func (s *termServer) stop() {
	s.mu.Lock()
	started := s.started
	s.started = false
	s.mu.Unlock()
	if started {
		s.src.stop()
	}
}

// serve accepts connections on ln until ctx is done or ln is closed. write says
// whether peers arriving on this listener may type: it is false for the
// read-only socket, and it is what the hello advertises.
func (s *termServer) serve(ctx context.Context, ln net.Listener, write bool) error {
	var wg sync.WaitGroup
	defer wg.Wait()
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			// A listener that stops accepting is not recoverable here; the
			// wrapper keeps running because a broken p2p path must never take
			// the session with it.
			return err
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.handle(ctx, c, write)
		}()
	}
}

// handle runs one peer to exhaustion.
func (s *termServer) handle(ctx context.Context, c net.Conn, write bool) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	w := termwire.NewConn(c)
	defer w.Close()

	cols, rows := ptmxSize(s.ptmx)
	hello, err := json.Marshal(termHostHello{T: "hello", Cols: cols, Rows: rows, Write: write})
	if err != nil {
		return
	}
	if err := w.WriteMsg(termwire.KindText, hello); err != nil {
		return
	}

	// Subscribe before the client can ask for a repaint, so output produced
	// between the snapshot and the first live frame is queued rather than lost.
	sub := s.fan.subscribe()
	defer s.fan.unsubscribe(sub)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer cancel()
		defer w.Close() // unblocks the read loop
		for {
			select {
			case <-ctx.Done():
				return
			case <-s.done:
				_ = w.WriteMsg(termwire.KindText, []byte(`{"t":"exit"}`))
				return
			case b, open := <-sub:
				if !open {
					return
				}
				if err := w.WriteMsg(termwire.KindBinary, b); err != nil {
					return
				}
			}
		}
	}()

	// A read-only peer's read direction ends immediately and by design: the
	// sidecar tears down peer->socket with STOP_SENDING and half-closes this
	// socket, so a viewer's keystrokes cannot even reach us. That EOF is not a
	// disconnect, so only a writable peer's read ending means the peer is gone —
	// otherwise a viewer would be dropped the instant it attached.
	wg.Add(1)
	go func() {
		defer wg.Done()
		s.readLoop(w, write)
		if write {
			cancel()
		}
	}()

	wg.Wait()
}

// readLoop applies a peer's control messages. Input and resize are dropped
// wholesale on a read-only connection: the sidecar should never have forwarded
// them, and if it did, this is the second place that says no.
func (s *termServer) readLoop(w *termwire.Conn, write bool) {
	for {
		kind, data, err := w.ReadMsg()
		if err != nil {
			return
		}
		if kind != termwire.KindText {
			continue
		}
		var msg termHostMsg
		if json.Unmarshal(data, &msg) != nil {
			continue
		}
		switch msg.T {
		case "snapshot":
			// Repaint on demand. It goes straight to the asking peer rather than
			// through the fan-out, so the other viewers' screens are left alone.
			if err := w.WriteMsg(termwire.KindBinary, s.repaint()); err != nil {
				return
			}
		case "input":
			if !write {
				continue
			}
			raw, derr := base64.StdEncoding.DecodeString(msg.B64)
			if derr != nil {
				continue
			}
			_ = s.src.send(raw)
		case "resize":
			if !write {
				continue
			}
			cols, rows := clampTermSize(msg.Cols, msg.Rows)
			_ = s.src.resize(cols, rows)
		}
	}
}

// repaint is a clear-and-home followed by the current screen, which is what a
// terminal needs to show a session it just joined mid-flight.
func (s *termServer) repaint() []byte {
	return append([]byte("\x1b[2J\x1b[H"), s.src.snapshot()...)
}

// termHostHello is the first frame a server sends: the terminal's shape and
// whether this peer may type into it.
type termHostHello struct {
	T     string `json:"t"`
	Cols  int    `json:"cols"`
	Rows  int    `json:"rows"`
	Write bool   `json:"write"`
}

// listenUnix creates a fresh stream socket at path. A stale socket from a
// crashed wrapper is removed first: the path is inside the session's own
// spool directory, so nothing else can own it.
func listenUnix(path string) (net.Listener, error) {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	// The sidecar runs as the same user; nobody else needs the socket.
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		return nil, err
	}
	return ln, nil
}
