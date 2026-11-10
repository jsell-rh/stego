package declaration

import (
	"fmt"
	"strings"
	"testing"
)

// orders is ST-DECL-TD02 with the modes given (none when empty) and the owned entity Order with the fields status
// (string) and synced (bool).
func orders(modes string) string {
	service := strings.Replace(td02, "entities:\n", "entities:\n  - name: Order\n    fields:\n      - {name: status, type: string}\n      - {name: synced, type: bool}\n", 1)
	if modes != "" {
		service += "modes: " + modes + "\n"
	}
	return service
}

// worker writes one worker with the lines given after its name.
func worker(name string, lines ...string) string {
	return "  - name: " + name + "\n    " + strings.Join(lines, "\n    ") + "\n"
}

// syncWorker is the worker sync-partner-orders with the trigger every 1m that writes Order, bound to a fill of the
// worker slot, with the effect partner-orders: order-changed, and the lines given.
func syncWorker(lines ...string) string {
	return worker("sync-partner-orders", append([]string{"trigger: every 1m", "writes: [Order]", "fill: sync", "effects:\n      - {destination: partner-orders, name: order-changed}"}, lines...)...)
}

// workers gives the declaration of orders, with the modes api and worker, and the workers given.
func workers(items ...string) string {
	return orders("[api, worker]") + "workers:\n" + strings.Join(items, "")
}

// workerFacts are the facts with the outbox destination partner-orders that an active component declares, the
// fills sync and apply of the worker slot, the fill reserve of the command slot and the fill policy of
// before_create.
func workerFacts() Facts {
	return Facts{
		FillSlots:    map[string]string{"sync": WorkerSlot, "apply": WorkerSlot, "other": WorkerSlot, "reserve": CommandSlot, "policy": "rest-api.before_create"},
		Destinations: map[string]bool{"partner-orders": true},
	}
}

func workerFaults(t *testing.T, service string, rules ...string) []ValidationError {
	t.Helper()
	return compositionFaults(t, service, workerFacts(), rules...)
}

func TestWorkerNames(t *testing.T) {
	t.Run("ST-DECL-INV65-T The worker sync-partner-orders is accepted", func(t *testing.T) {
		wantNoFault(t, workerFaults(t, workers(syncWorker())))
	})
	t.Run("ST-DECL-INV65-T Two workers named sync-partner-orders are refused", func(t *testing.T) {
		wantFault(t, workerFaults(t, workers(syncWorker(), syncWorker()), "worker-name"), `"sync-partner-orders"`)
	})
	t.Run("ST-DECL-INV65-T A worker and a command both named sync-partner-orders are refused", func(t *testing.T) {
		service := workers(syncWorker()) + "commands:\n  - name: sync-partner-orders\n    writes: [Order]\n    fill: reserve\n    route: POST /sync\n"
		wantFault(t, workerFaults(t, service, "worker-name"), `"sync-partner-orders"`)
	})
	t.Run("ST-DECL-INV65-T A worker and a controller both named sync-partner-orders are refused", func(t *testing.T) {
		service := workers(syncWorker()) + "controllers:\n  - {name: sync-partner-orders, sources: [scan, watch], resync: 10m, replicas: 2, lease_ttl: 15s, tenants: each}\n"
		wantFault(t, workerFaults(t, service, "worker-name"), `"sync-partner-orders"`)
	})
	for _, name := range []string{"SyncOrders", "9-sync", "sync_orders", strings.Repeat("s", 64)} {
		t.Run("ST-DECL-INV65-T The worker name "+name+" is refused", func(t *testing.T) {
			service := workers(worker(name, "trigger: every 1m", "writes: [Order]", "fill: sync"))
			wantFault(t, workerFaults(t, service, "worker-name"), `"`+name+`"`)
		})
	}
}

