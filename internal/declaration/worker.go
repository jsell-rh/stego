package declaration

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jsell-rh/stego/internal/types"
)

func workerPath(i int) string { return fmt.Sprintf("workers[%d]", i) }

var (
	kebabName    = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	everyTrigger = regexp.MustCompile(`^every ([0-9]+)(s|m|h)$`)
)

const (
	minInterval = 10 * time.Second
	maxInterval = 24 * time.Hour
	minLease    = 15 * time.Second
	maxLease    = 5 * time.Minute
	// maxWorkers is the most workers a declaration may have, so that each built-in worker counter stays within the
	// series limit of each instrument (ST-SUP-RC17-OC17).
	maxWorkers = 127
)

// workerTrigger is a worker's trigger as checking reads it: an interval, or the destination it consumes.
type workerTrigger struct {
	every    time.Duration
	consumes string
}

// parseTrigger reads every <interval> or consumes <destination>, and gives a reason when the trigger is neither.
func parseTrigger(trigger string) (workerTrigger, string) {
	if match := everyTrigger.FindStringSubmatch(trigger); match != nil {
		count, err := strconv.Atoi(match[1])
		if err != nil {
			return workerTrigger{}, "the interval is not a whole number"
		}
		unit := map[string]time.Duration{"s": time.Second, "m": time.Minute, "h": time.Hour}[match[2]]
		every := time.Duration(count) * unit
		if count > int(maxInterval/unit) || every < minInterval {
			return workerTrigger{}, "the interval must be from 10s to 24h"
		}
		return workerTrigger{every: every}, ""
	}
	if destination, found := strings.CutPrefix(trigger, "consumes "); found {
		if !kebabName.MatchString(destination) {
			return workerTrigger{}, "the destination must be in lower kebab case"
		}
		return workerTrigger{consumes: destination}, ""
	}
	return workerTrigger{}, "a trigger is every <interval>, with whole seconds, minutes or hours from 10s to 24h, or consumes <destination>"
}

// consumedDestinations gives the destination that each worker with a well-formed consumes trigger consumes, by the
// worker's name.
func consumedDestinations(workers []types.Worker) map[string]string {
	consumed := map[string]string{}
	for _, worker := range workers {
		if trigger, reason := parseTrigger(worker.Trigger); reason == "" && trigger.consumes != "" {
			consumed[worker.Name] = trigger.consumes
		}
	}
	return consumed
}

// validateWorkerNames checks that each worker's name is in lower kebab case, is unique in the declaration, and is
// not the name of a command, a query or a controller (ST-DECL-INV65).
func validateWorkerNames(service *types.ServiceDeclaration) []ValidationError {
	others := map[string]string{}
	for _, command := range service.Commands {
		others[command.Name] = "command"
	}
	for _, query := range service.Queries {
		others[query.Name] = "query"
	}
	for _, controller := range service.Controllers {
		others[controller.Name] = "controller"
	}
	var faults []ValidationError
	seen := map[string]bool{}
	for i, worker := range service.Workers {
		path := workerPath(i) + ".name"
		switch {
		case !kebabName.MatchString(worker.Name):
			faults = append(faults, ValidationError{Category: "worker", Rule: "worker-name", Path: path, Found: worker.Name, Message: fmt.Sprintf("the worker name %q must match %s: lower kebab case, at most 63 characters", worker.Name, kebabName)})
		case seen[worker.Name]:
			faults = append(faults, ValidationError{Category: "worker", Rule: "worker-name", Path: path, Found: worker.Name, Message: fmt.Sprintf("the worker name %q is used twice; give each worker its own name", worker.Name)})
		case others[worker.Name] != "":
			faults = append(faults, ValidationError{Category: "worker", Rule: "worker-name", Path: path, Found: worker.Name, Message: fmt.Sprintf("the worker name %q is the name of a %s; give the worker a name that no command, query or controller uses", worker.Name, others[worker.Name])})
		}
		seen[worker.Name] = true
	}
	return faults
}

