package termwire

import (
	"bytes"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
)

// A message survives the round trip with its kind and its exact bytes. The
// sidecar carrying these frames is a byte pump, so any reframing or reordering
// here would corrupt a terminal on the far side.
func TestRoundTripPreservesKindAndBytes(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	w, r := NewConn(a), NewConn(b)

	want := []struct {
		kind byte
		data []byte
	}{
		{KindText, []byte(`{"t":"hello"}`)},
		{KindBinary, []byte("\x1b[2J\x1b[Hscreen")},
		{KindBinary, nil}, // an empty payload is a legal frame, not EOF
		{KindText, []byte(`{"t":"exit"}`)},
	}
	go func() {
		for _, m := range want {
			if err := w.WriteMsg(m.kind, m.data); err != nil {
				return
			}
		}
	}()
	for i, m := range want {
		kind, data, err := r.ReadMsg()
		if err != nil {
			t.Fatalf("msg %d: %v", i, err)
		}
		if kind != m.kind {
			t.Fatalf("msg %d: kind=%d want %d", i, kind, m.kind)
		}
		if !bytes.Equal(data, m.data) {
			t.Fatalf("msg %d: data=%q want %q", i, data, m.data)
		}
	}
}

// Framing must hold under a large payload split across many TCP-sized reads:
// io.ReadFull is what guarantees a short read is not mistaken for a message
// boundary.
func TestLargePayloadReassembles(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()

	payload := make([]byte, 512<<10)
	for i := range payload {
		payload[i] = byte(i * 7) // a pattern where a dropped or reordered chunk shows
	}

	go func() { _ = NewConn(a).WriteMsg(KindBinary, payload) }()
	kind, got, err := NewConn(b).ReadMsg()
	if err != nil {
		t.Fatal(err)
	}
	if kind != KindBinary || !bytes.Equal(got, payload) {
		t.Fatalf("kind=%d len=%d want %d/%d", kind, len(got), KindBinary, len(payload))
	}
}

// Concurrent writers must not interleave a header with another message's
// payload: the terminal output pump and the control replies write on separate
// goroutines, so a torn frame would desynchronize the stream permanently.
func TestConcurrentWritesDoNotInterleave(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	w, r := NewConn(a), NewConn(b)

	const writers, each = 8, 25
	var wg sync.WaitGroup
	for i := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			body := bytes.Repeat([]byte{byte('a' + i)}, 1000)
			for range each {
				if err := w.WriteMsg(KindBinary, body); err != nil {
					return
				}
			}
		}()
	}
	go func() { wg.Wait(); a.Close() }()

	seen := 0
	for {
		kind, data, err := r.ReadMsg()
		if err != nil {
			break
		}
		if kind != KindBinary || len(data) != 1000 {
			t.Fatalf("torn frame: kind=%d len=%d", kind, len(data))
		}
		// Every byte of a frame must come from the one writer that wrote it.
		if bytes.Count(data, data[:1]) != len(data) {
			t.Fatalf("frame mixes writers: %q...", data[:16])
		}
		seen++
	}
	if seen != writers*each {
		t.Fatalf("read %d frames, want %d", seen, writers*each)
	}
}

// A length prefix beyond the cap is refused rather than allocated: the reader
// would otherwise let a peer choose how much memory to reserve.
func TestOversizeLengthRefused(t *testing.T) {
	// Header claiming 64 MiB, with no payload behind it.
	hdr := []byte{KindBinary, 0x04, 0x00, 0x00, 0x00}
	c := NewConn(nopCloser{bytes.NewReader(hdr)})
	if _, _, err := c.ReadMsg(); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err=%v want ErrTooLarge", err)
	}
	if err := NewConn(nopCloser{bytes.NewReader(nil)}).WriteMsg(KindBinary, make([]byte, MaxPayload+1)); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("write err=%v want ErrTooLarge", err)
	}
}

// A truncated payload is an error, never a short message: half a frame handed to
// a terminal is a corrupt screen.
func TestTruncatedPayloadIsAnError(t *testing.T) {
	buf := []byte{KindBinary, 0, 0, 0, 10, 'a', 'b', 'c'}
	if _, _, err := NewConn(nopCloser{bytes.NewReader(buf)}).ReadMsg(); err == nil {
		t.Fatal("truncated payload accepted")
	}
}

type nopCloser struct{ io.Reader }

func (nopCloser) Write(p []byte) (int, error) { return len(p), nil }
func (nopCloser) Close() error                { return nil }
