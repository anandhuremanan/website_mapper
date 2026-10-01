package api

import (
	"crypto/subtle"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Headers the web client's proxy adds to every request it forwards.
const (
	// secretHeader proves the request came through the web client.
	secretHeader = "X-Scanner-Secret"
	// clientHeader is the visitor's address as the web client saw it. It is
	// believed only together with the secret.
	clientHeader = "X-Scanner-Client"
)

// Access decides who may call the API and how often they may start scans.
type Access struct {
	// ProxySecret, if set, closes the API to everyone but the web client:
	// requests must carry it in the X-Scanner-Secret header. Empty leaves
	// the API open, for development and for deployments where the API port
	// is not reachable from outside anyway.
	ProxySecret string
	// StartLimit is how many scans one visitor may start per StartWindow
	// (0: no limit). Requests answered with a scan that already exists
	// (joined or reused) do not count and are never refused: they cost
	// nothing.
	StartLimit  int
	StartWindow time.Duration
	// Now is the clock; overridable for tests.
	Now func() time.Time
}

// viaWebClient reports whether the request carries the proxy secret.
func (a Access) viaWebClient(r *http.Request) bool {
	if a.ProxySecret == "" {
		return false
	}
	got := r.Header.Get(secretHeader)
	return subtle.ConstantTimeCompare([]byte(got), []byte(a.ProxySecret)) == 1
}

// allowed reports whether the request may use the API at all.
func (a Access) allowed(r *http.Request) bool {
	return a.ProxySecret == "" || a.viaWebClient(r)
}

// visitor identifies who is asking, for the start limit.
//
// Through the web client it is the address the client reports, which can
// be believed because the secret came with it. On an open API there is
// nothing to believe: the first forwarded address is used if a proxy added
// one (so that visitors behind a reverse proxy are told apart), else the
// peer address. A direct caller can lie about the former, which only an
// API closed with a secret prevents.
func (a Access) visitor(r *http.Request) string {
	if a.viaWebClient(r) {
		if v := strings.TrimSpace(r.Header.Get(clientHeader)); v != "" {
			return v
		}
		return "unknown"
	}
	if first, _, _ := strings.Cut(r.Header.Get("X-Forwarded-For"), ","); strings.TrimSpace(first) != "" {
		return strings.TrimSpace(first)
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// startLimiter counts the scans each visitor started within a window.
type startLimiter struct {
	limit  int
	window time.Duration
	now    func() time.Time

	mu     sync.Mutex
	starts map[string][]time.Time
}

// maxVisitors bounds the visitors remembered at once.
const maxVisitors = 10000

func newStartLimiter(a Access) *startLimiter {
	now := a.Now
	if now == nil {
		now = time.Now
	}
	return &startLimiter{limit: a.StartLimit, window: a.StartWindow, now: now, starts: map[string][]time.Time{}}
}

// recent returns the visitor's starts still inside the window. Caller
// holds mu.
func (l *startLimiter) recent(visitor string) []time.Time {
	cutoff := l.now().Add(-l.window)
	starts := l.starts[visitor]
	for len(starts) > 0 && !starts[0].After(cutoff) {
		starts = starts[1:]
	}
	if len(starts) == 0 {
		delete(l.starts, visitor)
	} else {
		l.starts[visitor] = starts
	}
	return starts
}

// take uses one of the visitor's starts. If none is left it uses nothing
// and returns how long until one is.
func (l *startLimiter) take(visitor string) (wait time.Duration) {
	if l.limit <= 0 || l.window <= 0 {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	starts := l.recent(visitor)
	if len(starts) >= l.limit {
		// The oldest start that still counts leaves the window first.
		return starts[len(starts)-l.limit].Add(l.window).Sub(l.now())
	}
	if len(l.starts) >= maxVisitors {
		for v := range l.starts {
			l.recent(v) // drops visitors with nothing left in the window
		}
		if len(l.starts) >= maxVisitors {
			return 0 // still full: stop remembering rather than grow
		}
	}
	l.starts[visitor] = append(starts, l.now())
	return 0
}

// startLimitError refuses a scan because the visitor started too many.
type startLimitError struct{ wait time.Duration }

func (e *startLimitError) Error() string { return "too many scans started" }
