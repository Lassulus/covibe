// Package mux launches a covibe session inside tmux. tmux is the only backend:
// it is the terminal emulator the dashboard reads (capture-pane returns a
// rendered grid), the thing control mode streams to the browser, and a server
// that outlives the dashboard, so a deploy does not kill anyone's session.
package mux

import (
	"fmt"
	"os/exec"
	"strings"

	"github.com/lassulus/covibe/internal/tmuxctl"
)

// Spec describes a session to launch.
type Spec struct {
	ID      string
	Name    string // window name
	Dir     string // working directory
	Session string // tmux session name
	// Socket is the tmux server socket to launch on. Each covibe session gets
	// its own: a shell in a pane can reach every session on its server through
	// $TMUX, so one server per session is what keeps sharing one session from
	// sharing all of them.
	Socket    string
	InnerArgv []string // the full `covibe session ...` argv to run in the pane
}

// Command builds the tmux invocation that creates the session, detached, with
// the pane command run directly (no shell, so no quoting games).
func Command(s Spec) ([]string, error) {
	if s.Session == "" {
		return nil, fmt.Errorf("tmux: session name required")
	}
	argv := append(tmuxArgv(s.Socket), "new-session", "-d", "-s", s.Session, "-n", s.Name)
	// Stamp the covibe id into the session environment: from inside a pane that
	// is the way back from "which tmux session is this" to "which covibe
	// session is this".
	if s.ID != "" {
		argv = append(argv, "-e", "COVIBE_SESSION_ID="+s.ID)
	}
	if s.Dir != "" {
		argv = append(argv, "-c", s.Dir)
	}
	argv = append(argv, "--")
	return append(argv, s.InnerArgv...), nil
}

// Launch creates the session and tags it with the covibe id.
func Launch(s Spec) error {
	argv, err := Command(s)
	if err != nil {
		return err
	}
	if err := run(argv); err != nil {
		return err
	}
	// The pane environment carries the id too, but an option is queryable with
	// a format string, which is what lets covibe find its session again after
	// someone renames it from inside.
	if s.ID != "" {
		// "=name:" and not "=name": set-option parses its target as a pane,
		// where a bare "=name" is rejected ("no such session"). The trailing
		// colon makes it the session's current pane, and the option still lands
		// on the session.
		tag := append(tmuxArgv(s.Socket), "set-option", "-t", "="+s.Session+":", tmuxctl.IDOption, s.ID)
		if err := run(tag); err != nil {
			return fmt.Errorf("tag session with covibe id: %w", err)
		}
	}
	return nil
}

// Kill tears down a session, best effort: it runs after the session's process
// has already been signalled.
func Kill(socket, session string) {
	if session == "" {
		return
	}
	argv := append(tmuxArgv(socket), "kill-session", "-t", "="+session)
	// #nosec G204 -- fixed argv; operator socket path and session name, no shell
	_ = exec.Command(argv[0], argv[1:]...).Run()
}

// tmuxArgv is the tmux invocation prefix, pinned to a socket when one is set.
// An empty socket means the user's default server, which is what a bare
// `covibe session` on a developer machine gets.
func tmuxArgv(socket string) []string {
	if socket == "" {
		return []string{"tmux"}
	}
	return []string{"tmux", "-S", socket}
}

func run(argv []string) error {
	if len(argv) == 0 {
		return fmt.Errorf("empty command")
	}
	cmd := exec.Command(argv[0], argv[1:]...) // #nosec G204 -- argv built here from covibe's own records; no shell
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w: %s", argv[0], err, out)
	}
	return nil
}

// SessionName derives a unique, readable tmux session name for one covibe
// session: "<name-or-base>-<shortid>". The short id keeps sessions distinct
// even when two share a display name.
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