func TestWorkerTrigger(t *testing.T) {
	with := func(trigger string) string {
		return workers(worker("sync-partner-orders", trigger, "writes: [Order]", "fill: sync", "effects:\n      - {destination: partner-orders, name: order-changed}"))
	}
	for _, trigger := range []string{"trigger: every 1m", "trigger: consumes order-changes"} {
		t.Run("ST-DECL-INV66-T A worker with the "+trigger+" is accepted", func(t *testing.T) {
			wantNoFault(t, workerFaults(t, with(trigger)))
		})
	}
	for _, c := range []struct{ name, trigger string }{
		{"every 5s", "trigger: every 5s"}, {"every 25h", "trigger: every 25h"}, {"every 1.5m", "trigger: every 1.5m"},
		{"hourly", "trigger: hourly"}, {"consumes partner-orders, which a component declares", "trigger: consumes partner-orders"},
		{"consumes authz", "trigger: consumes authz"}, {"no trigger", "permission: sync"},
	} {
		t.Run("ST-DECL-INV66-T A worker with the trigger "+c.name+" is refused", func(t *testing.T) {
			wantFault(t, workerFaults(t, with(c.trigger), "worker-trigger"), `"sync-partner-orders"`)
		})
	}
	t.Run("ST-DECL-INV66-T Two workers that both consume order-changes are refused", func(t *testing.T) {
		service := workers(worker("apply-a", "trigger: consumes order-changes", "writes: [Order]", "fill: apply"), worker("apply-b", "trigger: consumes order-changes", "writes: [Order]", "fill: other"))
		wantFault(t, workerFaults(t, service, "worker-trigger"), `"apply-b"`, `"apply-a"`, "consumes order-changes")
	})
}

func TestWorkerWrites(t *testing.T) {
	with := func(writes string) string {
		return workers(worker("sync-partner-orders", "trigger: every 1m", "writes: "+writes, "fill: sync"))
	}
	t.Run("ST-DECL-INV67-T A worker that writes Order is accepted", func(t *testing.T) {
		wantNoFault(t, workerFaults(t, with("[Order]")))
	})
	t.Run("ST-DECL-INV67-T A worker that writes no entity is refused", func(t *testing.T) {
		wantFault(t, workerFaults(t, with("[]"), "worker-writes"), `"sync-partner-orders"`, "no entity")
	})
	t.Run("ST-DECL-INV67-T A worker that writes Order and Invoice, which is not declared, is refused", func(t *testing.T) {
		wantFault(t, workerFaults(t, with("[Order, Invoice]"), "worker-writes"), `"sync-partner-orders"`, `"Invoice"`)
	})
}

func TestWorkerFill(t *testing.T) {
	with := func(fill string) string {
		return workers(worker("sync-partner-orders", "trigger: every 1m", "writes: [Order]", fill))
	}
	t.Run("ST-DECL-INV68-T A worker with a fill of the worker slot is accepted", func(t *testing.T) {
		wantNoFault(t, workerFaults(t, with("fill: sync")))
	})
	for _, c := range []struct{ name, fill, names string }{
		{"no fill", "fill: []", "no fill"},
		{"two fills", "fill: [sync, apply]", `"apply"`},
		{"a fill of the command slot", "fill: reserve", `"reserve"`},
		{"a fill of before_create", "fill: policy", `"policy"`},
	} {
		t.Run("ST-DECL-INV68-T A worker with "+c.name+" is refused", func(t *testing.T) {
			wantFault(t, workerFaults(t, with(c.fill), "worker-fill"), `"sync-partner-orders"`, c.names)
		})
	}
}

