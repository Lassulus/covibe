package dashboard

import (
	"sync"
	"time"
)

// killedTTL bounds how long a kill is remembered. It must comfortably exceed the
// remote heartbeat interval and RemoteTTL so a wrapper that checks in late (a
// suspended laptop, a slow link) still learns it was killed instead of silently
// re-registering.
const killedTTL = 10 * time.Minute

// killRegistry remembers recently killed sessions so a kill sticks even after
// the record itself is pruned.
//
// Without it, kill is a race against the 20s KeepEnded window: the record holds
// `ended` (which the heartbeat reply turns into stop:true) only until it is
// pruned, and a wrapper that misses that window gets a plain 404, treats it as
// "the dashboard forgot me", and re-registers under a fresh id — the card
// reappears and the session outlives the kill.
//
// Sessions are remembered by both keys the wrapper can present: the record id it
// heartbeats on, and the collab room id, which survives re-registration (covibe
// mints it once per wrapper run) and is therefore what makes a resurrection
// recognisable.
type killRegistry struct {
	mu   sync.Mutex
	seen map[string]time.Time
}

func newKillRegistry() *killRegistry {
	return &killRegistry{seen: map[string]time.Time{}}
}

// remember records every non-empty key as killed now.
func (k *killRegistry) remember(keys ...string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	now := time.Now()
	for key, at := range k.seen {
		if now.Sub(at) > killedTTL {
			delete(k.seen, key)
		}
	}
	for _, key := range keys {
		if key != "" {
			k.seen[key] = now
		}
	}
}

// has reports whether key was killed within killedTTL.
func (k *killRegistry) has(key string) bool {
	if key == "" {
		return false
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	at, ok := k.seen[key]
	if !ok {
		return false
	}
	if time.Since(at) > killedTTL {
		delete(k.seen, key)
		return false
	}
	return true
}
