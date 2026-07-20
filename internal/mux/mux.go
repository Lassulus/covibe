// Package mux launches a covibe session command inside a terminal
// multiplexer tab/window. zellij is the primary backend; tmux is provided as a
// second adapter (and is what the end-to-end test drives).
package mux

import (
	"fmt"
	"os/exec"
	"strings"
)

// Spec describes a session to launch.
type Spec struct {
	ID        string
	Name      string   // tab/window name
	Dir       string   // working directory
	Session   string   // multiplexer session name
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
	cmd := exec.Command(argv[0], argv[1:]...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w: %s", argv[0], err, out)
	}
	return nil
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

// Ensure creates a backgrounded zellij session if it does not already exist,
// so `run` has a session to target without a client being attached.
func (zellij) Ensure(session string) error {
	if session == "" {
		return fmt.Errorf("zellij: session name required")
	}
	if zellijHasSession(session) {
		return nil
	}
	// --create-background starts the session's server without attaching a client.
	return run([]string{"zellij", "attach", "--create-background", session})
}

func zellijHasSession(name string) bool {
	// `list-sessions -ns` prints one bare session name per line (no ANSI, no
	// "current" markers). Match an exact line.
	out, err := exec.Command("zellij", "list-sessions", "-ns").Output()
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(line) == name {
			return true
		}
	}
	return false
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

// Command creates the session with the pane command if it does not yet exist,
// otherwise opens a new window in it. tmux runs the argv as the pane's process
// directly (no shell), so no quoting games are needed.
func (tmux) Command(s Spec) ([]string, error) {
	if s.Session == "" {
		return nil, fmt.Errorf("tmux: session name required")
	}
	if !tmuxHasSession(s.Session) {
		argv := []string{"tmux", "new-session", "-d", "-s", s.Session, "-n", s.Name}
		if s.Dir != "" {
			argv = append(argv, "-c", s.Dir)
		}
		argv = append(argv, "--")
		return append(argv, s.InnerArgv...), nil
	}
	argv := []string{"tmux", "new-window", "-t", s.Session, "-n", s.Name}
	if s.Dir != "" {
		argv = append(argv, "-c", s.Dir)
	}
	argv = append(argv, "--")
	return append(argv, s.InnerArgv...), nil
}

func (t tmux) Launch(s Spec) error {
	argv, err := t.Command(s)
	if err != nil {
		return err
	}
	return run(argv)
}

func tmuxHasSession(name string) bool {
	return exec.Command("tmux", "has-session", "-t", "="+name).Run() == nil
}
