package dashboard

// Vendored browser assets, fetched verbatim (not minified, not edited):
//
//	xterm.js      @xterm/xterm 5.5.0
//	              https://unpkg.com/@xterm/xterm@5.5.0/lib/xterm.js
//	              sha256 1f991ac3b4b283ebf96e60ae23a00a52765dd3a2e46fa6fdda9f1aab032f7495
//	xterm.css     @xterm/xterm 5.5.0
//	              https://unpkg.com/@xterm/xterm@5.5.0/css/xterm.css
//	              sha256 ba8e6985669488981ccf40c0cefe3aba80722cb6c92de7ad628b0bd717faf2b6
//	addon-fit.js  @xterm/addon-fit 0.10.0
//	              https://unpkg.com/@xterm/addon-fit@0.10.0/lib/addon-fit.js
//	              sha256 bdaefa370b1bfc42ee88d46fe6072400902a4d4b2d45cd93438dda9b23c97089
//
// terminal.js is ours.

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"
)

//go:embed assets
var assetFS embed.FS

type asset struct {
	body  []byte
	ctype string
	etag  string
	// The vendored files are pinned to an exact upstream version (xterm 5.5.0,
	// addon-fit 0.10.0) and can never change, so they cache for a year.
	// terminal.js ships with covibe and does change across builds, so it
	// revalidates against its ETag instead.
	cache string
}

const (
	cacheImmutable  = "public, max-age=31536000, immutable"
	cacheRevalidate = "public, max-age=300, must-revalidate"
)

var assets = loadAssets()

func loadAssets() map[string]asset {
	sub, err := fs.Sub(assetFS, "assets")
	if err != nil {
		panic("dashboard: embedded assets: " + err.Error())
	}
	entries, err := fs.ReadDir(sub, ".")
	if err != nil {
		panic("dashboard: embedded assets: " + err.Error())
	}
	out := make(map[string]asset, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		body, err := fs.ReadFile(sub, e.Name())
		if err != nil {
			panic("dashboard: embedded asset " + e.Name() + ": " + err.Error())
		}
		sum := sha256.Sum256(body)
		cache := cacheImmutable
		if e.Name() == "terminal.js" {
			cache = cacheRevalidate
		}
		out[e.Name()] = asset{
			body:  body,
			ctype: contentType(e.Name()),
			etag:  `"` + hex.EncodeToString(sum[:]) + `"`,
			cache: cache,
		}
	}
	return out
}

func contentType(name string) string {
	switch path.Ext(name) {
	case ".js":
		return "application/javascript; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	default:
		return "application/octet-stream"
	}
}

// assetHandler serves the vendored browser assets under /assets/.
func assetHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		// Exact-name lookup in a flat map: no traversal, no directory listing,
		// nothing served that isn't embedded.
		name := strings.TrimPrefix(r.URL.Path, "/assets/")
		a, ok := assets[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		h := w.Header()
		h.Set("Content-Type", a.ctype)
		h.Set("Cache-Control", a.cache)
		h.Set("ETag", a.etag)
		h.Set("X-Content-Type-Options", "nosniff")
		// Zero modtime keeps ServeContent off Last-Modified; the ETag answers
		// conditional requests.
		http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(a.body))
	})
}
