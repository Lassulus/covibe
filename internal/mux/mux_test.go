package mux

import (
	"reflect"
	"testing"
)

func TestZellijCommand(t *testing.T) {
	z := zellij{}
	got, err := z.Command(Spec{
		Session:   "covibe",
		Name:      "api",
		Dir:       "/home/x/proj",
		InnerArgv: []string{"/usr/bin/covibe", "session", "--name", "api"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"zellij", "--session", "covibe", "run",
		"--cwd", "/home/x/proj", "--name", "api",
		"--close-on-exit", "--",
		"/usr/bin/covibe", "session", "--name", "api",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("\n got %v\nwant %v", got, want)
	}
}

func TestZellijNeedsSession(t *testing.T) {
	if _, err := (zellij{}).Command(Spec{Name: "x"}); err == nil {
		t.Fatal("expected error without session name")
	}
}

func TestTmuxCommandNewSession(t *testing.T) {
	// Use a session name that will not exist, so we get the create path.
	got, err := (tmux{}).Command(Spec{
		Session:   "covibe-nonexistent-test-xyzzy",
		Name:      "api",
		Dir:       "/tmp/p",
		InnerArgv: []string{"covibe", "session"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"tmux", "new-session", "-d", "-s", "covibe-nonexistent-test-xyzzy",
		"-n", "api", "-c", "/tmp/p", "--", "covibe", "session",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("\n got %v\nwant %v", got, want)
	}
}

func TestForUnknown(t *testing.T) {
	if _, err := For("screen"); err == nil {
		t.Fatal("expected error for unknown mux")
	}
	if _, err := For(""); err != nil {
		t.Fatalf("empty should default to zellij: %v", err)
	}
}
