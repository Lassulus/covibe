// Package session implements `covibe session`: the command a multiplexer runs
// inside a pane. It is a transparent PTY proxy around the patched `omp` that
// (a) mints an omp-compatible collab room up front, (b) hands it to omp via
// OMP_COLLAB_* env so omp autostarts its native collab host headlessly against
// covibe's relay, and (c) registers the session in the spool with its stable
// room id + join/browser links. No scraping, no capture — covibe owns the room.
package session

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"golang.org/x/term"

	"github.com/lassulus/covibe/internal/collablink"
	"github.com/lassulus/covibe/internal/spool"
)

// Config parameterizes a session wrapper.
type Config struct {
	ID         string // stable id; generated if empty
	Name       string // human label
	Dir        string // working directory for omp
	OmpBin     string // omp binary (default "omp")
	OmpArgs    []string
	RelayHost  string // public host for guest links, e.g. "covibe.lassul.us"
	WebClient  string // collab-web base for browser links, e.g. "https://my.omp.sh"
	LocalRelay string // ws(s):// base the omp host connects to, e.g. "ws://127.0.0.1:8770"
	Mux        string // "zellij" | "tmux"
	MuxSession string
	Store      *spool.Store
}

// Run executes the wrapper: mint the room, register the session, spawn the
// patched omp under a PTY with OMP_COLLAB_* set so it hosts natively, proxy I/O,
// and keep the spool record in sync with the session lifecycle.
func Run(cfg Config) error {
	if cfg.OmpBin == "" {
		cfg.OmpBin = "omp"
	}
	if cfg.ID == "" {
		cfg.ID = newID()
	}
	if cfg.Store == nil {
		st, err := spool.Open("")
		if err != nil {
			return err
		}
		cfg.Store = st
	}

	rec := &spool.Record{
		ID:         cfg.ID,
		Name:       cfg.Name,
		Dir:        cfg.Dir,
		Mux:        cfg.Mux,
		MuxSession: cfg.MuxSession,
		MuxTab:     cfg.Name,
		PID:        os.Getpid(),
		Status:     spool.StatusStarting,
		StartedAt:  time.Now(),
	}

	// Mint the collab room so the link/QR exist immediately and omp hosts on a
	// covibe-owned id. Requires a relay to connect to and a public host for the
	// shareable links.
	ompArgs := cfg.OmpArgs
	var extraEnv []string
	if cfg.LocalRelay != "" && cfg.RelayHost != "" {
		room, err := collablink.Mint()
		if err != nil {
			return fmt.Errorf("mint collab room: %w", err)
		}
		rec.RoomID = room.ID
		rec.JoinLink = collablink.JoinLink(cfg.RelayHost, room.ID, room.Secret)
		if cfg.WebClient != "" {
			rec.BrowserURL = collablink.BrowserURL(cfg.WebClient, cfg.RelayHost, room.ID, room.Secret)
		}
		extraEnv = []string{
			"OMP_COLLAB_RELAY=" + cfg.LocalRelay,
			"OMP_COLLAB_ROOM=" + room.ID,
			"OMP_COLLAB_KEY=" + room.Secret,
		}
	}

	if err := cfg.Store.Save(rec); err != nil {
		return fmt.Errorf("register session: %w", err)
	}
	defer func() {
		rec.Status = spool.StatusEnded
		_ = cfg.Store.Save(rec)
	}()

	cmd := exec.Command(cfg.OmpBin, ompArgs...) // #nosec G204 -- omp binary is operator-configured; launched with no shell
	cmd.Dir = cfg.Dir
	cmd.Env = append(os.Environ(), extraEnv...)

	ptmx, err := pty.Start(cmd)
	if err != nil {
		return fmt.Errorf("start %s: %w", cfg.OmpBin, err)
	}
	defer ptmx.Close()

	stopResize := watchResize(ptmx)
	defer stopResize()

	// Put our controlling terminal in raw mode so keystrokes pass through
	// untouched. Skipped when stdin is not a terminal (tests, pipes).
	var restore func()
	if term.IsTerminal(int(os.Stdin.Fd())) {
		old, err := term.MakeRaw(int(os.Stdin.Fd()))
		if err == nil {
			restore = func() { _ = term.Restore(int(os.Stdin.Fd()), old) }
		}
	}
	if restore != nil {
		defer restore()
	}

	// Always-on ring of recent output, served to the dashboard's pane endpoint
	// over a per-session unix socket.
	pane := &paneBuffer{max: 64 << 10}
	if closePane, err := servePane(cfg.Store.PanePath(cfg.ID), pane); err == nil {
		defer closePane()
	}

	// stdin → omp
	go func() { _, _ = io.Copy(ptmx, os.Stdin) }()

	// omp → stdout, tapped by the pane ring for on-demand snapshots.
	_, _ = io.Copy(io.MultiWriter(os.Stdout, pane), ptmx)

	return cmd.Wait()
}

// paneBuffer keeps the most recent terminal output for on-demand snapshots.
type paneBuffer struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func (p *paneBuffer) Write(b []byte) (int, error) {
	p.mu.Lock()
	p.buf = append(p.buf, b...)
	if len(p.buf) > p.max {
		p.buf = p.buf[len(p.buf)-p.max:]
	}
	p.mu.Unlock()
	return len(b), nil
}

func (p *paneBuffer) snapshot() []byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]byte, len(p.buf))
	copy(out, p.buf)
	return out
}

// servePane listens on a unix socket and writes the current pane snapshot to
// each connection. Returns a closer that stops the listener and removes the
// socket file.
func servePane(sockPath string, pane *paneBuffer) (func(), error) {
	_ = os.Remove(sockPath)
	l, err := net.Listen("unix", sockPath)
	if err != nil {
		return nil, err
	}
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_, _ = conn.Write(pane.snapshot())
			}()
		}
	}()
	return func() {
		_ = l.Close()
		_ = os.Remove(sockPath)
	}, nil
}

// watchResize mirrors our terminal size onto the pty and refreshes it on
// SIGWINCH. It returns a stop function.
func watchResize(ptmx *os.File) func() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGWINCH)
	ctx, cancel := context.WithCancel(context.Background())
	resize := func() {
		if term.IsTerminal(int(os.Stdin.Fd())) {
			_ = pty.InheritSize(os.Stdin, ptmx)
		}
	}
	resize()
	go func() {
		for {
			select {
			case <-ch:
				resize()
			case <-ctx.Done():
				return
			}
		}
	}()
	return func() {
		signal.Stop(ch)
		cancel()
	}
}

func newID() string {
	return fmt.Sprintf("%d-%d", time.Now().UnixNano(), os.Getpid())
}