func TestWorkerEffects(t *testing.T) {
	apply := func(lines ...string) string {
		return worker("apply-partner-replies", append([]string{"trigger: consumes partner-replies", "writes: [Order]", "fill: apply"}, lines...)...)
	}
	with := func(effects ...string) string {
		return workers(worker("sync-partner-orders", "trigger: every 1m", "writes: [Order]", "fill: sync", "effects:\n      - "+strings.Join(effects, "\n      - ")), apply())
	}
	for _, c := range []struct{ name, service string }{
		{"partner-orders: order-changed", with("{destination: partner-orders, name: order-changed}")},
		{"partner-replies: reply-expected, which a worker consumes", with("{destination: partner-replies, name: reply-expected}")},
	} {
		t.Run("ST-DECL-INV70-T A worker effect "+c.name+" is accepted", func(t *testing.T) {
			wantNoFault(t, workerFaults(t, c.service))
		})
	}
	for _, c := range []struct{ name, service, worker, names string }{
		{"nowhere: order-changed", with("{destination: nowhere, name: order-changed}"), "sync-partner-orders", `"nowhere"`},
		{"partner-orders: order-changed twice", with("{destination: partner-orders, name: order-changed}", "{destination: partner-orders, name: order-changed}"), "sync-partner-orders", `"order-changed"`},
		{"authz: holder", with("{destination: authz, name: holder}"), "sync-partner-orders", "only through the authorization handle"},
		{"audit: entry", with("{destination: audit, name: entry}"), "sync-partner-orders", "only through the audit runtime"},
		{"partner-replies: again from the worker that consumes it", workers(syncWorker(), apply("effects:\n      - {destination: partner-replies, name: again}")), "apply-partner-replies", "consumes itself"},
	} {
		t.Run("ST-DECL-INV70-T A worker effect "+c.name+" is refused", func(t *testing.T) {
			wantFault(t, workerFaults(t, c.service, "worker-effect"), `"`+c.worker+`"`, c.names)
		})
	}
	t.Run("ST-DECL-INV42-T A command effect for a destination that a worker consumes is accepted", func(t *testing.T) {
		service := workers(syncWorker(), apply()) + "commands:\n  - name: reserve\n    writes: [Order]\n    fill: reserve\n    route: POST /reserve\n    effects:\n      - {destination: partner-replies, name: reply}\n"
		wantNoFault(t, workerFaults(t, service))
	})
}

func TestWorkerMode(t *testing.T) {
	for _, c := range []struct{ name, modes string }{{"the modes api and worker", "[api, worker]"}, {"the modes omitted", ""}} {
		t.Run("ST-DECL-INV71-T A worker with "+c.name+" is accepted", func(t *testing.T) {
			wantNoFault(t, workerFaults(t, orders(c.modes)+"workers:\n"+syncWorker()))
		})
	}
	t.Run("ST-DECL-INV71-T A worker with the modes api only is refused", func(t *testing.T) {
		faults := workerFaults(t, orders("[api]")+"workers:\n"+syncWorker(), "worker-mode")
		wantFault(t, faults, "sync-partner-orders", "modes api")
	})
}

func TestWorkerLease(t *testing.T) {
	for _, c := range []struct {
		name  string
		lines []string
	}{{"no lease setting", nil}, {"Single no", []string{"single: false"}}, {"Lease TTL 30s", []string{"lease_ttl: 30s"}}} {
		t.Run("ST-DECL-INV74-T A worker with "+c.name+" is accepted", func(t *testing.T) {
			wantNoFault(t, workerFaults(t, workers(syncWorker(c.lines...))))
		})
	}
	t.Run("ST-DECL-INV74-T Single and Lease TTL get their defaults", func(t *testing.T) {
		declared := readDeclaration(t, workers(syncWorker()))
		if w := declared.Workers[0]; !WorkerSingle(w) || WorkerLeaseTTL(w) != "15s" {
			t.Errorf("the worker is single %v with the lease TTL %s, want true and 15s", WorkerSingle(w), WorkerLeaseTTL(w))
		}
		got := map[string]Setting{}
		for _, setting := range ResolvedSettings(declared) {
			got[setting.Path] = setting
		}
		if s := got["workers[0].single"]; s.Value != true || !s.Default {
			t.Errorf("checking reports single %+v, want the default true", s)
		}
		if s := got["workers[0].lease_ttl"]; s.Value != "15s" || !s.Default {
			t.Errorf("checking reports the lease TTL %+v, want the default 15s", s)
		}
	})
	consumer := func(line string) string {
		return workers(worker("sync-partner-orders", "trigger: consumes order-changes", "writes: [Order]", "fill: sync", line))
	}
	for _, c := range []struct{ name, service, field string }{
		{"Single no and Lease TTL 30s", workers(syncWorker("single: false", "lease_ttl: 30s")), "lease TTL"},
		{"Lease TTL 5s", workers(syncWorker("lease_ttl: 5s")), "lease TTL"},
		{"Lease TTL 10m", workers(syncWorker("lease_ttl: 10m")), "lease TTL"},
		{"consumes order-changes and Single yes", consumer("single: true"), "single"},
		{"consumes order-changes and Lease TTL 30s", consumer("lease_ttl: 30s"), "lease TTL"},
	} {
		t.Run("ST-DECL-INV74-T A worker with "+c.name+" is refused", func(t *testing.T) {
			wantFault(t, workerFaults(t, c.service, "worker-lease"), `"sync-partner-orders"`, "the "+c.field)
		})
	}
}

