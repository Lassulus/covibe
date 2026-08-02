package tmuxctl

import (
	"bytes"
	"context"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// tmux escapes non-printable output bytes as a backslash plus three octal
// digits; everything else is literal. Getting this wrong corrupts every escape
// sequence a TUI emits, so it is worth pinning.
func TestUnescape(t *testing.T) {
	cases := map[string]string{
		"plain":                      "plain",
		"\\033[31mred\\033[0m":       "\x1b[31mred\x1b[0m",
		"a\\015\\012b":               "a\r\nb",
		"back\\\\slash":              "back\\\\slash", // \\ is not an octal triple: kept verbatim
		"trailing\\03":               "trailing\\03",  // truncated escape stays literal
		"\\000null":                  "\x00null",
		"mixed \\177 and text \\007": "mixed \x7f and text \a",
	}
	for in, want := range cases {
		if got := string(unescape(in)); got != want {
			t.Errorf("unescape(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseOutputForms(t *testing.T) {
	data, ok := parseOutput("%output %3 hi\\015\\012")
	if !ok || string(data) != "hi\r\n" {
		t.Fatalf("parseOutput = %q, %v", data, ok)
	}
	// The extended form carries an age field before a " : " separator.
	data, ok = parseExtendedOutput("%extended-output %3 120 : lag\\012")
	if !ok || string(data) != "lag\n" {
		t.Fatalf("parseExtendedOutput = %q, %v", data, ok)
	}
	if _, ok := parseOutput("%output %3"); ok {
		t.Fatal("truncated output notification accepted")
	}
}

// Key names go to send-keys as a bare argument, so anything that could pass as
// an option or a second argument must be refused.
func TestSafeKeyName(t *testing.T) {
	for _, ok := range []string{"C-c", "Enter", "Up", "PageDown", "F5", "M-x"} {
		if !safeKeyName(ok) {
			t.Errorf("%q rejected", ok)
		}
	}
	for _, bad := range []string{"", "-X", "a b", "kill-session; ls", "C-c\n", "'", string(make([]byte, 40))} {
		if safeKeyName(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
}

// liveServer starts a private tmux server on a temp socket running cmd in one
// session. `cat` echoes what we type (handy for stream tests); `sh` lets a test
// have the program itself emit escape sequences, which is what a TUI does.
func liveServer(t *testing.T, cmd string) (Server, string) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not in PATH")
	}
	// Socket paths live in sun_path (~108 bytes), and t.TempDir() is already
	// long; keep the name short.
	srv := Server{Socket: filepath.Join(t.TempDir(), "t.sock")}
	const session = "probe"
	if _, err := srv.run("new-session", "-d", "-s", session, "-x", "80", "-y", "24", cmd); err != nil {
		t.Fatalf("new-session: %v", err)
	}
	t.Cleanup(func() { _, _ = srv.run("kill-server") })
	deadline := time.Now().Add(3 * time.Second)
	for !srv.HasSession(session) {
		if time.Now().After(deadline) {
			t.Fatal("session never appeared")
		}
		time.Sleep(20 * time.Millisecond)
	}
	return srv, session
}

// waitForScreen polls the rendered grid until it contains want.
func waitForScreen(t *testing.T, srv Server, session string, o CaptureOpts, want string) []byte {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var last []byte
	for time.Now().Before(deadline) {
		out, err := srv.Capture(session, o)
		if err != nil {
			t.Fatal(err)
		}
		last = out
		if bytes.Contains(out, []byte(want)) {
			return out
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("screen never contained %q, last capture:\n%s", want, last)
	return nil
}

// The point of using tmux is that it is the emulator: a capture returns the
// rendered grid, so text a program overwrote in place is gone. A byte log of
// everything the program wrote would still contain it.
//
// The pane program prints the sequences itself and then blocks in `cat`: typing
// them would put the shell's echo of the command line on screen too, and the
// literal text of an escape sequence is not what we are asserting about.
func TestCaptureReturnsRenderedGrid(t *testing.T) {
	srv, session := liveServer(t, `printf 'AAAAAAAAAAAA\n'; printf '\033[1A\033[5GBBB\n'; cat`)
	screen := waitForScreen(t, srv, session, CaptureOpts{}, "BBB")
	if !bytes.Contains(screen, []byte("AAAABBBAAAAA")) {
		t.Fatalf("overwrite not applied to the grid:\n%s", screen)
	}
	if bytes.Contains(screen, []byte("\x1b[")) {
		t.Fatalf("plain capture leaked escapes:\n%q", screen)
	}
}

// With -e the capture keeps styling, which is what the browser gets as its
// first frame.
func TestCaptureKeepsEscapesWhenAsked(t *testing.T) {
	srv, session := liveServer(t, `printf '\033[31mRED\033[0m\n'; cat`)
	waitForScreen(t, srv, session, CaptureOpts{Escapes: true}, "\x1b[31mRED")
}

// A control-mode client must stream what the pane produces and deliver what we
// send, both over the one pipe.
func TestAttachStreamsAndSends(t *testing.T) {
	srv, session := liveServer(t, "cat")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	c, err := srv.Attach(ctx, session, 100, 30)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if err := c.Send([]byte("ping-through-control\n")); err != nil {
		t.Fatal(err)
	}
	var seen []byte
	timeout := time.After(10 * time.Second)
	for {
		select {
		case data, open := <-c.Output():
			if !open {
				t.Fatalf("stream closed early, saw %q", seen)
			}
			seen = append(seen, data...)
			if bytes.Contains(seen, []byte("ping-through-control")) {
				return
			}
		case <-timeout:
			t.Fatalf("no echo within timeout, saw %q", seen)
		}
	}
}

// Killing the session must end the stream, so a browser socket closes instead
// of hanging on a dead session.
func TestOutputClosesWhenSessionDies(t *testing.T) {
	srv, session := liveServer(t, "cat")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	c, err := srv.Attach(ctx, session, 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// Drain until the channel closes.
	done := make(chan struct{})
	go func() {
		for range c.Output() {
		}
		close(done)
	}()
	if err := srv.Kill(session); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("output channel stayed open after kill")
	}
}

// Input is bounded so one request cannot build an unreasonable argv.
func TestSendRejectsOversizedInput(t *testing.T) {
	srv := Server{Socket: "/nonexistent/tmux.sock"}
	if err := srv.SendKeys("s", make([]byte, maxInput+1)); err == nil {
		t.Fatal("oversized input accepted")
	}
	if err := srv.SendKeys("s", nil); err != nil {
		t.Fatalf("empty input should be a no-op: %v", err)
	}
}
