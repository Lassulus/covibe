package session

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

// The sidecar is a separate binary (Rust, so iroh is used first-party) that holds
// this session's two iroh endpoints and pumps bytes between them and the unix
// sockets served in p2pterm.go. It is spawned as a child of the wrapper, so it
// dies with the session: the endpoints go with it, and the tickets that named
// them stop resolving. That is the revocation model — a ticket lives exactly as
// long as the session it points at.
//
// Nothing here may take the session down. A missing or broken sidecar means the
// session has no peer-to-peer terminal, exactly as a missing dashboard means it
// has no web terminal.

// p2pStartTimeout bounds how long the wrapper waits for the sidecar's ticket
// line. It is generous because the sidecar deliberately waits for its relay
// address before printing: a ticket without a relay URL stops resolving the
// moment the host's addresses change.
const p2pStartTimeout = 45 * time.Second

// Tickets are the capabilities for one session's terminal. The read-write and
// read-only tickets name different iroh endpoints — different keypairs — so
// holding the read-only one cannot be escalated by asking for another protocol.
type Tickets struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	RW   string `json:"rw"`
	RO   string `json:"ro"`
}

// p2pSidecar is a running sidecar plus the local plumbing behind it.
type p2pSidecar struct {
	Tickets Tickets

	cancel  func()
	cmd     *exec.Cmd
	srv     *termServer
	paths   []string
	stopped sync.Once
}

// startP2P serves src to a freshly spawned sidecar and returns it with the
// tickets it minted. allowWrite is false for a view-only session, in which case
// even the read-write endpoint refuses input — the ticket pair still exists so
// the two roles keep the same shape, but neither can type. The caller must call
// stop.
func startP2P(parent context.Context, cfg Config, src termSource, ptmx *os.File, allowWrite bool) (*p2pSidecar, error) {
	dir, err := p2pDir(cfg.StateDir)
	if err != nil {
		return nil, err
	}
	short := shortSessionID(cfg.ID)
	rwPath := filepath.Join(dir, short+"-rw.sock")
	roPath := filepath.Join(dir, short+"-ro.sock")

	ctx, cancel := context.WithCancel(parent)
	s := &p2pSidecar{cancel: cancel, paths: []string{rwPath, roPath}}

	srv := newTermServer(src, ptmx)
	if err := srv.start(ctx); err != nil {
		cancel()
		return nil, fmt.Errorf("attach terminal: %w", err)
	}
	s.srv = srv

	lnRW, err := listenUnix(rwPath)
	if err != nil {
		s.stop()
		return nil, err
	}
	lnRO, err := listenUnix(roPath)
	if err != nil {
		_ = lnRW.Close()
		s.stop()
		return nil, err
	}
	go func() { _ = srv.serve(ctx, lnRW, allowWrite) }()
	go func() { _ = srv.serve(ctx, lnRO, false) }()

	argv := []string{"host", "--rw-sock", rwPath, "--ro-sock", roPath}
	if cfg.P2PRelay != "" {
		argv = append(argv, "--relay", cfg.P2PRelay)
	}
	cmd := exec.CommandContext(ctx, cfg.P2PBin, argv...) // #nosec G204 -- operator-configured binary, fixed argv
	dieWithParent(cmd)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		s.stop()
		return nil, err
	}
	// The wrapper's stdout is the user's terminal (omp draws there), so the
	// sidecar's diagnostics must not land on it. They are kept instead, to be
	// reported if the sidecar never gets as far as printing its tickets.
	var errbuf lockedBuffer
	cmd.Stderr = &errbuf
	if err := cmd.Start(); err != nil {
		s.stop()
		return nil, fmt.Errorf("start %s: %w", cfg.P2PBin, err)
	}
	s.cmd = cmd

	tk, err := readTickets(ctx, stdout)
	if err != nil {
		s.stop()
		if msg := errbuf.String(); msg != "" {
			return nil, fmt.Errorf("%w: %s", err, firstLines(msg, 3))
		}
		return nil, err
	}
	tk.ID, tk.Name = cfg.ID, cfg.Name
	s.Tickets = tk

	// Drain the rest of the sidecar's stdout so it can never block on a full
	// pipe. It is not supposed to write anything else.
	go func() { _, _ = io.Copy(io.Discard, stdout) }()

	if err := writeTickets(dir, short, tk); err != nil {
		// The session is fine without the file; only recovery of a lost ticket
		// is lost with it.
		fmt.Fprintf(os.Stderr, "covibe: could not record tickets: %v\n", err)
	}
	return s, nil
}

