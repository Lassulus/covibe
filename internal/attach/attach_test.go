package attach

import (
	"bytes"
	"context"
	"io"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lassulus/covibe/internal/access"
	"github.com/lassulus/covibe/internal/dashboard"
	"github.com/lassulus/covibe/internal/spool"
)

// An id or tmux session name is unique; a display name shared by two sessions is
// refused, because attaching the wrong shell is worse than an error.
func TestResolve(t *testing.T) {
	sessions := []Session{
		{ID: "a1", Name: "proj", MuxSession: "proj-a1", Dir: "/one"},
		{ID: "b2", Name: "proj", MuxSession: "proj-b2", Dir: "/two"},
		{ID: "c3", Name: "solo", MuxSession: "solo-c3"},
	}
	if got, err := Resolve(sessions, "b2"); err != nil || got.Dir != "/two" {
		t.Fatalf("by id: %+v %v", got, err)
	}
	if got, err := Resolve(sessions, "proj-a1"); err != nil || got.Dir != "/one" {
		t.Fatalf("by tmux session: %+v %v", got, err)
	}
	if got, err := Resolve(sessions, "solo"); err != nil || got.ID != "c3" {
		t.Fatalf("by unique name: %+v %v", got, err)
	}
	err := func() error { _, err := Resolve(sessions, "proj"); return err }()
	if err == nil {
		t.Fatal("ambiguous name accepted")
	}
	for _, want := range []string{"a1", "b2", "/one", "/two"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("ambiguity error should list candidates, got: %v", err)
		}
	}
	if _, err := Resolve(sessions, "nope"); err == nil {
		t.Fatal("unknown target accepted")
	}
}

// liveDashboard serves one real tmux session, owned by alice and reachable with
// her user key.
func liveDashboard(t *testing.T, paneCmd string) (base, token string) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not in PATH")
	}
	store, err := spool.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(t.TempDir(), "t.sock")
	const muxSession = "remote-probe"
	if out, err := exec.Command("tmux", "-S", sock, "new-session", "-d", "-s", muxSession, "-x", "90", "-y", "24", paneCmd).CombinedOutput(); err != nil {
		t.Fatalf("new-session: %v: %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("tmux", "-S", sock, "kill-server").Run() })

	rec := &spool.Record{
		ID: "sess-1", Name: "remote", Dir: "/tmp", Status: spool.StatusLive, PID: os.Getpid(),
		MuxSession: muxSession, MuxSocket: sock, StartedAt: time.Now(),
	}
	if err := store.Save(rec); err != nil {
		t.Fatal(err)
	}
	acl, _ := access.Open("")
	if err := acl.SetOwner(rec.ID, "alice@example.com"); err != nil {
		t.Fatal(err)
	}
	var keys dashboard.APIKeys
	if err := keys.AddUserKeys("alice@example.com:alice-token"); err != nil {
		t.Fatal(err)
	}
	// A real authenticator with an unreachable issuer: no cookie means
	// unauthenticated, so the only way in is the user key under test. NoAuth
	// would make every caller an admin and prove nothing.
	auth, err := dashboard.NewAuthenticator(context.Background(), dashboard.OIDCConfig{
		Issuer:       "http://127.0.0.1:9/",
		ClientID:     "covibe",
		RedirectURL:  "http://127.0.0.1:9/auth/callback",
		CookieSecret: []byte("attach-test-secret"),
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(dashboard.NewServer(dashboard.Config{
		Store:   store,
		Access:  acl,
		Auth:    auth,
		APIKeys: keys,
	}).Handler())
	t.Cleanup(srv.Close)
	return srv.URL, "alice-token"
}

// The point of the client: a terminal on another machine, reached over the same
// socket the browser uses, with keystrokes going back.
func TestRunAttachesAndTypes(t *testing.T) {
	base, token := liveDashboard(t, "cat")

	in, keys := io.Pipe()
	var out safeBuffer
	var notices []string
	var mu sync.Mutex

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{
			Base: base, Token: token, Target: "remote",
			In: in, Out: &out,
			Size:   func() (int, int) { return 90, 24 },
			Notify: func(s string) { mu.Lock(); notices = append(notices, s); mu.Unlock() },
		})
	}()

	// The hello notice proves the stream is up before anything is typed.
	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(notices) > 0
	}, "no hello notice")
	mu.Lock()
	hello := notices[0]
	mu.Unlock()
	if !strings.Contains(hello, "attached") || !strings.Contains(hello, "Ctrl-]") {
		t.Fatalf("hello notice: %q", hello)
	}

	if _, err := keys.Write([]byte("typed-from-another-machine\n")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return strings.Contains(out.String(), "typed-from-another-machine") }, "input never echoed back")

	// Ctrl-] detaches without touching the far side.
	if _, err := keys.Write([]byte{DetachKey}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("attach returned %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("detach key did not end the attach")
	}
	mu.Lock()
	last := notices[len(notices)-1]
	mu.Unlock()
	if last != "detached" {
		t.Fatalf("last notice = %q, want detached", last)
	}
}

// A token that cannot see the session must not be able to attach it, and the
// error has to say so rather than hanging.
func TestRunRefusesUnknownToken(t *testing.T) {
	base, _ := liveDashboard(t, "cat")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	err := Run(ctx, Options{Base: base, Token: "not-a-key", Target: "remote", In: strings.NewReader(""), Out: io.Discard})
	if err == nil {
		t.Fatal("attach with a bogus token succeeded")
	}
	if !strings.Contains(err.Error(), "token") && !strings.Contains(err.Error(), "no session") {
		t.Fatalf("unhelpful error: %v", err)
	}
}

// safeBuffer is a Writer the test goroutine can read while Run writes to it.
type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *safeBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *safeBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal(msg)
}
