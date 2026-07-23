package dashboard

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// safeName limits session names to a single, filesystem-safe path component:
// starts alphanumeric, then alphanumerics plus space/dot/dash/underscore. No
// slashes, no leading dot/dash, so it can never introduce traversal or hidden
// dirs.
var safeName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 ._-]{0,63}$`)

func validateName(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("name is required")
	}
	if !safeName.MatchString(name) {
		return fmt.Errorf("name must be 1-64 chars: letters, digits, space, . _ - (no leading . or -)")
	}
	if name == "." || name == ".." {
		return fmt.Errorf("invalid name")
	}
	return nil
}

// resolveWorkspaceDir returns the absolute working directory for a new session,
// clamped inside root. `sub` (optional) overrides the default subdir (the
// name). Any path escaping root is rejected.
func resolveWorkspaceDir(root, name, sub string) (string, error) {
	if root == "" {
		return "", fmt.Errorf("session creation is disabled (no workspace root configured)")
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	target := sub
	if strings.TrimSpace(target) == "" {
		target = name
	}
	// Treat target as a relative path under root; reject absolutes and traversal.
	rel := filepath.Clean(strings.TrimSpace(target))
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("directory must be relative to the workspace root")
	}
	joined := filepath.Join(absRoot, rel)
	// Confirm containment: rel-from-root must not climb out.
	back, err := filepath.Rel(absRoot, joined)
	if err != nil || back == ".." || strings.HasPrefix(back, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("directory escapes the workspace root")
	}
	return joined, nil
}

type createRequest struct {
	Name     string `json:"name"`
	Dir      string `json:"dir"`
	Model    string `json:"model"`
	Thinking string `json:"thinking"`
}

// CreateSpec parameterizes a web/API-initiated session launch.
type CreateSpec struct {
	ID       string
	Name     string
	Dir      string
	Model    string // omp --model (optional; may carry :thinking suffix)
	Thinking string // omp --thinking level (optional)
}

// safeModel constrains a model selector to characters omp uses in provider/
// model ids plus the optional :thinking suffix. It is passed as a distinct
// argv element (no shell), but constrained to reject junk.
var safeModel = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/:@-]{0,127}$`)

// thinkingLevels are omp's accepted --thinking values.
var thinkingLevels = map[string]bool{
	"off": true, "minimal": true, "low": true,
	"medium": true, "high": true, "xhigh": true, "max": true,
}

func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Create == nil || s.cfg.WorkspaceRoot == "" {
		http.Error(w, "session creation is disabled", http.StatusForbidden)
		return
	}
	var req createRequest
	ct := r.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "application/json") {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
			http.Error(w, "invalid JSON body", http.StatusBadRequest)
			return
		}
	} else {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "invalid form", http.StatusBadRequest)
			return
		}
		req.Name = r.FormValue("name")
		req.Dir = r.FormValue("dir")
	}
	req.Name = strings.TrimSpace(req.Name)
	if err := validateName(req.Name); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	model := strings.TrimSpace(req.Model)
	if model != "" && !safeModel.MatchString(model) {
		http.Error(w, "invalid model", http.StatusBadRequest)
		return
	}
	thinking := strings.TrimSpace(req.Thinking)
	if thinking != "" && !thinkingLevels[thinking] {
		http.Error(w, "invalid thinking level", http.StatusBadRequest)
		return
	}
	if s.cfg.MaxSessions > 0 {
		if live, _ := s.cfg.Store.Live(s.cfg.KeepEnded); len(live) >= s.cfg.MaxSessions {
			http.Error(w, "session limit reached", http.StatusTooManyRequests)
			return
		}
	}
	dir, err := resolveWorkspaceDir(s.cfg.WorkspaceRoot, req.Name, req.Dir)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		http.Error(w, "create workspace dir: "+err.Error(), http.StatusInternalServerError)
		return
	}
	id := newSessionID()
	if err := s.cfg.Create(CreateSpec{ID: id, Name: req.Name, Dir: dir, Model: model, Thinking: thinking}); err != nil {
		http.Error(w, "launch failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"id": id, "name": req.Name, "dir": dir, "model": model, "thinking": thinking})
}

// newSessionID mints a spool id for a session created through the API, so the
// caller can immediately GET /api/v1/sessions/<id>.
func newSessionID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "covibe-" + hex.EncodeToString(b)
}
