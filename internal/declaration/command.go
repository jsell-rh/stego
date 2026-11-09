package declaration

import (
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jsell-rh/stego/internal/types"
)

func commandPath(i int) string { return fmt.Sprintf("commands[%d]", i) }

// validateCommandNames checks that each command's name is unique in the declaration (ST-DECL-INV37).
func validateCommandNames(commands []types.Command) []ValidationError {
	var faults []ValidationError
	first := map[string]int{}
	for i, command := range commands {
		if command.Name == "" {
			faults = append(faults, ValidationError{Category: "command", Rule: "command-name", Path: commandPath(i) + ".name", Message: "the command has no name; give it a name that is unique in the declaration"})
			continue
		}
		if j, used := first[command.Name]; used {
			faults = append(faults, ValidationError{Category: "command", Rule: "command-name", Path: commandPath(i) + ".name", Found: command.Name, Message: fmt.Sprintf("the command name %q is used twice, by %s and %s; give each command its own name", command.Name, commandPath(j), commandPath(i))})
			continue
		}
		first[command.Name] = i
	}
	return faults
}

// validateCommandWrites checks that each command names at least one entity it writes, and only entities that the
// declaration declares (ST-DECL-INV38).
func validateCommandWrites(commands []types.Command, entities []types.Entity) []ValidationError {
	var faults []ValidationError
	for i, command := range commands {
		faults = append(faults, entityFaults("command", command.Name, commandPath(i), "writes", command.Writes, entities)...)
	}
	return faults
}

// validateCommandFill checks that each command names exactly one fill and that the fill implements the command
// slot (ST-DECL-INV39).
func validateCommandFill(commands []types.Command, facts Facts) []ValidationError {
	var faults []ValidationError
	for i, command := range commands {
		faults = append(faults, fillFault("command", command.Name, commandPath(i), command.Fill, CommandSlot, facts)...)
	}
	return faults
}

// reservedDestinations are the outbox destinations that no effect may name: a relationship tuple goes only through
// the authorization handle, and an audit row only through the audit runtime.
var reservedDestinations = map[string]bool{"authz": true, "audit": true}

// validateCommandEffects checks each effect of each command (ST-DECL-INV42): its destination is one that an active
// component declares or that a worker of the declaration consumes, it is not authz or audit, and its effect name is
// unique in the command.
func validateCommandEffects(commands []types.Command, facts Facts) []ValidationError {
	var faults []ValidationError
	for i, command := range commands {
		faults = append(faults, effectFaults("command", command.Name, commandPath(i), command.Effects, "", facts)...)
	}
	return faults
}

// effectFaults checks the effects of one command or worker. own is the destination that a worker consumes itself,
// which it may not stage for; a command has none.
func effectFaults(kind, name, path string, effects []types.Effect, own string, facts Facts) []ValidationError {
	rule := kind + "-effect"
	known := map[string]bool{}
	for destination := range facts.Destinations {
		known[destination] = true
	}
	var faults []ValidationError
	seen := map[string]bool{}
	for i, effect := range effects {
		at := fmt.Sprintf("%s.effects[%d]", path, i)
		switch {
		case effect.Destination == "" || effect.Name == "":
			faults = append(faults, ValidationError{Category: kind, Rule: rule, Path: at, Found: effect.String(), Message: fmt.Sprintf("%s %q has the effect %q with no destination or no name; give both", kind, name, effect.String())})
		case reservedDestinations[effect.Destination]:
			faults = append(faults, ValidationError{Category: kind, Rule: rule, Path: at + ".destination", Found: effect.Destination, Message: fmt.Sprintf("%s %q stages the effect %q for %s, which is not an effect: a relationship goes only through the authorization handle and an audit row only through the audit runtime", kind, name, effect.String(), effect.Destination)})
		case own != "" && effect.Destination == own:
			faults = append(faults, ValidationError{Category: kind, Rule: rule, Path: at + ".destination", Found: effect.Destination, Message: fmt.Sprintf("%s %q stages the effect %q for the destination it consumes itself; stage it for another destination", kind, name, effect.String())})
		case !known[effect.Destination]:
			faults = append(faults, ValidationError{Category: kind, Rule: rule, Path: at + ".destination", Found: effect.Destination, Allowed: sortedNames(known), Message: fmt.Sprintf("%s %q stages the effect %q for the destination %q, which no active component declares and no worker consumes", kind, name, effect.String(), effect.Destination)})
		}
		if effect.Name != "" {
			if seen[effect.Name] {
				faults = append(faults, ValidationError{Category: kind, Rule: rule, Path: at + ".name", Found: effect.Name, Message: fmt.Sprintf("%s %q names the effect %q twice; each effect name is part of an outgoing idempotency key, so it must be unique in the %s", kind, name, effect.Name, kind)})
			}
			seen[effect.Name] = true
		}
	}
	return faults
}

