// Package spool is the on-disk session registry shared between the covibe
// session wrappers (writers) and the dashboard (reader). Each live omp session
// owns exactly one JSON file; writes are atomic (temp + rename) so a reader
// never observes a half-written record.
package spool

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Status values for a session record.
const (
	StatusStarting = "starting" // wrapper up, no collab link captured yet
	StatusLive     = "live"     // collab link captured, session shareable
	StatusEnded    = "ended"    // omp exited; record kept briefly for the UI
)

// RemoteTTL bounds how long a remote (REST-registered) record is considered
// alive without a heartbeat. The remote wrapper pushes pane/heartbeat well
// inside this window; a stale record (wrapper gone) is pruned by the dashboard.
const RemoteTTL = 30 * time.Second

// Record is one co-vibing session as seen by the dashboard.
type Record struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Dir        string    `json:"dir"`
	Mux        string    `json:"mux,omitempty"`        // "zellij" | "tmux"
	MuxSession string    `json:"muxSession,omitempty"` // multiplexer session name
	MuxSocket  string    `json:"muxSocket,omitempty"`  // tmux server socket covibe drives (control mode, capture-pane)
	MuxTab     string    `json:"muxTab,omitempty"`     // tab/window name
	Model      string    `json:"model,omitempty"`      // omp --model selector
	Thinking   string    `json:"thinking,omitempty"`   // omp --thinking level
	PID        int       `json:"pid"`                  // pid of the covibe session wrapper
	Status     string    `json:"status"`
	Relay      string    `json:"relay,omitempty"`      // ws(s):// relay used for /collab
	JoinLink   string    `json:"joinLink,omitempty"`   // omp join string "<host>/r/<roomId>.<secret>"
	BrowserURL string    `json:"browserUrl,omitempty"` // https collab-web deep link for phones/browsers
	ViewOnly   bool      `json:"viewOnly,omitempty"`
	RoomID     string    `json:"roomId,omitempty"` // covibe-minted collab room id (stable per session)
	StartedAt  time.Time `json:"startedAt"`
	UpdatedAt  time.Time `json:"updatedAt"`
	Remote     bool      `json:"remote,omitempty"` // registered via REST from another machine; liveness is heartbeat-TTL, pane is pushed
	Host       string    `json:"host,omitempty"`   // origin machine for remote sessions (display only)
}

// RegisterRequest is the body a remote wrapper POSTs to register a session it is
// hosting on another machine. The dashboard whitelists these fields into a
// fresh Record (Remote=true); it never trusts a caller-supplied pid or status.
type RegisterRequest struct {
	Name       string `json:"name"`
	Dir        string `json:"dir,omitempty"`
	Model      string `json:"model,omitempty"`
	Thinking   string `json:"thinking,omitempty"`
	Host       string `json:"host,omitempty"`
	Relay      string `json:"relay,omitempty"`
	JoinLink   string `json:"joinLink,omitempty"`
	BrowserURL string `json:"browserUrl,omitempty"`
	RoomID     string `json:"roomId,omitempty"`
	ViewOnly   bool   `json:"viewOnly,omitempty"`
}

// Alive reports whether the wrapper process backing the record is still running.
// A dead pid means the wrapper crashed without cleaning up; such records are
// pruned by the dashboard rather than shown as live.
func (r Record) Alive() bool {
	// Remote records have no local process to probe; liveness is the freshness
	// of the wrapper's heartbeat (UpdatedAt), reconciled the same way pid
	// liveness is for local sessions.
	if r.Remote {
		return time.Since(r.UpdatedAt) < RemoteTTL
	}
	if r.PID <= 0 {
		return false
	}
	proc, err := os.FindProcess(r.PID)
	if err != nil {
		return false
	}
	// Signal 0 performs error checking without delivering a signal.
	err = proc.Signal(syscall.Signal(0))
	if err == nil {
		return true
	}
	return errors.Is(err, syscall.EPERM)
}

// Store is a directory of session records.
type Store struct {
	dir string
}

