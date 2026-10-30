// Package testutil holds the small pieces of test support that more than one
// package needs and that cannot live in a _test.go file — because they depend
// on a build tag, which applies per file rather than per declaration.
package testutil

import "time"

// Wait scales a test's wall-clock patience to the runtime it is running on.
//
// Tests here wait for work happening on other goroutines, and every such wait
// needs a deadline or a hang becomes a hang rather than a failure. The
// deadlines were written against an ordinary build; under the race detector the
// same work takes several times longer, and on a single-CPU runner longer
// again. A limit that is generous locally is then a coin toss in CI, and the
// failure it produces — "timed out waiting for heartbeats" — reads as a product
// bug, which is the expensive part: it sends you looking for a deadlock that
// does not exist.
//
// Scaling the patience, not the assertion. The tests still fail if the work
// genuinely never happens; they just stop failing because the machine was slow.
func Wait(d time.Duration) time.Duration { return time.Duration(float64(d) * waitScale) }
