// Package launch builds and runs the multiplexer command that opens an omp
// covibe session in a tab/window. It is shared by the `covibe start` CLI and
// the dashboard's web-initiated session creation so both take the exact same
// path.
package launch

import (
	"os"

	"github.com/lassulus/covibe/internal/mux"
)

// Options describes a session to launch.
type Options struct {
	Self       string // covibe executable path (resolved from os.Executable if empty)
	Name       string
	Dir        string
	Mux        string // "zellij" | "tmux"
	MuxSession string
	Relay      string
	WebURL     string
	Omp        string
	AutoCollab string // "", "full", "view"
	StateDir   string
}

// inner builds the `covibe session ...` argv that the mux runs in the pane.
func (o Options) inner() ([]string, error) {
	self := o.Self
	if self == "" {
		exe, err := os.Executable()
		if err != nil {
			return nil, err
		}
		self = exe
	}
	auto := o.AutoCollab
	if auto == "" {
		auto = "full"
	}
	omp := o.Omp
	if omp == "" {
		omp = "omp"
	}
	argv := []string{
		self, "session",
		"--name", o.Name,
		"--dir", o.Dir,
		"--omp", omp,
		"--auto-collab", auto,
		"--mux", o.Mux,
		"--mux-session", o.MuxSession,
	}
	if o.Relay != "" {
		argv = append(argv, "--relay", o.Relay)
	}
	if o.WebURL != "" {
		argv = append(argv, "--web", o.WebURL)
	}
	if o.StateDir != "" {
		argv = append(argv, "--state-dir", o.StateDir)
	}
	return argv, nil
}

func (o Options) spec() (mux.Launcher, mux.Spec, error) {
	l, err := mux.For(o.Mux)
	if err != nil {
		return nil, mux.Spec{}, err
	}
	inner, err := o.inner()
	if err != nil {
		return nil, mux.Spec{}, err
	}
	return l, mux.Spec{Name: o.Name, Dir: o.Dir, Session: o.MuxSession, InnerArgv: inner}, nil
}

// Command returns the multiplexer argv without running it (dry-run).
func Command(o Options) ([]string, error) {
	l, spec, err := o.spec()
	if err != nil {
		return nil, err
	}
	return l.Command(spec)
}

// Launch ensures the multiplexer session exists, then opens the tab.
func Launch(o Options) error {
	l, spec, err := o.spec()
	if err != nil {
		return err
	}
	if err := l.Ensure(o.MuxSession); err != nil {
		return err
	}
	return l.Launch(spec)
}
