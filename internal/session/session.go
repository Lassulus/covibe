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
	"github.com/lassulus/covibe/internal/tmuxctl"
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
	MuxSession string
	MuxSocket  string // tmux server socket the session runs on (recorded for the dashboard)
	Token      string // per-user dashboard API key; empty disables the remote terminal
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

	// Ring of recent terminal output: the snapshot a browser gets when it
	// attaches to a session whose terminal is streamed from the pty.
	pane := &paneBuffer{max: 64 << 10}
	killed, stopWatch := sink.Watch(rec, pane)
	defer stopWatch()

	// Remote terminal: the dashboard cannot dial this machine, so the wrapper
	// dials out and relays its terminal to whoever opens it in the browser. It
	// needs a per-user key to prove which user owns this session; without one
	// (or without a dashboard) the session simply has no terminal, as before.
	// Nothing here is allowed to affect omp: a broken relay is not a dead agent.
	var fan *fanout
	if ds, ok := sink.(dashboardSink); ok && cfg.Token != "" && ds.DashboardBase() != "" {
		var src termSource
		if cfg.MuxSocket != "" && cfg.MuxSession != "" {
			src = &tmuxSource{srv: tmuxctl.Server{Socket: cfg.MuxSocket}, session: cfg.MuxSession}
		} else {
			fan = newFanout()
			src = &ptySource{ptmx: ptmx, pane: pane, fan: fan}
		}
		host := newTermHost(ds, cfg.Token, !rec.ViewOnly, ptmx, src)
		host.start()
		defer host.stop()
	}

	// stdin → omp
	go func() { _, _ = io.Copy(ptmx, os.Stdin) }()

	// omp → stdout, tapped by the pane ring for snapshots and, when the terminal
	// host streams the pty, by its fan-out. One reader on the pty, several taps.
	tee := []io.Writer{os.Stdout, pane}
	if fan != nil {
		tee = append(tee, fan)
	}
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.MultiWriter(tee...), ptmx)
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
// the local on-disk spool, or the dashboard REST API (remote).
type Sink interface {
	// Register records the session at start (and may assign rec.ID).
	Register(rec *spool.Record) error
	// Watch runs background liveness handling for the session's lifetime, with
	// pane as the snapshot source. It closes the returned channel if it observes
	// an external stop request (e.g. a dashboard kill); the returned func tears
	// the watcher down.
	Watch(rec *spool.Record, pane *paneBuffer) (killed <-chan struct{}, stop func())
	// End marks the session finished.
	End(rec *spool.Record)
}

// localSink is the default backend: the on-disk spool. Pane snapshots need no
// channel of their own — the dashboard captures the rendered grid straight from
// the session's tmux server.
type localSink struct{ store *spool.Store }

func (s localSink) Register(rec *spool.Record) error { return s.store.Save(rec) }

func (s localSink) Watch(_ *spool.Record, _ *paneBuffer) (<-chan struct{}, func()) {
	return nil, func() {}
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
