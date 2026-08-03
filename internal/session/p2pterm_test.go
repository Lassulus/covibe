package session

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/lassulus/covibe/internal/termwire"
)

// fakeSource stands in for tmux or the omp pty: it records what the server tried
// to do to the terminal, which is how a read-only connection is proven inert.
type fakeSource struct {
	out chan []byte

	mu      sync.Mutex
	sent    []byte
	resizes [][2]int
	stopped bool
}

func (f *fakeSource) start(context.Context, int, int) (<-chan []byte, error) { return f.out, nil }

func (f *fakeSource) stop() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped = true
}

func (f *fakeSource) snapshot() []byte { return []byte("SCREEN") }

func (f *fakeSource) send(b []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, b...)
	return nil
}

func (f *fakeSource) resize(cols, rows int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resizes = append(f.resizes, [2]int{cols, rows})
	return nil
}

func (f *fakeSource) input() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return bytes.Clone(f.sent)
}

func (f *fakeSource) resized() [][2]int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][2]int(nil), f.resizes...)
}

// dialTermServer runs one connection against a fresh server and returns the peer
// end of it.
func dialTermServer(t *testing.T, write bool) (*termwire.Conn, *fakeSource) {
	t.Helper()
	src := &fakeSource{out: make(chan []byte, 4)}
	srv := newTermServer(src, nil)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := srv.start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.stop)

	peer, server := net.Pipe()
	go srv.handle(ctx, server, write)
	t.Cleanup(func() { _ = peer.Close() })
	return termwire.NewConn(peer), src
}

