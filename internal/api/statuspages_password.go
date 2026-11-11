package api

import (
	"strconv"
	"sync"
	"time"
)

// Page-password brakes.
//
// Every submitted page password costs one argon2id run — 64 MiB of memory-hard
// work — on an anonymous endpoint. The public rate limit alone let one address
// start sixty of them a minute, all at once, and guess for as long as it liked.
const (
	// pagePasswordSlots bounds how many page-password checks run at the same
	// time, process-wide: at most this many × 64 MiB is ever spent on them.
	pagePasswordSlots = 4
	// pagePasswordSlotWait is how long a check waits for a free slot before
	// the request is turned away with 503. A check takes tens of milliseconds,
	// so a legitimate visitor arriving during a burst still gets in.
	pagePasswordSlotWait = 2 * time.Second
	// pagePasswordRetryAfter is the Retry-After sent with that 503.
	pagePasswordRetryAfter = 1
)

// passwordGuard is the shared state behind the brakes: the slots, and a
// lockout keyed by (page slug, client IP) — the login lockout's shape, for the
// login lockout's reasons: an attacker locks only their own bucket, never a
// page for everyone.
//
// The zero value is ready to use; the fields are built on first use so the
// server needs no constructor wiring for them.
type passwordGuard struct {
	once    sync.Once
	slots   chan struct{}
	lockout *lockout
}

// pagePasswordGuard returns the server's guard, building it on first use.
func (s *server) pagePasswordGuard() *passwordGuard {
	g := &s.pagePw
	g.once.Do(func() {
		g.slots = make(chan struct{}, pagePasswordSlots)
		g.lockout = newLockout(lockoutThreshold, lockoutDuration)
		g.lockout.now = s.now
	})
	return g
}

// pagePasswordKey is the lockout bucket for one page and one client address.
func pagePasswordKey(slug, ip string) string {
	return slug + "|" + ip
}

// lockoutRetryAfter renders the Retry-After for a locked bucket. The lockout
// does not expose when a bucket opens again, so this is its full duration: an
// upper bound, which is what a client needs to back off far enough.
func lockoutRetryAfter() string {
	return strconv.Itoa(int(lockoutDuration.Seconds()))
}
