package dashboard

import (
	"io"
	"net"
	"time"
)

// paneReadTimeout bounds a pane snapshot fetch.
const paneReadTimeout = 2 * time.Second

// paneReadLimit caps a snapshot to avoid unbounded reads.
const paneReadLimit = 256 << 10

// readPane dials the session wrapper's pane socket and reads the current
// terminal snapshot it serves.
func readPane(sock string) ([]byte, error) {
	conn, err := net.DialTimeout("unix", sock, paneReadTimeout)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(paneReadTimeout))
	return io.ReadAll(io.LimitReader(conn, paneReadLimit))
}
