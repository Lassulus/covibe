package mux

import (
	"reflect"
	"strings"
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

func TestSessionName(t *testing.T) {
	// Distinct ids yield distinct names even for the same display name, so two
	// same-named covibe sessions never collide into one mux session.
	a := SessionName("covibe", "my proj", "covibe-85de7e348a172088")
	b := SessionName("covibe", "my proj", "covibe-11112222")
	if a == b {
		t.Fatalf("same mux name for different ids: %q", a)
	}
	// Sanitized (no spaces/colons/dots) and carries the readable display name.
	if strings.ContainsAny(a, " :.") {
		t.Fatalf("unsanitized session name: %q", a)
	}
	if !strings.HasPrefix(a, "my-proj-") {
		t.Fatalf("want readable prefix, got %q", a)
	}
	// Empty name falls back to the base.
	if got := SessionName("covibe", "", "abcd1234"); got != "covibe-abcd1234" {
		t.Fatalf("fallback base: got %q", got)
	}
}

// One socket holds every session of one covibe user, so the tmux session has to
// carry the covibe id: it is the only way back from a pane to the session it
// belongs to when two sessions share a display name.
func TestTmuxCommandStampsSessionID(t *testing.T) {
	got, err := (tmux{}).Command(Spec{
		ID:        "covibe-85de7e348a172088",
		Session:   "proj-8a172088",
		Name:      "proj",
		Socket:    "/run/covibe/tmux/alice.sock",
		InnerArgv: []string{"covibe", "session"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"tmux", "-S", "/run/covibe/tmux/alice.sock", "new-session", "-d",
		"-s", "proj-8a172088", "-n", "proj",
		"-e", "COVIBE_SESSION_ID=covibe-85de7e348a172088",
		"--", "covibe", "session",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("\n got %v\nwant %v", got, want)
	}
}
