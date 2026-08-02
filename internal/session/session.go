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
	Model      string // omp --model (optional)
	Thinking   string // omp --thinking level (optional)
	RelayHost  string // public host for guest links, e.g. "covibe.lassul.us"
	WebClient  string // collab-web base for browser links, e.g. "https://my.omp.sh"
	LocalRelay string // ws(s):// base the omp host connects to, e.g. "ws://127.0.0.1:8770"
	Mux        string // "zellij" | "tmux"
	MuxSession string
	MuxSocket  string // tmux server socket the session runs on (recorded for the dashboard)
	Store      *spool.Store
	Sink       Sink // lifecycle backend; nil uses the local on-disk spool (cfg.Store)
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
	sink := cfg.Sink
	if sink == nil {
		if cfg.Store == nil {
			st, err := spool.Open("")
			if err != nil {
				return err
			}
			cfg.Store = st
		}
		sink = localSink{store: cfg.Store}
	}

	rec := &spool.Record{
		ID:         cfg.ID,
		Name:       cfg.Name,
		Dir:        cfg.Dir,
		Mux:        cfg.Mux,
		MuxSession: cfg.MuxSession,
		MuxSocket:  cfg.MuxSocket,
		MuxTab:     cfg.Name,
		Model:      cfg.Model,
		Thinking:   cfg.Thinking,
		PID:        os.Getpid(),
		Status:     spool.StatusStarting,
		StartedAt:  time.Now(),
	}

	// Mint the collab room so the link/QR exist immediately and omp hosts on a
	// covibe-owned id. Requires a relay to connect to and a public host for the
	// shareable links.
	ompArgs := cfg.OmpArgs
	if cfg.Thinking != "" {
		ompArgs = append([]string{"--thinking", cfg.Thinking}, ompArgs...)
	}
	if cfg.Model != "" {
		ompArgs = append([]string{"--model", cfg.Model}, ompArgs...)
	}
	var extraEnv []string
	if cfg.LocalRelay != "" && cfg.RelayHost != "" {
		room, err := collablink.Mint()
		if err != nil {
			return fmt.Errorf("mint collab room: %w", err)
		}
		rec.RoomID = room.ID
		rec.Relay = cfg.LocalRelay
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

	if err := sink.Register(rec); err != nil {
		return fmt.Errorf("register session: %w", err)
	}
	defer sink.End(rec)

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

	// Always-on ring of recent terminal output for the dashboard's pane view.
	pane := &paneBuffer{max: 64 << 10}
	killed, stopWatch := sink.Watch(rec, pane)
	defer stopWatch()

	// stdin → omp
	go func() { _, _ = io.Copy(ptmx, os.Stdin) }()

	// omp → stdout, tapped by the pane ring for snapshots.
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.MultiWriter(os.Stdout, pane), ptmx)
		close(done)
	}()

	select {
	case <-done:
	case <-killed:
		// Dashboard requested termination: stop omp, then drain.
		if cmd.Process != nil {
			_ = cmd.Process.Signal(syscall.SIGTERM)
		}
		<-done
	}
	return cmd.Wait()
}

// Sink is the lifecycle backend a session's wrapper drives around the omp PTY:
// the local on-disk spool (unix-socket pane) or the dashboard REST API (remote).
type Sink interface {
	// Register records the session at start (and may assign rec.ID).
	Register(rec *spool.Record) error
	// Watch runs background liveness/pane handling for the session's lifetime,
	// reading snapshots from pane. It closes the returned channel if it observes
	// an external stop request (e.g. a dashboard kill); the returned func tears
	// the watcher down.
	Watch(rec *spool.Record, pane *paneBuffer) (killed <-chan struct{}, stop func())
	// End marks the session finished.
	End(rec *spool.Record)
}

// localSink is the default backend: an on-disk spool plus a per-session unix
// socket the co-located dashboard reads pane snapshots from on demand.
type localSink struct{ store *spool.Store }

func (s localSink) Register(rec *spool.Record) error { return s.store.Save(rec) }

func (s localSink) Watch(rec *spool.Record, pane *paneBuffer) (<-chan struct{}, func()) {
	closePane, err := servePane(s.store.PanePath(rec.ID), pane)
	if err != nil {
		return nil, func() {}
	}
	return nil, closePane
}

func (s localSink) End(rec *spool.Record) {
	rec.Status = spool.StatusEnded
	_ = s.store.Save(rec)
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