// validateWorkerTrigger checks that each worker has a known trigger (ST-DECL-INV66): every <interval> from 10s to
// 24h, or consumes <destination> of a destination that is not authz or audit, that no active component declares
// and that no other worker consumes.
func validateWorkerTrigger(workers []types.Worker, facts Facts) []ValidationError {
	var faults []ValidationError
	consumer := map[string]string{}
	for i, worker := range workers {
		path := workerPath(i) + ".trigger"
		fault := func(message string) {
			faults = append(faults, ValidationError{Category: "worker", Rule: "worker-trigger", Path: path, Found: worker.Trigger, Message: fmt.Sprintf("worker %q has the trigger %q: %s", worker.Name, worker.Trigger, message)})
		}
		trigger, reason := parseTrigger(worker.Trigger)
		switch {
		case reason != "":
			fault(reason)
		case trigger.consumes == "":
		case reservedDestinations[trigger.consumes]:
			fault("a worker cannot consume authz or audit")
		case facts.Destinations[trigger.consumes]:
			fault("an active component declares this destination, so it delivers the messages; consume a destination of the service's own")
		case consumer[trigger.consumes] != "":
			fault(fmt.Sprintf("the worker %q consumes this destination too; each destination has one consumer", consumer[trigger.consumes]))
		default:
			consumer[trigger.consumes] = worker.Name
		}
	}
	return faults
}

// validateWorkerWrites checks that each worker names at least one entity it reads or writes, and only entities
// that the declaration declares (ST-DECL-INV67).
func validateWorkerWrites(workers []types.Worker, entities []types.Entity) []ValidationError {
	var faults []ValidationError
	for i, worker := range workers {
		faults = append(faults, entityFaults("worker", worker.Name, workerPath(i), "writes", worker.Writes, entities)...)
	}
	return faults
}

// validateWorkerFill checks that each worker names exactly one fill and that the fill implements the worker slot
// (ST-DECL-INV68).
func validateWorkerFill(workers []types.Worker, facts Facts) []ValidationError {
	var faults []ValidationError
	for i, worker := range workers {
		faults = append(faults, fillFault("worker", worker.Name, workerPath(i), worker.Fill, WorkerSlot, facts)...)
	}
	return faults
}

// stagingFacts gives the facts with the destinations that the workers of the declaration consume added to the
// destinations that the active components declare: an effect may name either.
func stagingFacts(workers []types.Worker, facts Facts) Facts {
	known := Facts{FillSlots: facts.FillSlots, Destinations: map[string]bool{}}
	for destination := range facts.Destinations {
		known.Destinations[destination] = true
	}
	for _, destination := range consumedDestinations(workers) {
		known.Destinations[destination] = true
	}
	return known
}

// validateWorkerEffects checks each effect of each worker (ST-DECL-INV70): its destination is one that an active
// component declares or that another worker consumes, it is not authz, audit or the destination that the worker
// consumes itself, and its effect name is unique in the worker.
func validateWorkerEffects(workers []types.Worker, facts Facts) []ValidationError {
	known := stagingFacts(workers, facts)
	consumed := consumedDestinations(workers)
	var faults []ValidationError
	for i, worker := range workers {
		faults = append(faults, effectFaults("worker", worker.Name, workerPath(i), worker.Effects, consumed[worker.Name], known)...)
	}
	return faults
}

// RunsMode tells if a declaration runs a mode: when its Modes name it, or when it names no mode, so it runs all.
func RunsMode(service *types.ServiceDeclaration, mode string) bool {
	return len(service.Modes) == 0 || contains(service.Modes, mode)
}

// validateWorkerMode checks that a declaration with a worker runs the worker mode (ST-DECL-INV71).
func validateWorkerMode(service *types.ServiceDeclaration) []ValidationError {
	if len(service.Workers) == 0 || RunsMode(service, "worker") {
		return nil
	}
	var names []string
	for _, worker := range service.Workers {
		names = append(names, worker.Name)
	}
	return []ValidationError{{Category: "worker", Rule: "worker-mode", Path: "modes", Found: strings.Join(service.Modes, ", "), Allowed: []string{"api", "worker", "controller"}, Message: fmt.Sprintf("the declaration has the workers %s, but its modes %s do not hold worker; add worker to modes, or leave modes out to run all", strings.Join(names, ", "), strings.Join(service.Modes, ", "))}}
}