// operationRoute is one command or query route, with the place that declares it.
type operationRoute struct {
	kind, name, path, route string
}

// validateOperationRoutes checks that each command declares a route with POST and each query a route with GET, that
// each route is a valid pattern under the base path, and that no two of them conflict (ST-DECL-INV47). The compile
// check holds them against the routes of the active components too.
func validateOperationRoutes(service *types.ServiceDeclaration) []ValidationError {
	var routes []operationRoute
	for i, command := range service.Commands {
		routes = append(routes, operationRoute{"command", command.Name, commandPath(i) + ".route", command.Route})
	}
	for i, query := range service.Queries {
		routes = append(routes, operationRoute{"query", query.Name, queryPath(i) + ".route", query.Route})
	}
	var faults []ValidationError
	mux := http.NewServeMux()
	for _, r := range routes {
		method := map[string]string{"command": http.MethodPost, "query": http.MethodGet}[r.kind]
		if r.route == "" {
			faults = append(faults, ValidationError{Category: r.kind, Rule: "operation-route", Path: r.path, Message: fmt.Sprintf("%s %q declares no route; give it a route such as %s /path", r.kind, r.name, method)})
			continue
		}
		verb, routePath, _ := strings.Cut(r.route, " ")
		if verb != method {
			faults = append(faults, ValidationError{Category: r.kind, Rule: "operation-route", Path: r.path, Found: r.route, Allowed: []string{method}, Message: fmt.Sprintf("%s %q has the route %q; a %s is served with the method %s", r.kind, r.name, r.route, r.kind, method)})
			continue
		}
		if !strings.HasPrefix(routePath, "/") {
			faults = append(faults, ValidationError{Category: r.kind, Rule: "operation-route", Path: r.path, Found: r.route, Message: fmt.Sprintf("%s %q has the route %q, whose path does not start with /", r.kind, r.name, r.route)})
			continue
		}
		if err := registerRoute(mux, OperationPattern(service.BasePath, r.route)); err != nil {
			faults = append(faults, ValidationError{Category: r.kind, Rule: "operation-route", Path: r.path, Found: r.route, Message: fmt.Sprintf("%s %q has the route %q: %v", r.kind, r.name, r.route, err)})
		}
	}
	return faults
}

// OperationPattern gives the pattern that serves a command or query route under the service's base path, as the
// HTTP multiplexer reads it.
func OperationPattern(basePath, route string) string {
	verb, routePath, _ := strings.Cut(route, " ")
	return verb + " " + strings.TrimSuffix(basePath, "/") + routePath
}

// registerRoute registers a pattern on mux, and gives the multiplexer's refusal of a pattern that is not valid or
// conflicts with one that it holds as an error.
func registerRoute(mux *http.ServeMux, pattern string) (err error) {
	defer func() {
		if failure := recover(); failure != nil {
			err = fmt.Errorf("%v", failure)
		}
	}()
	mux.HandleFunc(pattern, func(http.ResponseWriter, *http.Request) {})
	return nil
}

var (
	refusalName = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)
	refusalCode = regexp.MustCompile(`^[a-z][a-z0-9_.]{0,62}$`)
	// reservedCodes are the codes that the handler keeps for an unexpected error and for a conflict.
	reservedCodes    = map[string]bool{"internal": true, "conflict": true}
	refusalStatuses  = []int{400, 403, 404, 409, 412, 422}
	refusalStatusSet = map[int]bool{400: true, 403: true, 404: true, 409: true, 412: true, 422: true}
)

