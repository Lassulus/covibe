package dashboard

import (
	_ "embed"
	"html/template"
	"net/http"
)

//go:embed viewer.html
var viewerHTML string

var viewerTmpl = template.Must(template.New("viewer").Parse(viewerHTML))

// handleViewer serves the browser viewer for a live session. Gated by OIDC (it
// is registered under the protected mux). The page connects back to
// /collab/guest/{id} for the live event stream.
func (s *Server) handleViewer(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	rec, ok := s.liveRecord(id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	nonce := randToken()
	data := struct {
		Nonce     string
		SessionID string
		Name      string
		CanWrite  bool
	}{Nonce: nonce, SessionID: id, Name: rec.Name, CanWrite: true}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy",
		"default-src 'self'; script-src 'nonce-"+nonce+"'; style-src 'nonce-"+nonce+"'; "+
			"img-src 'self' data:; connect-src 'self'; base-uri 'none'; "+
			"frame-ancestors 'none'; object-src 'none'; form-action 'self'")
	if err := viewerTmpl.Execute(w, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
