package dashboard

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateName(t *testing.T) {
	good := []string{"proj", "my-app", "api_v2", "Proj 1", "a.b", "x"}
	bad := []string{"", " ", ".hidden", "-dash", "../etc", "a/b", "a\tb", strings.Repeat("x", 65), "..", "."}
	for _, n := range good {
		if err := validateName(n); err != nil {
			t.Errorf("validateName(%q) unexpected error: %v", n, err)
		}
	}
	for _, n := range bad {
		if err := validateName(n); err == nil {
			t.Errorf("validateName(%q) should have failed", n)
		}
	}
}

func TestResolveWorkspaceDir(t *testing.T) {
	root := "/srv/work"
	ok := []struct{ name, sub, want string }{
		{"proj", "", "/srv/work/proj"},
		{"proj", "sub/dir", "/srv/work/sub/dir"},
		{"proj", "./nested", "/srv/work/nested"},
	}
	for _, tc := range ok {
		got, err := resolveWorkspaceDir(root, tc.name, tc.sub)
		if err != nil {
			t.Fatalf("resolveWorkspaceDir(%q,%q) error: %v", tc.name, tc.sub, err)
		}
		if got != filepath.Clean(tc.want) {
			t.Fatalf("got %q want %q", got, tc.want)
		}
	}

	bad := []struct{ name, sub string }{
		{"proj", "../escape"},
		{"proj", "../../etc"},
		{"proj", "/etc/passwd"},
		{"..", ""},
	}
	for _, tc := range bad {
		if _, err := resolveWorkspaceDir(root, tc.name, tc.sub); err == nil {
			t.Errorf("resolveWorkspaceDir(%q,%q) should have been rejected", tc.name, tc.sub)
		}
	}

	if _, err := resolveWorkspaceDir("", "proj", ""); err == nil {
		t.Error("empty root should disable creation")
	}
}

func TestHandleCreateDisabled(t *testing.T) {
	s := NewServer(Config{Auth: testAuth(OIDCConfig{NoAuth: true})})
	req := httptest.NewRequest("POST", "/api/sessions", strings.NewReader(`{"name":"x"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.handleCreate(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("got %d want 403 when creation disabled", rec.Code)
	}
}

func TestHandleCreateLaunches(t *testing.T) {
	dir := t.TempDir()
	var gotID, gotName, gotDir, gotModel, gotThinking string
	s := NewServer(Config{
		Auth:          testAuth(OIDCConfig{NoAuth: true}),
		WorkspaceRoot: dir,
		Create: func(sp CreateSpec) error {
			gotID, gotName, gotDir, gotModel, gotThinking = sp.ID, sp.Name, sp.Dir, sp.Model, sp.Thinking
			return nil
		},
	})
	req := httptest.NewRequest("POST", "/api/v1/sessions", strings.NewReader(`{"name":"demo","model":"anthropic/claude-opus-4-6","thinking":"high"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.handleCreate(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("got %d want 201; body=%s", rec.Code, rec.Body.String())
	}
	if gotID == "" {
		t.Fatal("create should mint an id and pass it to the launcher")
	}
	if gotName != "demo" || gotDir != filepath.Join(dir, "demo") {
		t.Fatalf("launcher got name=%q dir=%q", gotName, gotDir)
	}
	if gotModel != "anthropic/claude-opus-4-6" || gotThinking != "high" {
		t.Fatalf("launcher got model=%q thinking=%q", gotModel, gotThinking)
	}
	if fi, err := os.Stat(filepath.Join(dir, "demo")); err != nil || !fi.IsDir() {
		t.Fatalf("workspace dir not created: %v", err)
	}
}

func TestHandleCreateRejectsBadName(t *testing.T) {
	dir := t.TempDir()
	called := false
	s := NewServer(Config{
		Auth:          testAuth(OIDCConfig{NoAuth: true}),
		WorkspaceRoot: dir,
		Create:        func(CreateSpec) error { called = true; return nil },
	})
	req := httptest.NewRequest("POST", "/api/v1/sessions", strings.NewReader(`{"name":"../evil"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.handleCreate(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d want 400 for bad name", rec.Code)
	}
	if called {
		t.Fatal("launcher must not run for an invalid name")
	}
}
