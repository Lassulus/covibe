// Package session implements `covibe session`: the command a multiplexer runs
// inside a pane. It is a transparent PTY proxy around `omp` that (a) registers
// the session in the spool with its browser link/QR up front, and (b) loads the
// covibe-collab omp extension so the running session streams to covibe's own
// collab relay (the dashboard) for browser viewers. No terminal scraping.
package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	_ "embed"

	"github.com/creack/pty"
	"golang.org/x/term"

	"github.com/lassulus/covibe/internal/spool"
)

//go:embed assets/covibe-collab.ts
var collabPluginTS string

// Config parameterizes a session wrapper.
type Config struct {
	ID         string // stable id; generated if empty
	Name       string // human label
	Dir        string // working directory for omp
	OmpBin     string // omp binary (default "omp")
	OmpArgs    []string
	WebURL     string // browser UI base; BrowserURL = WebURL + "/s/" + ID
	CollabWS   string // ws(s):// base of the covibe dashboard relay, e.g. ws://127.0.0.1:8770
	Mux        string // "zellij" | "tmux"
	MuxSession string
	Store      *spool.Store
}

// Run executes the wrapper: register the session, spawn omp under a PTY with the
// covibe-collab extension loaded, proxy I/O, and keep the spool record in sync
// with the session lifecycle.
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
	if cfg.WebURL != "" {
		rec.BrowserURL = strings.TrimSuffix(cfg.WebURL, "/") + "/s/" + cfg.ID
	}

	// Wire the covibe-collab extension when a relay base is configured. The link
	// and QR exist immediately (no waiting on any handshake) because covibe owns
	// the room identity: we mint the host token here and store it for the relay
	// to validate the plugin's connection.
	ompArgs := cfg.OmpArgs
	var extraEnv []string
	if cfg.CollabWS != "" {
		token := randToken()
		rec.HostToken = token
		pluginPath, err := writePlugin(cfg.Store.Dir())
		if err != nil {
			return fmt.Errorf("write collab plugin: %w", err)
		}
		hostURL := strings.TrimSuffix(cfg.CollabWS, "/") + "/collab/host/" + cfg.ID + "?token=" + token
		ompArgs = append([]string{"-e", pluginPath}, cfg.OmpArgs...)
		extraEnv = []string{
			"COVIBE_COLLAB_URL=" + hostURL,
			"COVIBE_SESSION_NAME=" + cfg.Name,
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

// writePlugin materializes the embedded covibe-collab extension next to the
// spool and returns its path. Rewritten each run so it always matches the
// covibe binary that produced it.
func writePlugin(dir string) (string, error) {
	path := filepath.Join(dir, "covibe-collab.ts")
	if err := os.WriteFile(path, []byte(collabPluginTS), 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// randToken returns a 128-bit hex token used to authorize the host plugin's
// relay connection for a single session.
func randToken() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
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
