// Package mux launches a covibe session command inside a terminal
// multiplexer tab/window. zellij is the primary backend; tmux is provided as a
// second adapter (and is what the end-to-end test drives).
package mux

import (
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Spec describes a session to launch.
type Spec struct {
	ID      string
	Name    string // tab/window name
	Dir     string // working directory
	Session string // multiplexer session name
	// Socket is the tmux server socket to launch on (tmux only). Each covibe
	// user gets their own socket, which is both the isolation boundary between
	// users and the handle the dashboard drives with control mode.
	Socket    string
	InnerArgv []string // the full `covibe session ...` argv to run in the pane
}

// Launcher opens a pane running InnerArgv.
type Launcher interface {
	// Ensure makes the multiplexer session exist without attaching, so a
	// headless caller (e.g. the dashboard) can add tabs to it. No-op when the
	// backend creates the session as part of Launch.
	Ensure(session string) error
	// Command returns the multiplexer argv (for dry-run/tests).
	Command(Spec) ([]string, error)
	// Launch runs the multiplexer command.
	Launch(Spec) error
	// Kill tears down the multiplexer session (best-effort cleanup after the
	// session's process has been signalled). Only Socket and Session are read.
	Kill(Spec) error
}

// For returns the launcher for a backend name.
func For(name string) (Launcher, error) {
	switch name {
	case "zellij", "":
		return zellij{}, nil
	case "tmux":
		return tmux{}, nil
	default:
		return nil, fmt.Errorf("unknown mux %q (want zellij or tmux)", name)
	}
}

func run(argv []string) error {
	if len(argv) == 0 {
		return fmt.Errorf("empty command")
	}
	cmd := exec.Command(argv[0], argv[1:]...) // #nosec G204 -- argv built from validated name + operator config; no shell
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w: %s", argv[0], err, out)
	}
	return nil
}

// runIn is run() with an explicit working directory. `zellij attach
// --create-background` daemonizes into the current cwd and fails with EACCES
// when that dir is unreadable by the user, so we launch it from a safe dir.
func runIn(dir string, argv []string) error {
	if len(argv) == 0 {
		return fmt.Errorf("empty command")
	}
	cmd := exec.Command(argv[0], argv[1:]...) // #nosec G204 -- argv built from validated name + operator config; no shell
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w: %s", argv[0], err, out)
	}
	return nil
}

// SessionName derives a unique, readable multiplexer session name for one
// covibe session: "<name-or-base>-<shortid>". The short id keeps sessions
// distinct even when two share a display name; each covibe session thus gets
// its own mux session (no shared session, no stacked panes).
func SessionName(base, name, id string) string {
	n := sanitizeMux(name)
	if n == "" {
		n = sanitizeMux(base)
	}
	if n == "" {
		n = "covibe"
	}
	if s := shortID(id); s != "" {
		return n + "-" + s
	}
	return n
}

