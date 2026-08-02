// Package tmuxctl drives a tmux server over its control mode and one-shot
// commands. tmux is covibe's terminal backend for two reasons: its server
// outlives the dashboard (a deploy does not kill anyone's session), and it is
// the terminal emulator — `capture-pane` returns the rendered grid, so covibe
// needs no VT emulator of its own to read a screen.
//
// Control mode is a documented protocol (tmux(1), CONTROL MODE): commands go in
// one per line, each answered by exactly one %begin/%end (or %error) block, and
// pane output arrives asynchronously as %output notifications. Replies are
// matched on the command number in the guard line, never on position: pane
// content is written into blocks unescaped and can forge a guard line.
package tmuxctl

import (
	"bufio"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Server is one tmux server, addressed by its socket path. An empty socket uses
// tmux's default server.
type Server struct {
	Socket string
	// Bin overrides the tmux binary (tests, unusual deployments).
	Bin string
}

func (s Server) argv(args ...string) []string {
	bin := s.Bin
	if bin == "" {
		bin = "tmux"
	}
	out := []string{bin}
	if s.Socket != "" {
		out = append(out, "-S", s.Socket)
	}
	return append(out, args...)
}

// oneShotTimeout bounds a non-streaming tmux command. A live server answers in
// milliseconds; a hung one must not wedge an HTTP handler.
const oneShotTimeout = 5 * time.Second

// Run executes a one-shot tmux command on this server and returns stdout.
func (s Server) Run(args ...string) ([]byte, error) { return s.run(args...) }

// run executes a one-shot tmux command and returns stdout.
func (s Server) run(args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), oneShotTimeout)
	defer cancel()
	argv := s.argv(args...)
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) // #nosec G204 -- argv is built here; socket path and target come from covibe's own records
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			return nil, fmt.Errorf("tmux %s: %w: %s", args[0], err, strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, fmt.Errorf("tmux %s: %w", args[0], err)
	}
	return out, nil
}

// Target names a tmux session exactly. The "=" prefix stops tmux from resolving
// a prefix match to some other session.
func Target(session string) string { return "=" + session }

// paneTarget names the active pane of a session exactly. A bare "=name" is a
// session target and tmux rejects it where a pane is expected ("can't find
// pane"); the trailing colon makes it the session's current window and pane.
func paneTarget(session string) string { return "=" + session + ":" }

// HasSession reports whether the named session exists on this server.
func (s Server) HasSession(session string) bool {
	_, err := s.run("has-session", "-t", Target(session))
	return err == nil
}

// IDOption is the tmux user option covibe stamps a session's id into at launch
// (see internal/mux). It is what makes targeting survive a rename: a user who
// runs `tmux rename-session` inside their own session would otherwise strand
// the dashboard, which only knows the name it created.
const IDOption = "@covibe_id"

// SessionFor returns the name of the session tagged with covibeID, falling back
// to want when nothing is tagged (sessions from before the tag, or a tmux too
// old to have kept it).
func (s Server) SessionFor(covibeID, want string) string {
	if covibeID == "" {
		return want
	}
	out, err := s.run("list-sessions", "-F", "#{session_name}\t#{"+IDOption+"}")
	if err != nil {
		return want
	}
	for _, line := range strings.Split(string(out), "\n") {
		name, id, ok := strings.Cut(strings.TrimRight(line, "\r"), "\t")
		if ok && id == covibeID && name != "" {
			return name
		}
	}
	return want
}

// CaptureOpts selects what a screen read returns.
type CaptureOpts struct {
	Escapes bool // keep SGR sequences (colour/attributes)
	Lines   int  // scrollback lines above the visible screen (0 = visible only)
	Alt     bool // capture the alternate screen instead of the visible one
}