// readHello consumes the first frame, which must be the hello, and reports
// whether the peer was told it may type.
func readHello(t *testing.T, c *termwire.Conn) (write bool, cols, rows int) {
	t.Helper()
	kind, data, err := c.ReadMsg()
	if err != nil {
		t.Fatalf("hello: %v", err)
	}
	if kind != termwire.KindText {
		t.Fatalf("first frame kind=%d, want text hello", kind)
	}
	var msg struct {
		T     string `json:"t"`
		Write bool   `json:"write"`
		Cols  int    `json:"cols"`
		Rows  int    `json:"rows"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatalf("hello %q: %v", data, err)
	}
	if msg.T != "hello" {
		t.Fatalf("first frame t=%q, want hello", msg.T)
	}
	return msg.Write, msg.Cols, msg.Rows
}

// requestRepaint asks for a screen and returns it. On the peer-to-peer path this
// is also the message that makes the host notice the stream at all.
func requestRepaint(t *testing.T, c *termwire.Conn) []byte {
	t.Helper()
	msg, _ := json.Marshal(clientControl{T: "snapshot"})
	if err := c.WriteMsg(termwire.KindText, msg); err != nil {
		t.Fatalf("request repaint: %v", err)
	}
	kind, data, err := c.ReadMsg()
	if err != nil {
		t.Fatalf("repaint: %v", err)
	}
	if kind != termwire.KindBinary {
		t.Fatalf("repaint kind=%d, want binary", kind)
	}
	return data
}

func writeInput(t *testing.T, c *termwire.Conn, s string) {
	t.Helper()
	msg, _ := json.Marshal(clientControl{T: "input", B64: base64.StdEncoding.EncodeToString([]byte(s))})
	if err := c.WriteMsg(termwire.KindText, msg); err != nil {
		t.Fatalf("send input: %v", err)
	}
}

type clientControl struct {
	T    string `json:"t"`
	B64  string `json:"b64,omitempty"`
	Cols int    `json:"cols,omitempty"`
	Rows int    `json:"rows,omitempty"`
}

// A writable peer reaches the terminal: it is greeted as a writer, gets a
// repaint, sees live output, and its keystrokes land on the source.
func TestTermServerWritablePeerReachesTerminal(t *testing.T) {
	c, src := dialTermServer(t, true)

	write, cols, rows := readHello(t, c)
	if !write {
		t.Fatal("writable peer told it is read-only")
	}
	if cols <= 0 || rows <= 0 {
		t.Fatalf("hello carries no terminal size: %dx%d", cols, rows)
	}

	// A joining peer asks for the screen and gets it, cleared first so the
	// repaint cannot land on top of whatever the local terminal had.
	data := requestRepaint(t, c)
	if !bytes.Contains(data, []byte("SCREEN")) {
		t.Fatalf("repaint %q does not carry the screen", data)
	}
	if !bytes.HasPrefix(data, []byte("\x1b[2J")) {
		t.Fatalf("repaint does not clear first: %q", data[:min(8, len(data))])
	}

	// Live output reaches the peer.
	src.out <- []byte("live-bytes")
	kind, data, err := c.ReadMsg()
	if err != nil {
		t.Fatalf("live: %v", err)
	}
	if kind != termwire.KindBinary || !bytes.Equal(data, []byte("live-bytes")) {
		t.Fatalf("live kind=%d data=%q", kind, data)
	}

	writeInput(t, c, "typed")
	waitFor(t, func() bool { return string(src.input()) == "typed" },
		func() string { return "input never reached the terminal: " + string(src.input()) })
}

// A read-only peer can watch and nothing else. This is the load-bearing property
// of the whole ticket model: the read-only ticket names a different endpoint, and
// the server behind it must refuse input even if something forwards it anyway.
func TestTermServerReadOnlyPeerCannotType(t *testing.T) {
	c, src := dialTermServer(t, false)

	write, _, _ := readHello(t, c)
	if write {
		t.Fatal("read-only peer told it may write")
	}

	// It still gets the screen: watching is the point.
	if data := requestRepaint(t, c); !bytes.Contains(data, []byte("SCREEN")) {
		t.Fatalf("read-only peer got no screen: %q", data)
	}

	writeInput(t, c, "should-never-land")
	resize, _ := json.Marshal(clientControl{T: "resize", Cols: 80, Rows: 24})
	if err := c.WriteMsg(termwire.KindText, resize); err != nil {
		t.Fatalf("send resize: %v", err)
	}

	// Prove the messages were processed rather than merely slow: a snapshot
	// request after them is answered, so the read loop has moved past both.
	if data := requestRepaint(t, c); !bytes.Contains(data, []byte("SCREEN")) {
		t.Fatalf("no repaint after the refused input: %q", data)
	}

	if got := src.input(); len(got) != 0 {
		t.Fatalf("read-only peer typed into the session: %q", got)
	}
	if got := src.resized(); len(got) != 0 {
		t.Fatalf("read-only peer reshaped the session: %v", got)
	}
}

// Several peers watch one terminal on one source: the fan-out is what keeps N
// viewers from costing N tmux control clients.
func TestTermServerFansOutToEveryPeer(t *testing.T) {
	src := &fakeSource{out: make(chan []byte, 4)}
	srv := newTermServer(src, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := srv.start(ctx); err != nil {
		t.Fatal(err)
	}
	defer srv.stop()

	conns := make([]*termwire.Conn, 3)
	for i := range conns {
		peer, server := net.Pipe()
		defer peer.Close()
		go srv.handle(ctx, server, i == 0) // one writer, two watchers
		c := termwire.NewConn(peer)
		if write, _, _ := readHello(t, c); write != (i == 0) {
			t.Fatalf("peer %d write=%v", i, write)
		}
		requestRepaint(t, c)
		conns[i] = c
	}

	src.out <- []byte("broadcast")
	for i, c := range conns {
		kind, data, err := c.ReadMsg()
		if err != nil || kind != termwire.KindBinary || !bytes.Equal(data, []byte("broadcast")) {
			t.Fatalf("peer %d: kind=%d data=%q err=%v", i, kind, data, err)
		}
	}
}

// When the terminal ends, every peer is told the session is gone rather than
// left staring at a stream that simply stopped.
func TestTermServerReportsExitWhenTerminalEnds(t *testing.T) {
	c, src := dialTermServer(t, true)
	readHello(t, c)
	requestRepaint(t, c)

	close(src.out) // the terminal ended

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		kind, data, err := c.ReadMsg()
		if err != nil {
			t.Fatalf("no exit before the stream closed: %v", err)
		}
		if kind == termwire.KindText && bytes.Contains(data, []byte(`"exit"`)) {
			return
		}
	}
	t.Fatal("never told the session ended")
}

func waitFor(t *testing.T, ok func() bool, msg func() string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal(msg())
}

// The sidecar half-closes a read-only peer's write direction the moment it
// bridges the connection: peer bytes are stopped at the QUIC layer, so this
// socket immediately reports EOF on its read side. That is not a disconnect, and
// a viewer dropped at that instant sees nothing at all — which is exactly what
// happened before the read and write halves were untangled.
func TestTermServerReadOnlyOutlivesItsClosedReadSide(t *testing.T) {
	src := &fakeSource{out: make(chan []byte, 4)}
	srv := newTermServer(src, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := srv.start(ctx); err != nil {
		t.Fatal(err)
	}
	defer srv.stop()

	// A real unix socket, because net.Pipe cannot express a half-close.
	path := filepath.Join(t.TempDir(), "ro.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		srv.handle(ctx, c, false)
	}()

	peer, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	c := termwire.NewConn(peer)

	if write, _, _ := readHello(t, c); write {
		t.Fatal("read-only peer told it may write")
	}
	// Exactly what the sidecar does for the read-only endpoint.
	if err := peer.(*net.UnixConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}

	// The session keeps streaming to the viewer afterwards.
	for i, want := range []string{"after-half-close", "still-watching"} {
		src.out <- []byte(want)
		kind, data, err := c.ReadMsg()
		if err != nil {
			t.Fatalf("frame %d after half-close: %v", i, err)
		}
		if kind != termwire.KindBinary || string(data) != want {
			t.Fatalf("frame %d: kind=%d data=%q want %q", i, kind, data, want)
		}
	}
}
