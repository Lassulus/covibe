// Package termwire frames covibe's terminal protocol for a plain byte stream.
//
// The dashboard carries that protocol over a WebSocket, which is already
// message-framed: a text frame is control JSON, a binary frame is raw terminal
// bytes. The peer-to-peer path carries the same protocol over a QUIC stream,
// which is not framed, so each message gets a kind and a length in front of it.
//
// The framing exists so the transport can stay ignorant: the sidecar that moves
// these bytes between two machines never looks inside them, which is why it can
// be a byte pump with no idea what a terminal is.
package termwire

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"
)

// Message kinds. These mirror the WebSocket frame types the dashboard path uses,
// so both transports carry byte-identical payloads.
const (
	KindBinary = 0 // raw terminal bytes
	KindText   = 1 // control JSON
)

// MaxPayload bounds one message. Terminal writes are far smaller; the limit is
// here so a corrupt or hostile length cannot make the reader allocate freely.
const MaxPayload = 4 << 20

const headerLen = 5 // kind:u8 + length:u32be

// ErrTooLarge reports a length prefix beyond MaxPayload. It is fatal for the
// stream: the framing is lost, so there is no safe place to resume.
var ErrTooLarge = errors.New("termwire: message exceeds maximum payload")

// Conn reads and writes framed messages over a byte stream. Writes are
// serialized, so several goroutines may write concurrently — the terminal
// output pump and the control replies do exactly that. Reads are not: one
// reader per Conn, as with any framed stream.
type Conn struct {
	rw  io.ReadWriteCloser
	hdr [headerLen]byte // read-side scratch, owned by the single reader

	mu   sync.Mutex
	wbuf []byte // write-side scratch, owned under mu
}

// NewConn frames messages over rw.
func NewConn(rw io.ReadWriteCloser) *Conn { return &Conn{rw: rw} }

// ReadMsg returns the next message. The payload is freshly allocated, so the
// caller may retain it.
func (c *Conn) ReadMsg() (kind byte, payload []byte, err error) {
	if _, err := io.ReadFull(c.rw, c.hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(c.hdr[1:])
	if n > MaxPayload {
		return 0, nil, fmt.Errorf("%w: %d bytes", ErrTooLarge, n)
	}
	if n == 0 {
		return c.hdr[0], nil, nil
	}
	payload = make([]byte, n)
	if _, err := io.ReadFull(c.rw, payload); err != nil {
		return 0, nil, err
	}
	return c.hdr[0], payload, nil
}

// WriteMsg writes one message. Header and payload go out in a single Write so a
// concurrent writer can never interleave between them.
func (c *Conn) WriteMsg(kind byte, payload []byte) error {
	if len(payload) > MaxPayload {
		return fmt.Errorf("%w: %d bytes", ErrTooLarge, len(payload))
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	need := headerLen + len(payload)
	if cap(c.wbuf) < need {
		c.wbuf = make([]byte, need)
	}
	buf := c.wbuf[:need]
	buf[0] = kind
	binary.BigEndian.PutUint32(buf[1:], uint32(len(payload))) // #nosec G115 -- bounded by MaxPayload above
	copy(buf[headerLen:], payload)
	_, err := c.rw.Write(buf)
	return err
}

// Close closes the underlying stream, which is also how a blocked ReadMsg is
// unblocked: a read on a pipe or a socket cannot be cancelled otherwise.
func (c *Conn) Close() error { return c.rw.Close() }
