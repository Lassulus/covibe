package mux

import (
	"reflect"
	"strings"
	"testing"
)

func TestCommandNewSession(t *testing.T) {
	got, err := Command(Spec{
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

func TestCommandNeedsSession(t *testing.T) {
	if _, err := Command(Spec{Name: "x"}); err == nil {
		t.Fatal("expected error without session name")
	}
}

// Each session gets its own server, and the covibe id rides in the session
// environment: it is the way back from a pane to the session it belongs to.
func TestCommandPinsSocketAndStampsID(t *testing.T) {
	got, err := Command(Spec{
		ID:        "covibe-85de7e348a172088",
		Session:   "proj-8a172088",
		Name:      "proj",
		Socket:    "/run/covibe/tmux/alice-8a172088.sock",
		InnerArgv: []string{"covibe", "session"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"tmux", "-S", "/run/covibe/tmux/alice-8a172088.sock", "new-session", "-d",
		"-s", "proj-8a172088", "-n", "proj",
		"-e", "COVIBE_SESSION_ID=covibe-85de7e348a172088",
		"--", "covibe", "session",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("\n got %v\nwant %v", got, want)
	}
}

func TestSessionName(t *testing.T) {
	// Distinct ids yield distinct names even for the same display name, so two
	// same-named covibe sessions never collide into one tmux session.
	a := SessionName("covibe", "my proj", "covibe-85de7e348a172088")
	b := SessionName("covibe", "my proj", "covibe-11112222")
	if a == b {
		t.Fatalf("same session name for different ids: %q", a)
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
