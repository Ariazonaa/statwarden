package api

import (
	"net/http"
	"strings"
	"sync"
	"time"
)

// publicRateLimit is a chi middleware applying the public fixed-window limiter,
// keyed by client IP, to unauthenticated endpoints (status pages).
func (s *server) publicRateLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.publicLimiter.allow("public:" + clientIP(r)) {
			writeProblemDetail(w, http.StatusTooManyRequests, "rate limit exceeded")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// rateLimiter is a simple fixed-window in-memory limiter keyed by an arbitrary
// string (client IP, or IP+account). It is intended for login and other public
// endpoints where a coarse abuse brake is enough; it needs no external store
// and resets on restart.
type rateLimiter struct {
	mu        sync.Mutex
	hits      map[string]*window
	limit     int
	window    time.Duration
	now       func() time.Time
	lastSweep time.Time
}

type window struct {
	count int
	reset time.Time
}

func newRateLimiter(limit int, per time.Duration) *rateLimiter {
	return &rateLimiter{
		hits:   map[string]*window{},
		limit:  limit,
		window: per,
		now:    time.Now,
	}
}

// allow records one hit for key and reports whether it is within the limit.
func (l *rateLimiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.sweep(now)
	w, ok := l.hits[key]
	if !ok || now.After(w.reset) {
		l.hits[key] = &window{count: 1, reset: now.Add(l.window)}
		return true
	}
	w.count++
	return w.count <= l.limit
}

// sweep removes expired windows so the map cannot grow without bound as new
// source IPs appear. It is opportunistic: because every request already takes
// the lock in allow, we piggy-back the cleanup here instead of running a
// separate goroutine, and rate-limit the scan itself to at most once per
// window so a burst of distinct keys stays O(1) amortised. Callers must hold
// l.mu.
func (l *rateLimiter) sweep(now time.Time) {
	if now.Sub(l.lastSweep) < l.window {
		return
	}
	l.lastSweep = now
	for k, w := range l.hits {
		if now.After(w.reset) {
			delete(l.hits, k)
		}
	}
}

// lockout tracks consecutive failed logins and locks the bucket after a
// threshold for a cool-off period, blunting online password guessing.
//
// The bucket is keyed by (submitted login, client IP), not by account id, for
// two reasons that the account-id version got wrong.
//
// It was an existence oracle: the lockout could only be consulted after the
// account had been found, so five failures against a real address started
// answering 429 while a made-up address kept answering 401. Anyone could sort
// real addresses from invented ones. Keying on what was TYPED means both
// behave identically, because the counter exists before any lookup does.
//
// And it was a denial of service: five deliberate failures locked a known
// account for everyone, including its owner. Including the IP scopes that to
// the attacker's own bucket. Distributed guessing is correspondingly less
// constrained here, which is what the per-IP request limiter and argon2 are
// for; a lockout that any stranger can trigger against any account is the
// worse of the two problems.
type lockout struct {
	mu        sync.Mutex
	fails     map[string]*failCounter
	threshold int
	duration  time.Duration
	now       func() time.Time
	lastSweep time.Time
}

type failCounter struct {
	count    int
	lockedTo time.Time
	// seen is when this bucket last recorded a failure. It is what makes an
	// entry expirable at all: reset only ever runs after a SUCCESSFUL login, so
	// without a timestamp a bucket that only ever fails has nothing to age out
	// on, and the key is (typed login, client IP) — attacker-chosen on both
	// halves, from unauthenticated traffic.
	seen time.Time
}

func newLockout(threshold int, duration time.Duration) *lockout {
	return &lockout{
		fails:     map[string]*failCounter{},
		threshold: threshold,
		duration:  duration,
		now:       time.Now,
	}
}

// sweep removes buckets that are neither currently locked nor touched within
// the last duration, so the map cannot grow without bound as new (login, IP)
// pairs appear. It mirrors rateLimiter.sweep exactly: opportunistic, under the
// lock the caller already holds, and rate-limited to once per duration so a
// burst of distinct keys stays O(1) amortised. Callers must hold l.mu.
func (l *lockout) sweep(now time.Time) {
	if now.Sub(l.lastSweep) < l.duration {
		return
	}
	l.lastSweep = now
	for k, f := range l.fails {
		if now.Before(f.lockedTo) || now.Sub(f.seen) < l.duration {
			continue
		}
		delete(l.fails, k)
	}
}

// locked reports whether key is currently locked out.
func (l *lockout) locked(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.sweep(now)
	f, ok := l.fails[key]
	if !ok {
		return false
	}
	return now.Before(f.lockedTo)
}

// fail records a failed login and returns whether key is now locked.
func (l *lockout) fail(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.sweep(now)
	f, ok := l.fails[key]
	if !ok {
		f = &failCounter{}
		l.fails[key] = f
	} else if now.Sub(f.seen) >= l.duration {
		// Decay. The threshold counts failures within one window, not failures
		// ever: without this, four attempts a year apart still added up towards
		// the fifth. The decay is applied here rather than left to sweep because
		// sweep runs at most once per duration, so a stale bucket can still be
		// live when this is called.
		f.count = 0
	}
	f.seen = now
	f.count++
	if f.count >= l.threshold {
		f.lockedTo = now.Add(l.duration)
		f.count = 0
		return true
	}
	return false
}

// reset clears the failure counter after a successful login.
func (l *lockout) reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.fails, key)
}

// loginKey is the lockout bucket for one login attempt: the identifier as
// typed, case-folded, plus the client IP. Folding matters because the login
// itself resolves an address or username case-insensitively, so "Bob@x" and
// "bob@x" must not be two budgets.
func loginKey(login, ip string) string {
	return strings.ToLower(strings.TrimSpace(login)) + "|" + ip
}
