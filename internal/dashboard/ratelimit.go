package dashboard

import (
	"net"
	"net/http"
	"sync"
	"time"
)

// failLimiter throttles a client after too many failed authentication attempts
// within a rolling window, to blunt online API-key / OIDC brute force. Only
// failures are counted; a successful auth never consumes budget.
type failLimiter struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	hits   map[string]*failEntry
}

type failEntry struct {
	count int
	reset time.Time
}

func newFailLimiter(max int, window time.Duration) *failLimiter {
	return &failLimiter{max: max, window: window, hits: map[string]*failEntry{}}
}

// blocked reports whether the client has exhausted its failure budget. It also
// opportunistically prunes expired entries so the map cannot grow unbounded.
func (l *failLimiter) blocked(ip string) bool {
	if l == nil {
		return false
	}
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	for k, e := range l.hits {
		if now.After(e.reset) {
			delete(l.hits, k)
		}
	}
	e := l.hits[ip]
	return e != nil && e.count >= l.max && now.Before(e.reset)
}

// fail records one failed attempt for the client.
func (l *failLimiter) fail(ip string) {
	if l == nil {
		return
	}
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.hits[ip]
	if e == nil || now.After(e.reset) {
		l.hits[ip] = &failEntry{count: 1, reset: now.Add(l.window)}
		return
	}
	e.count++
}

// clientIP returns the request's peer IP (no port). Behind a reverse proxy this
// is the proxy address; deploy covibe on loopback/a private network so the peer
// is meaningful (X-Forwarded-For is intentionally not trusted).
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