// stop tears down the sidecar and the sockets it was serving.
func (s *p2pSidecar) stop() {
	s.stopped.Do(func() {
		s.cancel()
		if s.srv != nil {
			s.srv.stop()
		}
		if s.cmd != nil {
			_ = s.cmd.Wait()
		}
		for _, p := range s.paths {
			_ = os.Remove(p)
		}
	})
}

// readTickets reads the sidecar's single stdout line.
func readTickets(ctx context.Context, r io.Reader) (Tickets, error) {
	type result struct {
		tk  Tickets
		err error
	}
	ch := make(chan result, 1)
	go func() {
		br := bufio.NewReader(io.LimitReader(r, 8<<10))
		line, err := br.ReadBytes('\n')
		if err != nil && len(line) == 0 {
			ch <- result{err: fmt.Errorf("sidecar produced no tickets: %w", err)}
			return
		}
		var tk Tickets
		if err := json.Unmarshal(bytes.TrimSpace(line), &tk); err != nil {
			ch <- result{err: fmt.Errorf("sidecar ticket line: %w", err)}
			return
		}
		if tk.RW == "" || tk.RO == "" {
			ch <- result{err: fmt.Errorf("sidecar returned an incomplete ticket pair")}
			return
		}
		if tk.RW == tk.RO {
			// Identical tickets would mean one keypair serving both modes, which
			// makes read-only unenforceable. Refuse rather than advertise it.
			ch <- result{err: fmt.Errorf("sidecar returned the same ticket for both modes")}
			return
		}
		ch <- result{tk: tk}
	}()
	select {
	case <-ctx.Done():
		return Tickets{}, ctx.Err()
	case <-time.After(p2pStartTimeout):
		return Tickets{}, fmt.Errorf("sidecar did not report its tickets within %s", p2pStartTimeout)
	case res := <-ch:
		return res.tk, res.err
	}
}

// p2pDir is the private directory holding a session's sockets and tickets. Kept
// directly under the state dir so socket paths stay well inside sun_path's ~108
// bytes.
func p2pDir(stateDir string) (string, error) {
	if stateDir == "" {
		stateDir = os.TempDir()
	}
	dir := filepath.Join(stateDir, "p2p")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create p2p dir: %w", err)
	}
	return dir, nil
}

// writeTickets records the pair so a holder who lost their scrollback can get it
// back. It stays local, and 0600: these are capabilities, and deliberately never
// sent to the dashboard — a dashboard holding every session's write ticket would
// be exactly the central point of trust this path exists to remove.
func writeTickets(dir, short string, tk Tickets) error {
	b, err := json.MarshalIndent(tk, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, short+".json"), append(b, '\n'), 0o600)
}

// LoadTickets returns the recorded tickets under stateDir, newest first by name
// match. target may be a session id, a session name, or empty for all.
func LoadTickets(stateDir, target string) ([]Tickets, error) {
	dir, err := p2pDir(stateDir)
	if err != nil {
		return nil, err
	}
	entries, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	var out []Tickets
	for _, p := range entries {
		b, err := os.ReadFile(p) // #nosec G304 -- fixed glob under the state dir
		if err != nil {
			continue
		}
		var tk Tickets
		if json.Unmarshal(b, &tk) != nil {
			continue
		}
		if target != "" && tk.ID != target && tk.Name != target {
			continue
		}
		out = append(out, tk)
	}
	return out, nil
}

// shortSessionID keeps a socket file name short without losing the part of an id
// that varies; ids are minted with the entropy at the end.
func shortSessionID(id string) string {
	if len(id) > 12 {
		return id[len(id)-12:]
	}
	if id == "" {
		return "session"
	}
	return id
}

func firstLines(s string, n int) string {
	lines := bytes.SplitN([]byte(s), []byte("\n"), n+1)
	if len(lines) > n {
		lines = lines[:n]
	}
	return string(bytes.TrimSpace(bytes.Join(lines, []byte("; "))))
}

// lockedBuffer collects the sidecar's stderr without racing cmd.Wait.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.buf.Len() > 8<<10 {
		return len(p), nil
	}
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// PrintTickets tells the operator what to hand out. It goes to stderr because
// stdout is the session's terminal.
func PrintTickets(w io.Writer, tk Tickets, allowWrite bool) {
	rw := "read-write"
	if !allowWrite {
		rw = "read-write endpoint (session is view-only, so it cannot type either)"
	}
	fmt.Fprintf(w, "\n  covibe: peer-to-peer terminal for %q\n", tk.Name)
	fmt.Fprintf(w, "    %s\n      covibe attach --ticket %s\n", rw, tk.RW)
	fmt.Fprintf(w, "    read-only\n      covibe attach --ticket %s\n\n", tk.RO)
}