// DefaultDir resolves the spool directory: $COVIBE_STATE_DIR, else
// $XDG_RUNTIME_DIR/covibe, else a uid-scoped path under the temp dir.
func DefaultDir() string {
	if d := os.Getenv("COVIBE_STATE_DIR"); d != "" {
		return d
	}
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" {
		return filepath.Join(d, "covibe")
	}
	return filepath.Join(os.TempDir(), "covibe-"+strconv.Itoa(os.Getuid()))
}

// Open returns a store rooted at dir (DefaultDir when empty), creating it 0700.
func Open(dir string) (*Store, error) {
	if dir == "" {
		dir = DefaultDir()
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create spool dir: %w", err)
	}
	return &Store{dir: dir}, nil
}

// Dir returns the store's root directory.
func (s *Store) Dir() string { return s.dir }

func (s *Store) path(id string) string {
	return filepath.Join(s.dir, id+".json")
}

// PanePath is the unix socket a session wrapper serves its terminal snapshot on.
func (s *Store) PanePath(id string) string {
	return filepath.Join(s.dir, id+".sock")
}

// PaneFilePath is the regular file a remote wrapper's pushed pane snapshot is
// stored at (remote sessions cannot serve the unix socket of PanePath).
func (s *Store) PaneFilePath(id string) string {
	return filepath.Join(s.dir, id+".pane")
}

// TmuxDir is the directory holding one tmux server socket per covibe user.
func (s *Store) TmuxDir() string { return filepath.Join(s.dir, "tmux") }

// TmuxSocket is the socket of the tmux server that runs a user's sessions.
// One socket is one tmux server, so this is also the boundary between users:
// a session on alice's socket is invisible to a tmux client on bob's. The
// directory is created 0700 on demand, and the socket itself is created 0600 by
// tmux (non-default sockets get no group/other bits).
func (s *Store) TmuxSocket(user string) (string, error) {
	dir := s.TmuxDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create tmux socket dir: %w", err)
	}
	return filepath.Join(dir, sanitizeUser(user)+".sock"), nil
}

// sanitizeUser turns a covibe user key (an email, a sub, a username) into one
// safe path component. Unowned sessions — started from the CLI on the host —
// share the "local" socket, which is the host operator's own server.
func sanitizeUser(user string) string {
	user = strings.TrimSpace(strings.ToLower(user))
	if user == "" {
		return "local"
	}
	var b strings.Builder
	for _, r := range user {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	// sun_path is ~108 bytes; keep the component well clear of it.
	out := strings.Trim(b.String(), "-")
	if len(out) > 48 {
		out = out[:48]
	}
	if out == "" {
		return "local"
	}
	return out
}

// Save atomically writes a record, stamping UpdatedAt.
func (s *Store) Save(r *Record) error {
	if r.ID == "" {
		return errors.New("record has no id")
	}
	r.UpdatedAt = time.Now()
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.dir, r.ID+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, s.path(r.ID))
}

// Load reads a single record by id.
func (s *Store) Load(id string) (*Record, error) {
	data, err := os.ReadFile(s.path(id))
	if err != nil {
		return nil, err
	}
	var r Record
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// Remove deletes a record file, ignoring absence.
func (s *Store) Remove(id string) error {
	_ = os.Remove(s.PaneFilePath(id))
	err := os.Remove(s.path(id))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// List returns every parseable record, newest first. Unreadable/partial files
// are skipped rather than failing the whole listing.
func (s *Store) List() ([]Record, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []Record
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.dir, e.Name()))
		if err != nil {
			continue
		}
		var r Record
		if json.Unmarshal(data, &r) != nil {
			continue
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].StartedAt.After(out[j].StartedAt)
	})
	return out, nil
}

// Live returns records whose wrapper is still alive, reconciling status and
// pruning stale files (dead wrapper, or ended more than keepEnded ago).
func (s *Store) Live(keepEnded time.Duration) ([]Record, error) {
	all, err := s.List()
	if err != nil {
		return nil, err
	}
	var out []Record
	for _, r := range all {
		if !r.Alive() {
			_ = s.Remove(r.ID)
			continue
		}
		if r.Status == StatusEnded && time.Since(r.UpdatedAt) > keepEnded {
			_ = s.Remove(r.ID)
			continue
		}
		out = append(out, r)
	}
	return out, nil
}
