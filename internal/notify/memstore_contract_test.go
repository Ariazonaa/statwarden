package notify_test

import (
	"testing"
	"time"

	"github.com/Ariazonaa/statwarden/internal/notify"
	"github.com/Ariazonaa/statwarden/internal/notify/storetest"
)

// MemStore against the shared contract. The same table runs against PgStore
// behind the integration tag; see internal/api/pgstore_contract_integration_test.go.
//
// Running it here is what makes the PgStore side worth anything: a bug in the
// assertions is found on this machine, so a failure over there means the
// implementations differ rather than that the test is wrong.
func TestMemStoreSatisfiesTheContract(t *testing.T) {
	storetest.Run(t, func(t *testing.T) storetest.Fixture {
		store := notify.NewMemStore()
		var seq int64
		skew := time.Duration(0)
		return storetest.Fixture{
			Store: store,
			Monitor: func(*testing.T, string) int64 {
				seq++
				return seq
			},
			Channel: func(*testing.T, string) int64 {
				seq++
				return seq
			},
			ExpireLeases: func(*testing.T) {
				// Each call moves further, so repeated expiry keeps working: a
				// claim re-leases the row past a fixed instant, and a frozen
				// clock would silently stop expiring anything after the first.
				skew += 24 * time.Hour
				at := time.Now().Add(skew)
				store.SetOutboxClock(func() time.Time { return at })
			},
		}
	})
}
