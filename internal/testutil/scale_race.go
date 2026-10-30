//go:build race

package testutil

// The race detector costs roughly 2-20x in CPU and memory depending on the
// shape of the work. Ten is chosen to be comfortably past that on a loaded
// single-CPU runner rather than to be precise: an over-long deadline costs
// nothing on a passing test and only shows up when something is genuinely
// broken, where a few extra seconds are irrelevant next to the diagnosis.
const waitScale = 10.0