// Capture returns the rendered contents of the session's active pane. This is
// tmux's own grid, so overwritten cells are gone and a full-screen TUI reads as
// what a human sees — unlike a byte log of everything the program ever wrote.
func (s Server) Capture(session string, o CaptureOpts) ([]byte, error) {
	// -J joins wrapped lines so a soft-wrapped command reads as one line.
	args := []string{"capture-pane", "-p", "-J", "-t", paneTarget(session)}
	if o.Escapes {
		args = append(args, "-e")
	}
	if o.Alt {
		args = append(args, "-a")
	}
	if o.Lines > 0 {
		args = append(args, "-S", "-"+strconv.Itoa(o.Lines))
	}
	return s.run(args...)
}

// maxInput caps one input write. Keystrokes are tiny; a paste is bounded so a
// single request cannot build an unreasonable argv.
const maxInput = 8 << 10

// SendKeys writes raw bytes to the session's active pane as hex, which sidesteps
// every quoting and key-name question: -H takes byte values, not key names.
func (s Server) SendKeys(session string, data []byte) error {
	if len(data) == 0 {
		return nil
	}
	if len(data) > maxInput {
		return fmt.Errorf("input too large (%d bytes, max %d)", len(data), maxInput)
	}
	args := append([]string{"send-keys", "-H", "-t", paneTarget(session)}, hexBytes(data)...)
	_, err := s.run(args...)
	return err
}

// SendNamedKey sends a tmux key name (e.g. "C-c", "Enter", "Up"), for callers
// that want to press a key rather than deliver bytes.
func (s Server) SendNamedKey(session, key string) error {
	if !safeKeyName(key) {
		return fmt.Errorf("invalid key name %q", key)
	}
	_, err := s.run("send-keys", "-t", paneTarget(session), key)
	return err
}

// safeKeyName keeps key names to what tmux's key table can contain, so a caller
// cannot smuggle an option (a leading "-") or a second argument into send-keys.
func safeKeyName(key string) bool {
	if key == "" || len(key) > 24 || key[0] == '-' {
		return false
	}
	for _, r := range key {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

func hexBytes(data []byte) []string {
	out := make([]string, len(data))
	for i, b := range data {
		out[i] = hex.EncodeToString([]byte{b})
	}
	return out
}

// Kill terminates a session on this server.
func (s Server) Kill(session string) error {
	_, err := s.run("kill-session", "-t", Target(session))
	return err
}

// Client is an attached control-mode client: one process, one session, output
// streaming on Output() and input written back through the same pipe.
type Client struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	out    chan []byte
	target string

	mu     sync.Mutex
	closed bool
	cmdNum int
}

// pauseAfter is how many seconds of backlog tmux tolerates for this client
// before pausing the pane instead of killing the connection. Without it a
// client that falls 300s behind is dropped outright ("too far behind").
const pauseAfter = 3

// outputQueue is the buffered depth of the output channel. A slow consumer
// drops frames rather than blocking the reader, which would stall tmux.
const outputQueue = 256

// Attach starts a control-mode client on the session and begins streaming. The
// returned Client is closed when ctx is cancelled, the session ends, or Close
// is called.
func (s Server) Attach(ctx context.Context, session string, cols, rows int) (*Client, error) {
	if session == "" {
		return nil, errors.New("session required")
	}
	argv := s.argv("-C", "attach-session", "-t", Target(session))
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) // #nosec G204 -- argv is built here from covibe's own records
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	// A control client is not a terminal; tmux must not inherit ours.
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	c := &Client{cmd: cmd, stdin: stdin, out: make(chan []byte, outputQueue), target: paneTarget(session)}
	go c.readLoop(stdout)

	// Size the client before anything renders, and turn on flow control.
	if cols > 0 && rows > 0 {
		_ = c.Resize(cols, rows)
	}
	_ = c.command("refresh-client -f pause-after=" + strconv.Itoa(pauseAfter))
	return c, nil
}

// Output yields terminal bytes as tmux reports them. The channel is closed when
// the client ends.
func (c *Client) Output() <-chan []byte { return c.out }

// Resize sets this client's view size. Client-driven resize is the correct one:
// it respects tmux's window-size option instead of forcing the window.
func (c *Client) Resize(cols, rows int) error {
	if cols <= 0 || rows <= 0 {
		return fmt.Errorf("bad size %dx%d", cols, rows)
	}
	return c.command(fmt.Sprintf("refresh-client -C %dx%d", cols, rows))
}

