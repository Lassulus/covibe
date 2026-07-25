package session

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/lassulus/covibe/internal/spool"
)

// RemoteSink announces a session to a covibe dashboard over its (keyless) REST
// API and keeps it listed by periodically pushing the pane snapshot, which
// doubles as a heartbeat. Used when the wrapper runs on a different machine than
// the dashboard, so the session still shows up in the overview. The dashboard
// GCs the announcement once the heartbeat stops.
type RemoteSink struct {
	base     string // dashboard base URL, e.g. https://covibe.lassul.us
	interval time.Duration
	client   *http.Client
}

// NewRemoteSink builds a sink targeting the dashboard at base.
func NewRemoteSink(base string) *RemoteSink {
	return &RemoteSink{
		base:     strings.TrimSuffix(base, "/"),
		interval: 4 * time.Second,
		client:   &http.Client{Timeout: 15 * time.Second},
	}
}

func (s *RemoteSink) do(ctx context.Context, method, path string, body []byte) (*http.Response, error) {
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, s.base+path, r)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/octet-stream")
	}
	return s.client.Do(req)
}

// Register creates the dashboard record and adopts the server-minted id (which
// is the capability for pushing this session's pane).
func (s *RemoteSink) Register(rec *spool.Record) error {
	rec.Remote = true
	rec.PID = 0
	if rec.Host == "" {
		rec.Host, _ = os.Hostname()
	}
	reqBody, _ := json.Marshal(spool.RegisterRequest{
		Name:       rec.Name,
		Dir:        rec.Dir,
		Model:      rec.Model,
		Thinking:   rec.Thinking,
		Host:       rec.Host,
		Relay:      rec.Relay,
		JoinLink:   rec.JoinLink,
		BrowserURL: rec.BrowserURL,
		RoomID:     rec.RoomID,
		ViewOnly:   rec.ViewOnly,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.base+"/api/v1/sessions/register", bytes.NewReader(reqBody))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("register: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return fmt.Errorf("register: decode response: %w", err)
	}
	if out.ID == "" {
		return fmt.Errorf("register: dashboard returned no id")
	}
	rec.ID = out.ID
	if rec.BrowserURL != "" {
		fmt.Fprintf(os.Stderr, "\n  covibe: session registered on %s\n  browser  %s\n\n", s.base, rec.BrowserURL)
	}
	return nil
}

// Watch pushes the pane snapshot on a fixed interval (heartbeat + preview) and
// closes the returned channel when the dashboard asks the session to stop.
func (s *RemoteSink) Watch(rec *spool.Record, pane *paneBuffer) (<-chan struct{}, func()) {
	killed := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		t := time.NewTicker(s.interval)
		defer t.Stop()
		var lastHash uint64
		var sent bool
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				snap := pane.snapshot()
				h := hashBytes(snap)
				var body []byte
				if !sent || h != lastHash {
					body = snap // include the pane only when it changed
				}
				stop, err := s.heartbeat(ctx, rec.ID, body)
				if err == nil {
					lastHash, sent = h, true
				}
				if stop {
					close(killed)
					return
				}
			}
		}
	}()
	return killed, cancel
}

// heartbeat pushes the optional pane snapshot and reports whether the dashboard
// has asked the session to stop. An empty body is a liveness-only heartbeat.
func (s *RemoteSink) heartbeat(ctx context.Context, id string, body []byte) (stop bool, err error) {
	hctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if body == nil {
		body = []byte{}
	}
	resp, err := s.do(hctx, http.MethodPost, "/api/v1/sessions/"+id+"/pane", body)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("heartbeat: %s", resp.Status)
	}
	var out struct {
		Stop bool `json:"stop"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return out.Stop, nil
}

// End is a no-op: deregistration is left to the dashboard's GC, which drops the
// announcement once the heartbeat stops (killing requires the dashboard owner).
func (s *RemoteSink) End(_ *spool.Record) {}

func hashBytes(b []byte) uint64 {
	h := fnv.New64a()
	_, _ = h.Write(b)
	return h.Sum64()
}
