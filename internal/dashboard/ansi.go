package dashboard

import "regexp"

var (
	// ANSI CSI sequences (colour, cursor moves).
	ansiRe = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)
	// OSC sequences (e.g. OSC-8 hyperlinks), terminated by BEL or ST.
	oscRe = regexp.MustCompile("\x1b\\][^\x07\x1b]*(?:\x07|\x1b\\\\)")
)

// stripANSI removes OSC and CSI escape sequences, yielding readable plain text
// for pane snapshots.
func stripANSI(b []byte) []byte {
	b = oscRe.ReplaceAll(b, nil)
	return ansiRe.ReplaceAll(b, nil)
}