func TestWorkerCycles(t *testing.T) {
	stage := func(name, consumes string, effects ...string) string {
		lines := []string{"trigger: consumes " + consumes, "writes: [Order]", "fill: sync"}
		if len(effects) > 0 {
			var items []string
			for _, effect := range effects {
				destination, effectName, _ := strings.Cut(effect, ": ")
				items = append(items, fmt.Sprintf("{destination: %s, name: %s}", destination, effectName))
			}
			lines = append(lines, "effects:\n      - "+strings.Join(items, "\n      - "))
		}
		return worker(name, lines...)
	}
	cycleFaults := func(t *testing.T, service string) []ValidationError {
		t.Helper()
		return workerFaults(t, service, "worker-cycle")
	}
	t.Run("ST-DECL-INV76-T Workers that stage without a cycle are accepted", func(t *testing.T) {
		wantNoFault(t, cycleFaults(t, workers(stage("a", "x", "y: next"), stage("b", "y"))))
	})
	t.Run("ST-DECL-INV76-T A cycle of two workers is refused and named", func(t *testing.T) {
		faults := cycleFaults(t, workers(stage("a", "x", "y: next"), stage("b", "y", "x: back")))
		wantFault(t, faults, "x, a, y, b, x")
		if faults[0].Found != "x, a, y, b, x" {
			t.Errorf("the fault found %q, want the cycle x, a, y, b, x", faults[0].Found)
		}
	})
	t.Run("ST-DECL-INV76-T A cycle of three workers is refused and named", func(t *testing.T) {
		faults := cycleFaults(t, workers(stage("c", "z", "x: start"), stage("b", "y", "z: loop"), stage("a", "x", "y: next")))
		wantFault(t, faults, "x, a, y, b, z, c, x")
	})
}

func TestWorkerCount(t *testing.T) {
	many := func(n int) string {
		var items []string
		for i := 1; i <= n; i++ {
			items = append(items, worker(fmt.Sprintf("w-%d", i), "trigger: every 1m", "writes: [Order]", "fill: sync"))
		}
		return workers(items...)
	}
	t.Run("ST-DECL-INV79-T A declaration with 127 workers is accepted", func(t *testing.T) {
		wantNoFault(t, workerFaults(t, many(127)))
	})
	t.Run("ST-DECL-INV79-T A declaration with 128 workers is refused", func(t *testing.T) {
		wantFault(t, workerFaults(t, many(128), "worker-count"), "128")
	})
}

func TestWorkerEffectStagingType(t *testing.T) {
	stager := func(resourceType string) string {
		return worker("sync-documents", "trigger: every 1m", "writes: [Todo]", "fill: sync", "resource_type: "+resourceType, "permission: view", "effects:\n      - {destination: document-replies, name: reply}")
	}
	t.Run("ST-DECL-INV70-T With authorization bound, a worker on document that stages for a worker on document is accepted", func(t *testing.T) {
		expectAccepted(t, checkOperations(t, td03Staging(t, stager("document"), ""), operationFacts()))
	})
	t.Run("ST-DECL-INV70-T With authorization bound, a worker on folder that stages for a worker on document is refused", func(t *testing.T) {
		expectOneFault(t, checkOperations(t, td03Staging(t, stager("folder"), ""), operationFacts()), "worker-effect", "sync-documents", "document-replies: reply", `"folder"`, `"document"`)
	})
}