func sanitizeMux(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

func shortID(id string) string {
	s := sanitizeMux(id)
	if len(s) > 8 {
		s = s[len(s)-8:]
	}
	return strings.Trim(s, "-_")
}

// Kill tears down a backend's session by name (best-effort). socket is the tmux
// server socket and is ignored by backends that do not have one.
func Kill(backend, socket, session string) {
	if l, err := For(backend); err == nil {
		_ = l.Kill(Spec{Socket: socket, Session: session})
	}
}

// --- zellij -----------------------------------------------------------------

type zellij struct{}

// Command opens a new named tab in the target session and runs the inner argv
// there. `zellij run` spawns the command in the (newly focused) tab; --cwd sets
// the working directory; --close-on-exit removes the pane when omp exits.
func (zellij) Command(s Spec) ([]string, error) {
	if s.Session == "" {
		return nil, fmt.Errorf("zellij: session name required")
	}
	argv := []string{"zellij", "--session", s.Session, "run"}
	if s.Dir != "" {
		argv = append(argv, "--cwd", s.Dir)
	}
	if s.Name != "" {
		argv = append(argv, "--name", s.Name)
	}
	argv = append(argv, "--close-on-exit", "--")
	argv = append(argv, s.InnerArgv...)
	return argv, nil
}

// sessionState classifies a zellij session for Ensure.
type sessionState int

const (
	stateNone sessionState = iota
	stateLive
	stateExited
)

// Ensure makes the target session exist and be *live*. A live session of the
// same name is reused; a resurrectable EXITED corpse (which `list-sessions`
// still reports, shadowing a fresh session and making `run` fail with "no
// active session") is deleted first; then a detached session is created.
func (zellij) Ensure(session string) error {
	if session == "" {
		return fmt.Errorf("zellij: session name required")
	}
	switch zellijState(session) {
	case stateLive:
		return nil
	case stateExited:
		_ = run([]string{"zellij", "delete-session", session, "--force"})
	}
	// Run from "/" so an unreadable service WorkingDirectory can't trip the
	// daemonize chdir (EACCES).
	if err := runIn("/", []string{"zellij", "attach", "--create-background", session}); err != nil {
		return err
	}
	// create-background returns before the session is ready for `run`; wait.
	for i := 0; i < 50; i++ {
		if zellijState(session) == stateLive {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("zellij: session %q did not become live", session)
}

// zellijState reports whether a session is live, exited (resurrectable), or
// absent. `list-sessions -n` prints one line per session with no ANSI; exited
// sessions carry an "(EXITED..." suffix.
func zellijState(name string) sessionState {
	out, err := exec.Command("zellij", "list-sessions", "-n").Output()
	if err != nil {
		return stateNone
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] != name {
			continue
		}
		if strings.Contains(line, "EXITED") {
			return stateExited
		}
		return stateLive
	}
	return stateNone
}

// Kill terminates the session and clears its resurrectable corpse.
func (zellij) Kill(s Spec) error {
	if s.Session == "" {
		return nil
	}
	_ = run([]string{"zellij", "kill-session", s.Session})
	_ = run([]string{"zellij", "delete-session", s.Session, "--force"})
	return nil
}

func (z zellij) Launch(s Spec) error {
	argv, err := z.Command(s)
	if err != nil {
		return err
	}
	return run(argv)
}

// --- tmux -------------------------------------------------------------------

type tmux struct{}

// Ensure is a no-op for tmux: Command already creates the session (via
// new-session) when it is missing, and the tmux server daemonizes itself.
func (tmux) Ensure(_ string) error { return nil }

// Command creates a fresh detached tmux session running the pane command
// directly (no shell, so no quoting games). Each covibe session gets its own
// tmux session, on the socket of the covibe user that owns it: a socket is one
// tmux server, so it is also the isolation boundary between users.
func (tmux) Command(s Spec) ([]string, error) {
	if s.Session == "" {
		return nil, fmt.Errorf("tmux: session name required")
	}
	argv := append(tmuxArgv(s.Socket), "new-session", "-d", "-s", s.Session, "-n", s.Name)
	// Stamp the covibe id into the tmux session environment: from inside a pane
	// (or `show-environment`) that is the only way back from "which tmux session
	// is this" to "which covibe session is this".
	if s.ID != "" {
		argv = append(argv, "-e", "COVIBE_SESSION_ID="+s.ID)
	}
	if s.Dir != "" {
		argv = append(argv, "-c", s.Dir)
	}
	argv = append(argv, "--")
	return append(argv, s.InnerArgv...), nil
}

// tmuxArgv is the tmux invocation prefix, pinned to a socket when one is set.
// An empty socket means the user's default server, which is what a bare
// `covibe start` on a developer machine wants.
func tmuxArgv(socket string) []string {
	if socket == "" {
		return []string{"tmux"}
	}
	return []string{"tmux", "-S", socket}
}

// Kill terminates the tmux session.
func (tmux) Kill(s Spec) error {
	if s.Session == "" {
		return nil
	}
	argv := append(tmuxArgv(s.Socket), "kill-session", "-t", "="+s.Session)
	// #nosec G204 -- fixed argv; operator socket path and session name, no shell
	_ = exec.Command(argv[0], argv[1:]...).Run()
	return nil
}

func (t tmux) Launch(s Spec) error {
	argv, err := t.Command(s)
	if err != nil {
		return err
	}
	return run(argv)
}