// validateOperationRefusals checks the refusals of each command and query (ST-DECL-INV56): a name in lower snake
// case and a well-formed code, each unique in its command or query, a code that is not reserved, and a known status.
func validateOperationRefusals(service *types.ServiceDeclaration) []ValidationError {
	var faults []ValidationError
	for i, command := range service.Commands {
		faults = append(faults, refusalFaults("command", command.Name, commandPath(i), command.Refusals)...)
	}
	for i, query := range service.Queries {
		faults = append(faults, refusalFaults("query", query.Name, queryPath(i), query.Refusals)...)
	}
	return faults
}

func refusalFaults(kind, name, path string, refusals []types.Refusal) []ValidationError {
	var faults []ValidationError
	names, codes := map[string]bool{}, map[string]bool{}
	statuses := make([]string, len(refusalStatuses))
	for i, status := range refusalStatuses {
		statuses[i] = strconv.Itoa(status)
	}
	for i, refusal := range refusals {
		at := fmt.Sprintf("%s.refusals[%d]", path, i)
		fault := func(field, found, message string, allowed ...string) {
			faults = append(faults, ValidationError{Category: kind, Rule: "operation-refusal", Path: at + "." + field, Found: found, Allowed: allowed, Message: fmt.Sprintf("%s %q has the refusal %q: %s", kind, name, refusal.Name, message)})
		}
		switch {
		case !refusalName.MatchString(refusal.Name):
			fault("name", refusal.Name, "its name must be in lower snake case")
		case names[refusal.Name]:
			fault("name", refusal.Name, "its name is used twice; give each refusal its own name")
		}
		names[refusal.Name] = true
		switch {
		case reservedCodes[refusal.Code]:
			fault("code", refusal.Code, fmt.Sprintf("the code %q is reserved for the handler; choose another code", refusal.Code))
		case !refusalCode.MatchString(refusal.Code):
			fault("code", refusal.Code, fmt.Sprintf("the code %q must match %s", refusal.Code, refusalCode))
		case codes[refusal.Code]:
			fault("code", refusal.Code, fmt.Sprintf("the code %q is used twice; give each refusal its own code", refusal.Code))
		}
		codes[refusal.Code] = true
		if !refusalStatusSet[refusal.Status] {
			fault("status", strconv.Itoa(refusal.Status), fmt.Sprintf("the status %d is not one a refusal may answer with", refusal.Status), statuses...)
		}
	}
	return faults
}

// validateCommandRetry checks that each command's Retry on conflict is a whole number from 0 to 3 and its Lock
// timeout a duration from 100 ms to 10 s (ST-DECL-INV80).
func validateCommandRetry(commands []types.Command) []ValidationError {
	var faults []ValidationError
	for i, command := range commands {
		if command.RetryOnConflict != "" {
			if retries, err := strconv.Atoi(command.RetryOnConflict); err != nil || retries < 0 || retries > 3 {
				faults = append(faults, ValidationError{Category: "command", Rule: "command-retry", Path: commandPath(i) + ".retry_on_conflict", Found: command.RetryOnConflict, Allowed: []string{"0", "1", "2", "3"}, Message: fmt.Sprintf("command %q has the retry on conflict %q; it must be a whole number from 0 to 3", command.Name, command.RetryOnConflict)})
			}
		}
		if command.LockTimeout != "" {
			if timeout, err := time.ParseDuration(command.LockTimeout); err != nil || timeout < 100*time.Millisecond || timeout > 10*time.Second {
				faults = append(faults, ValidationError{Category: "command", Rule: "command-lock-timeout", Path: commandPath(i) + ".lock_timeout", Found: command.LockTimeout, Message: fmt.Sprintf("command %q has the lock timeout %q; it must be a duration from 100ms to 10s", command.Name, command.LockTimeout)})
			}
		}
	}
	return faults
}
