// Package collab extracts an omp /collab join link out of a terminal byte
// stream and derives the browser deep link for it.
//
// When a host runs /collab, omp prints (with ANSI colour and an OSC-8
// hyperlink wrapping the browser URL):
//
//   - Join from another terminal: omp join "<roomId>.<key>"
//   - or any web browser: my.omp.sh/#<roomId>.<key>
//
// The canonical link is "<roomId>.<key>" (base64url, dot-joined). We recover it
// from either surface: the `omp join "..."` text or a `host/#<link>` URL (which
// also appears verbatim inside the OSC-8 escape target).
package collab

import (
	"regexp"
	"strings"
)

var (
	// omp join "<roomId>.<key>"  — key may be dot- or (legacy) #-joined.
	joinRe = regexp.MustCompile(`omp join "([A-Za-z0-9_-]{8,}[.#][A-Za-z0-9_-]{20,})"`)
	// A browser/relay URL carrying the link in its fragment: host[:port][/path]/#<link>.
	// Matches both raw text and the URL inside an OSC-8 hyperlink target.
	fragRe = regexp.MustCompile(`[A-Za-z0-9.:-]+(?:/[A-Za-z0-9._~/-]*)?/#([A-Za-z0-9_-]{8,}[.#][A-Za-z0-9_-]{20,})`)
	// ANSI CSI sequences (colour, cursor moves) — stripped before text search.
	ansiRe = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)
)

// Extract scans raw terminal bytes for a collab join link. It returns the
// canonical "<roomId>.<key>" (dot-joined) form and whether one was found.
func Extract(raw []byte) (link string, ok bool) {
	// The join text is the most reliable surface; search it first on the
	// ANSI-stripped stream so colour codes between words don't defeat the match.
	stripped := ansiRe.ReplaceAll(raw, nil)
	if m := joinRe.FindSubmatch(stripped); m != nil {
		return canonical(string(m[1])), true
	}
	// Fall back to a fragment URL (raw stream: also catches the OSC-8 target).
	if m := fragRe.FindSubmatch(raw); m != nil {
		return canonical(string(m[1])), true
	}
	return "", false
}

// canonical normalizes the legacy "<roomId>#<key>" form to the dot-joined
// "<roomId>.<key>" form used in newly generated links.
func canonical(link string) string {
	if i := strings.IndexByte(link, '#'); i >= 0 {
		return link[:i] + "." + link[i+1:]
	}
	return link
}

// RoomID returns the room id portion of a canonical link.
func RoomID(link string) string {
	if i := strings.IndexByte(link, '.'); i >= 0 {
		return link[:i]
	}
	return link
}

// BrowserURL builds the https deep link a phone or browser scans to join.
//
//   - webURL set   → "<webURL>/#<link>"
//   - else relay   → scheme-swapped relay origin + "/#<link>"
//     (wss→https, ws→http; ws://localhost stays http)
//   - else default → "https://my.omp.sh/#<link>"
//
// The relay-specific link always rides in the fragment, matching omp's own
// deep-link format.
func BrowserURL(link, relay, webURL string) string {
	if link == "" {
		return ""
	}
	base := "https://my.omp.sh"
	switch {
	case webURL != "":
		base = strings.TrimRight(webURL, "/")
	case relay != "":
		base = relayOrigin(relay)
	}
	return base + "/#" + link
}

// relayOrigin converts a websocket relay URL to its browser http(s) origin,
// dropping any /r/... path.
func relayOrigin(relay string) string {
	origin := relay
	switch {
	case strings.HasPrefix(origin, "wss://"):
		origin = "https://" + origin[len("wss://"):]
	case strings.HasPrefix(origin, "ws://"):
		origin = "http://" + origin[len("ws://"):]
	case !strings.Contains(origin, "://"):
		origin = "https://" + origin
	}
	// Keep only scheme://host[:port].
	if i := strings.Index(origin, "://"); i >= 0 {
		rest := origin[i+3:]
		if j := strings.IndexByte(rest, '/'); j >= 0 {
			rest = rest[:j]
		}
		origin = origin[:i+3] + rest
	}
	return strings.TrimRight(origin, "/")
}
