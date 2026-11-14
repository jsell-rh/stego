package declaration

import (
	"fmt"
	"time"

	"github.com/jsell-rh/stego/internal/types"
)

// The ranges of the numeric and duration settings of a ledger and a controller (ST-DECL-INV87). Each bound comes
// from the runtime that the setting feeds.
const (
	// maxPendingTimeout is the longest pending timeout of a ledger: one year. A hold that stays open longer is not a
	// pending transfer, and a value such as 15000h is a typing fault.
	maxPendingTimeout = 365 * 24 * time.Hour
	// maxSnapshotEvery is the largest snapshot interval of a ledger: a balance read sums every posted entry of the
	// account after its latest snapshot, so the interval bounds the rows that one read sums.
	maxSnapshotEvery = 10000
	// minResync and maxResync are the range of a controller's resync: the range of the resync interval of the
	// controller runtime, which refuses to start outside it.
	minResync = time.Millisecond
	maxResync = 24 * time.Hour
	// maxReplicas is the most replicas of a controller: one replica holds the lease, and each other one only polls
	// the lease row at every retry, so more replicas add load on the database and no availability.
	maxReplicas = 10
)

// validateSettingBounds holds the declaration to ST-DECL-INV87: a ledger's pending timeout and snapshot interval,
// and a controller's resync, lease TTL and replicas, stay in their ranges. A value that is not of its form, or not
// greater than zero, is refused by validateMoney or validateControllers, so this refuses only a value of the right
// form beyond its range.
func validateSettingBounds(service *types.ServiceDeclaration) []ValidationError {
	var faults []ValidationError
	for ei, e := range service.Entities {
		if !isLedger(e) || e.Ledger == nil {
			continue
		}
		path := entityPath(ei) + ".ledger"
		fault := func(field, found, format string, args ...any) {
			faults = append(faults, ValidationError{Category: "ledger", Rule: "setting-bound", Path: path + "." + field, Found: found,
				Message: fmt.Sprintf("ledger %q ", e.Name) + fmt.Sprintf(format, args...)})
		}
		if timeout, err := time.ParseDuration(e.Ledger.PendingTimeout); err == nil && timeout > maxPendingTimeout {
			fault("pending_timeout", e.Ledger.PendingTimeout, "has the pending timeout %q, which is longer than %s (one year)", e.Ledger.PendingTimeout, "8760h")
		}
		if every := e.Ledger.SnapshotEvery; every != nil && *every > maxSnapshotEvery {
			fault("snapshot_every", fmt.Sprint(*every), "takes a snapshot every %d entries, more than %d; a balance read sums every entry after the latest snapshot", *every, maxSnapshotEvery)
		}
	}
	for i, controller := range service.Controllers {
		path := fmt.Sprintf("controllers[%d]", i)
		fault := func(field, found, format string, args ...any) {
			faults = append(faults, ValidationError{Category: "service", Rule: "setting-bound", Path: path + "." + field, Found: found,
				Message: fmt.Sprintf("controller %q ", controller.Name) + fmt.Sprintf(format, args...)})
		}
		if resync, err := time.ParseDuration(controller.Resync); err == nil && resync > 0 && (resync < minResync || resync > maxResync) {
			fault("resync", controller.Resync, "has the resync %q; the resync is a duration from 1ms to 24h, the range of the controller runtime", controller.Resync)
		}
		if ttl, err := time.ParseDuration(controller.LeaseTTL); err == nil && ttl > 0 && (ttl < minLease || ttl > maxLease) {
			fault("lease_ttl", controller.LeaseTTL, "has the lease TTL %q; the lease TTL is a duration from 15s to 5m, as for a worker", controller.LeaseTTL)
		}
		if controller.Replicas > maxReplicas {
			fault("replicas", fmt.Sprint(controller.Replicas), "declares %d replicas, more than %d; one replica holds the lease and the others only stand by", controller.Replicas, maxReplicas)
		}
	}
	return faults
}
