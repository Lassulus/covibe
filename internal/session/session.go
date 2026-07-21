// Package session implements `covibe session`: the command a multiplexer runs
// inside a pane. It is a transparent PTY proxy around `omp` that (a) registers
// the session in the spool, (b) sniffs the /collab join link out of omp's
// output and records it, and (c) optionally auto-starts sharing so a link
// exists without the operator typing anything.
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

	"github.com/lassulus/covibe/internal/collab"
	"github.com/lassulus/covibe/internal/spool"
)

// Config parameterizes a session wrapper.
type Config struct {
	ID         string        // stable id; generated if empty
	Name       string        // human label
	Dir        string        // working directory for omp
	OmpBin     string        // omp binary (default "omp")
	OmpArgs    []string      // extra args passed to omp
	Relay      string        // relay URL for /collab (inline)
	WebURL     string        // browser UI base for deep links
	AutoCollab string        // "", "full", or "view": auto-run /collab on start
	AutoDelay  time.Duration // delay before auto /collab (default 2s)
	Mux        string        // "zellij" | "tmux"
	MuxSession string
	Store      *spool.Store
}

// linkSink accumulates proxied output and reports the first collab link seen.
// It is written to from the pty→stdout copy path, so it must be cheap and
// never block that path.
type linkSink struct {
	mu    sync.Mutex
	buf   []byte
	max   int
	done  bool
	onHit func(link string)
}

func newLinkSink(onHit func(string)) *linkSink {
	return &linkSink{max: 64 << 10, onHit: onHit}
}

func (s *linkSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	if s.done {
		s.mu.Unlock()
		return len(p), nil
	}
	s.buf = append(s.buf, p...)
	if len(s.buf) > s.max {
		s.buf = s.buf[len(s.buf)-s.max:]
	}
	link, ok := collab.Extract(s.buf)
	if ok {
		s.done = true
		s.buf = nil
	}
	s.mu.Unlock()
	if ok {
		s.onHit(link)
	}
	return len(p), nil
}

// Run executes the wrapper: spawn omp under a PTY, proxy I/O, capture the link,
// and keep the spool record in sync with the session lifecycle.
func Run(cfg Config) error {
	if cfg.OmpBin == "" {
		cfg.OmpBin = "omp"
	}
	if cfg.AutoDelay == 0 {
		cfg.AutoDelay = 2 * time.Second
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
		Relay:      cfg.Relay,
		ViewOnly:   cfg.AutoCollab == "view",
		StartedAt:  time.Now(),
	}
	if err := cfg.Store.Save(rec); err != nil {
		return fmt.Errorf("register session: %w", err)
	}
	// Best-effort cleanup: mark ended so the dashboard drops it promptly.
	defer func() {
		rec.Status = spool.StatusEnded
		_ = cfg.Store.Save(rec)
	}()

	cmd := exec.Command(cfg.OmpBin, cfg.OmpArgs...)
	cmd.Dir = cfg.Dir
	cmd.Env = os.Environ()

	ptmx, err := pty.Start(cmd)
	if err != nil {
		return fmt.Errorf("start %s: %w", cfg.OmpBin, err)
	}
	defer ptmx.Close()

	// Keep the inner pty sized to our own terminal.
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

	sink := newLinkSink(func(link string) {
		rec.JoinLink = link
		rec.BrowserURL = collab.BrowserURL(link, cfg.Relay, cfg.WebURL)
		rec.Status = spool.StatusLive
		_ = cfg.Store.Save(rec)
	})

	// Always-on ring of recent output, served to the dashboard's pane endpoint
	// over a per-session unix socket.
	pane := &paneBuffer{max: 64 << 10}
	if closePane, err := servePane(cfg.Store.PanePath(cfg.ID), pane); err == nil {
		defer closePane()
	}

	// stdin → omp
	go func() { _, _ = io.Copy(ptmx, os.Stdin) }()

	// Optionally trigger sharing once omp has settled.
	if cfg.AutoCollab != "" {
		go autoCollab(ptmx, cfg)
	}

	// omp → stdout, tapped by the link sink and the pane ring.
	_, _ = io.Copy(io.MultiWriter(os.Stdout, sink, pane), ptmx)

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

// autoCollab types the /collab command into omp after a short settle delay so a
// shareable link is produced without operator interaction.
func autoCollab(ptmx io.Writer, cfg Config) {
	time.Sleep(cfg.AutoDelay)
	line := "/collab"
	if cfg.AutoCollab == "view" {
		line += " view"
	}
	if cfg.Relay != "" {
		line += " " + cfg.Relay
	}
	_, _ = io.WriteString(ptmx, line+"\r")
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