// validateWorkerLease checks that a worker's Single and Lease TTL fit its trigger (ST-DECL-INV74): a worker with an
// every trigger may have a Lease TTL from 15s to 5m only when Single is yes, and a worker with a consumes trigger
// gives neither.
func validateWorkerLease(workers []types.Worker) []ValidationError {
	var faults []ValidationError
	for i, worker := range workers {
		trigger, reason := parseTrigger(worker.Trigger)
		if reason != "" {
			continue
		}
		fault := func(field, found, message string) {
			label := map[string]string{"single": "single", "lease_ttl": "lease TTL"}[field]
			faults = append(faults, ValidationError{Category: "worker", Rule: "worker-lease", Path: workerPath(i) + "." + field, Found: found, Message: fmt.Sprintf("worker %q has the %s %q: %s", worker.Name, label, found, message)})
		}
		if trigger.consumes != "" {
			if worker.Single != nil {
				fault("single", strconv.FormatBool(*worker.Single), "a worker that consumes a destination omits single, because the outbox gives each message to one instance under a lease")
			}
			if worker.LeaseTTL != "" {
				fault("lease_ttl", worker.LeaseTTL, "a worker that consumes a destination omits lease_ttl, because the outbox gives each message to one instance under a lease")
			}
			continue
		}
		if worker.LeaseTTL == "" {
			continue
		}
		if worker.Single != nil && !*worker.Single {
			fault("lease_ttl", worker.LeaseTTL, "only a single worker holds a lease; remove lease_ttl, or leave single out")
			continue
		}
		if ttl, err := time.ParseDuration(worker.LeaseTTL); err != nil || ttl < minLease || ttl > maxLease {
			fault("lease_ttl", worker.LeaseTTL, "the lease TTL must be a duration from 15s to 5m")
		}
	}
	return faults
}

// WorkerSingle tells if one instance of the service runs the worker at a time: yes for a worker with an every
// trigger when Single is omitted.
func WorkerSingle(worker types.Worker) bool {
	trigger, _ := parseTrigger(worker.Trigger)
	if trigger.consumes != "" {
		return false
	}
	return worker.Single == nil || *worker.Single
}

// WorkerLeaseTTL gives the lease TTL of a single worker: its Lease TTL, or 15s when it is omitted.
func WorkerLeaseTTL(worker types.Worker) string {
	if worker.LeaseTTL == "" {
		return minLease.String()
	}
	return worker.LeaseTTL
}

// validateWorkerCycles checks that staging and consuming form no cycle (ST-DECL-INV76): the graph whose nodes are
// the destinations that workers consume, with an edge from X to Y when the worker that consumes X stages an effect
// for Y. A fault names each cycle once, as its destinations and workers in order, from the destination that sorts
// first.
func validateWorkerCycles(workers []types.Worker) []ValidationError {
	consumer := map[string]types.Worker{}
	for _, worker := range workers {
		if trigger, reason := parseTrigger(worker.Trigger); reason == "" && trigger.consumes != "" {
			if _, taken := consumer[trigger.consumes]; !taken {
				consumer[trigger.consumes] = worker
			}
		}
	}
	next := func(destination string) []string {
		var targets []string
		for _, effect := range consumer[destination].Effects {
			if _, consumed := consumer[effect.Destination]; consumed && !contains(targets, effect.Destination) {
				targets = append(targets, effect.Destination)
			}
		}
		sort.Strings(targets)
		return targets
	}
	destinations := make([]string, 0, len(consumer))
	for destination := range consumer {
		destinations = append(destinations, destination)
	}
	sort.Strings(destinations)
	var faults []ValidationError
	reported := map[string]bool{}
	for i, start := range destinations {
		// Find the cycles through start whose other destinations all sort after it, so each cycle is found once,
		// from its first destination.
		later := map[string]bool{}
		for _, destination := range destinations[i+1:] {
			later[destination] = true
		}
		var walk func(path []string)
		walk = func(path []string) {
			for _, target := range next(path[len(path)-1]) {
				switch {
				case target == start:
					var parts []string
					for _, destination := range path {
						parts = append(parts, destination, consumer[destination].Name)
					}
					cycle := strings.Join(append(parts, start), ", ")
					if !reported[cycle] {
						reported[cycle] = true
						faults = append(faults, ValidationError{Category: "worker", Rule: "worker-cycle", Path: "workers", Found: cycle, Message: fmt.Sprintf("the workers stage and consume in a cycle: %s; a message would go round it for ever, so break the cycle", cycle)})
					}
				case later[target] && !contains(path, target):
					walk(append(append([]string(nil), path...), target))
				}
			}
		}
		walk([]string{start})
	}
	return faults
}

// validateWorkerCount checks that a declaration has at most 127 workers (ST-DECL-INV79).
func validateWorkerCount(workers []types.Worker) []ValidationError {
	if len(workers) <= maxWorkers {
		return nil
	}
	return []ValidationError{{Category: "worker", Rule: "worker-count", Path: "workers", Found: strconv.Itoa(len(workers)), Message: fmt.Sprintf("the declaration has %d workers; it may have at most %d, so that each built-in worker counter stays within the series limit", len(workers), maxWorkers)}}
}
