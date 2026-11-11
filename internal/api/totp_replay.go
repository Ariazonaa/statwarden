package api

import (
	"sync"
	"time"

	"github.com/Ariazonaa/statwarden/internal/auth"
)

// totpReplayGuard remembers, per account, the last TOTP time step a code was
// accepted for, so the same code cannot authenticate twice.
//
// A code verifies for its own 30-second step and one step either side, so for
// about ninety seconds it is a reusable password to anyone who saw it typed.
// RFC 6238 §5.2 requires the verifier to refuse a second use; refusing every
// step not later than the last accepted one does that, and also refuses an
// older code that is still inside the skew window.
//
// In memory, like the login lockout, because the window it protects is ninety
// seconds long: a restart inside that window reopens it for one code, which is
// a far smaller thing than a schema change for a value nobody reads after a
// minute and a half. The zero value is ready to use.
type totpReplayGuard struct {
	mu   sync.Mutex
	last map[int64]totpStep
}

type totpStep struct {
	step uint64
	at   time.Time
}

// totpReplayRetention is how long an accepted step stays remembered: past it,
// every code that could match the step has expired on its own.
const totpReplayRetention = 2 * time.Minute

// accept records step for userID and reports whether it is newer than the last
// accepted one. Compare and record happen under one lock, so two concurrent
// requests carrying the same code cannot both win.
func (g *totpReplayGuard) accept(userID int64, step uint64, now time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.last == nil {
		g.last = map[int64]totpStep{}
	}
	if prev, ok := g.last[userID]; ok && step <= prev.step {
		return false
	}
	g.last[userID] = totpStep{step: step, at: now}
	// Opportunistic cleanup, so the map holds only accounts that used a code
	// within the retention window.
	for id, e := range g.last {
		if now.Sub(e.at) > totpReplayRetention {
			delete(g.last, id)
		}
	}
	return true
}

// verifyTOTPOnce is the only way a handler should check an authenticator code
// for an account: it verifies the code and then claims its time step, so a code
// that has already been accepted once — for a login, a step-up or the
// enrolment itself — is refused.
func (s *server) verifyTOTPOnce(userID int64, secret, code string) bool {
	now := s.now()
	step, ok := auth.VerifyTOTPStep(secret, code, now)
	if !ok {
		return false
	}
	return s.totpReplay.accept(userID, step, now)
}
