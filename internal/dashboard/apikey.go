package dashboard

import (
	"bufio"
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"net/http"
	"os"
	"strings"
)

// APIKeys is a set of accepted API tokens, stored as SHA-256 digests so the
// plaintext never lives in memory longer than the load call. A digest may carry
// the covibe user it acts as: an unattributed key is a machine credential with
// access to everything, a user key acts as exactly that user.
type APIKeys struct {
	digests [][32]byte
	// users[i] is the covibe user digests[i] acts as, "" for a machine key.
	users []string
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
		data, err := os.ReadFile(f) // #nosec G304 -- f is an operator-provided config path, not user input
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
	k.addFor("", tok)
}

// addFor records a token, optionally bound to a covibe user.
func (k *APIKeys) addFor(user, tok string) {
	tok = strings.TrimSpace(tok)
	if tok == "" {
		return
	}
	k.digests = append(k.digests, sha256.Sum256([]byte(tok)))
	k.users = append(k.users, strings.ToLower(strings.TrimSpace(user)))
}

// AddUserKeys parses `user:token` pairs (comma/space/newline separated) plus
// optional files of the same, one per line with `#` comments. Unlike the machine
// keys above, the part before the colon is authentication data, not a label: a
// request carrying the token acts as that covibe user.
func (k *APIKeys) AddUserKeys(inline string, files ...string) error {
	for _, pair := range strings.FieldsFunc(inline, func(r rune) bool {
		return r == ',' || r == '\n' || r == ' ' || r == '\t' || r == '\r'
	}) {
		if err := k.addPair(pair); err != nil {
			return err
		}
	}
	for _, f := range files {
		if f == "" {
			continue
		}
		data, err := os.ReadFile(f) // #nosec G304 -- f is an operator-provided config path, not user input
		if err != nil {
			return err
		}
		sc := bufio.NewScanner(strings.NewReader(string(data)))
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			if err := k.addPair(line); err != nil {
				return err
			}
		}
	}
	return nil
}

func (k *APIKeys) addPair(pair string) error {
	user, tok, ok := strings.Cut(pair, ":")
	if !ok || strings.TrimSpace(user) == "" || strings.TrimSpace(tok) == "" {
		return fmt.Errorf("user key %q is not user:token", firstField(pair))
	}
	k.addFor(user, tok)
	return nil
}

// firstField keeps a malformed-entry error from echoing what might be a secret.
func firstField(pair string) string {
	if i := strings.IndexAny(pair, ":="); i >= 0 {
		return pair[:i] + ":…"
	}
	return "…"
}

// Valid reports whether token matches a configured key (constant-time, and
// never true when no keys are configured or the token is empty).
func (k APIKeys) Valid(token string) bool {
	_, ok := k.Lookup(token)
	return ok
}

// Lookup matches a token and returns the covibe user it acts as, empty for a
// machine key. The scan is constant-time in the comparison and does not stop
// early, so a caller cannot time which key matched.
func (k APIKeys) Lookup(token string) (user string, ok bool) {
	if token == "" || len(k.digests) == 0 {
		return "", false
	}
	sum := sha256.Sum256([]byte(token))
	var match int
	for i := range k.digests {
		if subtle.ConstantTimeCompare(sum[:], k.digests[i][:]) == 1 {
			match = 1
			user = k.users[i]
		}
	}
	return user, match == 1
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
