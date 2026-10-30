package engine

import "github.com/Ariazonaa/statwarden/internal/domain"

// Transition is the pure state-machine step: given the previous state, the
// consecutive-failure counter and a boolean check outcome it returns the new
// state, the new counter and whether a state transition happened.
//
// Rules per docs/architecture.md:
//
//	UP → PENDING   on the first failure (when maxRetries > 0)
//	PENDING → DOWN after the failure count exceeds maxRetries
//	* → UP         on any success (counter resets)
//
// maxRetries = 0 sends a failing monitor straight to DOWN. The initial
// "pending" DB state behaves like PENDING with retries carried in, so a brand
// new monitor follows the same path. A transition — any change of the state
// value — is what marks a heartbeat important and emits a StateChange.
//
// Transition is Step with a single-success recovery; see Step for the
// confirmed recovery out of DOWN.
func Transition(prev domain.MonitorStatus, retries, maxRetries int, up bool) (next domain.MonitorStatus, newRetries int, transitioned bool) {
	next, newRetries, _, transitioned = Step(prev, retries, 0, maxRetries, 1, up)
	return next, newRetries, transitioned
}

// Step is Transition with the recovery half of flapping damping: a DOWN
// monitor needs recoveryChecks consecutive successes before it is UP again.
//
//	DOWN → DOWN    on a success while successes < recoveryChecks (counting)
//	DOWN → UP      on the recoveryChecks-th consecutive success
//	DOWN → DOWN    on a failure, which resets the success count
//
// It only applies out of DOWN. A PENDING monitor was never announced, so one
// success is still enough to call it UP — confirming a recovery nobody was told
// needed is only delay. recoveryChecks <= 1 is the historical single-success
// behaviour, so monitors created before the setting existed behave as before.
func Step(prev domain.MonitorStatus, retries, successes, maxRetries, recoveryChecks int, up bool) (next domain.MonitorStatus, newRetries, newSuccesses int, transitioned bool) {
	if up {
		if prev == domain.StatusDown && recoveryChecks > 1 {
			successes++
			if successes < recoveryChecks {
				return domain.StatusDown, retries, successes, false
			}
		}
		return domain.StatusUp, 0, 0, prev != domain.StatusUp
	}
	if prev == domain.StatusDown {
		return domain.StatusDown, retries, 0, false
	}
	retries++
	if retries > maxRetries {
		return domain.StatusDown, retries, 0, true
	}
	return domain.StatusPending, retries, 0, prev != domain.StatusPending
}
