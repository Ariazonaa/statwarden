//go:build integration

package api

import "testing"

// TestPgStoreSatisfiesTheIncidentContract runs the incident contract against
// PostgreSQL. Everything it asserts already passes against MemStore, so a
// failure here is a divergence in the SQL — a placeholder bound to the wrong
// column, or a batched query that drops or duplicates rows.
func TestPgStoreSatisfiesTheIncidentContract(t *testing.T) {
	runIncidentStoreContract(t, func(t *testing.T) Store { return newPgStore(t) })
}
