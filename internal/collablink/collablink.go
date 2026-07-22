// Package collablink mints and formats omp-compatible collab links. covibe owns
// the room identity: it generates the roomId + room secret (32-byte AES key ‖
// 16-byte write token) up front, hands them to the patched omp host via env, and
// derives the same shareable links omp's `/collab` would — so sessions have a
// stable, enumerable id with no scraping or capture.
//
// Wire format (see omp packages/wire + collab-web/src/lib/link.ts):
//
//	roomId  = base64url(16 random bytes)          → 22 chars
//	secret  = base64url(key‖writeToken, 48 bytes)  → 64 chars   (full link)
//	relay link (omp join): "<host[:port]>/r/<roomId>.<secret>"  (wss inferred)
//	browser link:          "<webClient>/#<relay link>"
package collablink

import (
	"crypto/rand"
	"encoding/base64"
	"strings"
)

const (
	roomIDBytes = 16
	keyBytes    = 32
	tokenBytes  = 16
)

// Room is a freshly minted collab room identity.
type Room struct {
	ID     string // base64url room id
	Secret string // base64url(key‖writeToken); goes in the link fragment + OMP_COLLAB_KEY
}

// Mint generates a new room id and full room secret.
func Mint() (Room, error) {
	b := make([]byte, roomIDBytes+keyBytes+tokenBytes)
	if _, err := rand.Read(b); err != nil {
		return Room{}, err
	}
	return Room{
		ID:     base64.RawURLEncoding.EncodeToString(b[:roomIDBytes]),
		Secret: base64.RawURLEncoding.EncodeToString(b[roomIDBytes:]),
	}, nil
}

// JoinLink is the terminal/relay link accepted by `omp join`: scheme-less so omp
// infers wss:// (host must be a real TLS host for non-localhost guests).
func JoinLink(relayHost, roomID, secret string) string {
	return relayHost + "/r/" + roomID + "." + secret
}

// BrowserURL is the click-to-join link: the collab-web client base with the
// relay link in the fragment (the key never leaves the fragment).
func BrowserURL(webClient, relayHost, roomID, secret string) string {
	return strings.TrimSuffix(webClient, "/") + "/#" + JoinLink(relayHost, roomID, secret)
}