// Send writes raw bytes to the attached session's active pane.
func (c *Client) Send(data []byte) error {
	if len(data) == 0 {
		return nil
	}
	if len(data) > maxInput {
		return fmt.Errorf("input too large (%d bytes, max %d)", len(data), maxInput)
	}
	return c.command("send-keys -H -t " + c.target + " " + strings.Join(hexBytes(data), " "))
}

// command writes one command line. An empty line would detach the client, so
// blank commands are refused rather than silently sent.
func (c *Client) command(line string) error {
	line = strings.TrimRight(line, "\r\n")
	if line == "" {
		return errors.New("refusing to send an empty control command")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return errors.New("client closed")
	}
	c.cmdNum++
	_, err := io.WriteString(c.stdin, line+"\n")
	return err
}

// Close detaches the client and reaps the process.
func (c *Client) Close() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	c.mu.Unlock()
	_ = c.stdin.Close()
	if c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
	_ = c.cmd.Wait()
}

// readLoop parses control-mode notifications. Lines inside a %begin/%end block
// are command output, never terminal output, and are discarded here: the
// handlers that need command replies use one-shot commands instead, which keeps
// this loop immune to pane content that looks like a guard line.
func (c *Client) readLoop(stdout io.Reader) {
	defer close(c.out)
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	inBlock := false
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "%begin"):
			inBlock = true
		case strings.HasPrefix(line, "%end"), strings.HasPrefix(line, "%error"):
			inBlock = false
		case inBlock:
			// Command output; not terminal bytes.
		case strings.HasPrefix(line, "%output "):
			if data, ok := parseOutput(line); ok {
				c.emit(data)
			}
		case strings.HasPrefix(line, "%extended-output "):
			if data, ok := parseExtendedOutput(line); ok {
				c.emit(data)
			}
		case strings.HasPrefix(line, "%exit"), strings.HasPrefix(line, "%session-changed"):
			// %exit ends the client; a session change means the session we
			// attached to is gone (killed), so stop either way.
			if strings.HasPrefix(line, "%exit") {
				return
			}
		case strings.HasPrefix(line, "%pause"):
			// The pane was paused because we fell behind; resume it. Output
			// while paused is lost, so the caller re-snapshots.
			_ = c.command("refresh-client -A '%*:continue'")
		}
	}
}

// emit hands bytes to the consumer, dropping them if it is not keeping up:
// blocking here would stall tmux for every other client of the session.
func (c *Client) emit(data []byte) {
	select {
	case c.out <- data:
	default:
	}
}

// parseOutput decodes "%output %<pane> <escaped bytes>".
func parseOutput(line string) ([]byte, bool) {
	rest := line[len("%output "):]
	sp := strings.IndexByte(rest, ' ')
	if sp < 0 {
		return nil, false
	}
	return unescape(rest[sp+1:]), true
}

// parseExtendedOutput decodes "%extended-output %<pane> <age> : <escaped bytes>".
func parseExtendedOutput(line string) ([]byte, bool) {
	rest := line[len("%extended-output "):]
	i := strings.Index(rest, " : ")
	if i < 0 {
		return nil, false
	}
	return unescape(rest[i+len(" : "):]), true
}

// unescape reverses tmux's control-mode escaping: a backslash followed by three
// octal digits, everything else literal.
func unescape(s string) []byte {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); {
		if s[i] == '\\' && i+3 < len(s) && isOctal(s[i+1]) && isOctal(s[i+2]) && isOctal(s[i+3]) {
			v := (int(s[i+1]-'0') << 6) | (int(s[i+2]-'0') << 3) | int(s[i+3]-'0')
			out = append(out, byte(v))
			i += 4
			continue
		}
		out = append(out, s[i])
		i++
	}
	return out
}

func isOctal(b byte) bool { return b >= '0' && b <= '7' }
