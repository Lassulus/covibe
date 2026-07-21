package dashboard

import (
	"bufio"
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"os"
	"strings"
)

// APIKeys is a set of accepted API tokens, stored as SHA-256 digests so the
// plaintext never lives in memory longer than the load call.
type APIKeys struct {
	digests [][32]byte
}

// LoadAPIKeys builds a key set from inline tokens (comma/space/newline
// separated) plus optional files (one token per line, `#` comments). A leading
// `label:` or `label=` on a token is stripped — labels are for the operator's
// bookkeeping, not authentication.
func LoadAPIKeys(inline string, files ...string) (APIKeys, error) {
	var ks APIKeys
	ks.addAll(inline)
	for _, f := range files {
		if f == "" {
			continue
		}
		data, err := os.ReadFile(f)
		if err != nil {
			return ks, err
		}
		sc := bufio.NewScanner(strings.NewReader(string(data)))
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			ks.add(line)
		}
	}
	return ks, nil
}

func (k *APIKeys) addAll(s string) {
	for _, tok := range strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == '\n' || r == ' ' || r == '\t' || r == '\r'
	}) {
		k.add(tok)
	}
}

func (k *APIKeys) add(tok string) {
	// Strip an optional "label:" / "label=" prefix.
	if i := strings.IndexAny(tok, ":="); i >= 0 {
		tok = tok[i+1:]
	}
	tok = strings.TrimSpace(tok)
	if tok == "" {
		return
	}
	k.digests = append(k.digests, sha256.Sum256([]byte(tok)))
}

// Enabled reports whether any key is configured.
func (k APIKeys) Enabled() bool { return len(k.digests) > 0 }

// Valid reports whether token matches a configured key (constant-time, and
// never true when no keys are configured or the token is empty).
func (k APIKeys) Valid(token string) bool {
	if token == "" || len(k.digests) == 0 {
		return false
	}
	sum := sha256.Sum256([]byte(token))
	var match int
	for i := range k.digests {
		match |= subtle.ConstantTimeCompare(sum[:], k.digests[i][:])
	}
	return match == 1
}

// bearerToken extracts an API token from the Authorization bearer header or the
// X-API-Key header.
func bearerToken(r *http.Request) string {
	if h := r.Header.Get("Authorization"); h != "" {
		if rest, ok := strings.CutPrefix(h, "Bearer "); ok {
			return strings.TrimSpace(rest)
		}
	}
	return strings.TrimSpace(r.Header.Get("X-API-Key"))
}
