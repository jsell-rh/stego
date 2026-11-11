package declaration

import (
	"fmt"
	"regexp"
	"sort"

	"github.com/jsell-rh/stego/internal/types"
)

// MethodKind is the kind of a gRPC method of a proto file: unary, a server stream, a client stream or a stream both
// ways.
type MethodKind string

// The kinds of a gRPC method.
const (
	MethodUnary        MethodKind = "unary"
	MethodServerStream MethodKind = "server stream"
	MethodClientStream MethodKind = "client stream"
	MethodBidiStream   MethodKind = "bidirectional stream"
)

// methodName is the form of a Method: package.Service/Method, with a package of one or more dotted names.
var methodName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(\.[A-Za-z_][A-Za-z0-9_]*)+/[A-Za-z_][A-Za-z0-9_]*$`)

// validateGRPCMethods checks the gRPC method of each command and query (ST-DECL-INV73): a command's Method names a
// unary method, and a query's Method a unary method or a server stream, of a service in a proto file that the
// grpc-application component declares; a Method is given only when that component is active (facts.GRPCMethods is
// not nil), and no two commands or queries name the same Method. Each fault names the method.
func validateGRPCMethods(service *types.ServiceDeclaration, facts Facts) []ValidationError {
	type operation struct {
		kind, name, path, method string
		allowed                  []MethodKind
	}
	var operations []operation
	for i, c := range service.Commands {
		operations = append(operations, operation{"command", c.Name, commandPath(i) + ".method", c.Method, []MethodKind{MethodUnary}})
	}
	for i, q := range service.Queries {
		operations = append(operations, operation{"query", q.Name, queryPath(i) + ".method", q.Method, []MethodKind{MethodUnary, MethodServerStream}})
	}
	var faults []ValidationError
	first := map[string]string{}
	for _, op := range operations {
		if op.method == "" {
			continue
		}
		fault := func(format string, args ...any) {
			faults = append(faults, ValidationError{Category: op.kind, Rule: op.kind + "-method", Path: op.path, Found: op.method,
				Message: fmt.Sprintf("%s %q has the gRPC method %s, which ", op.kind, op.name, op.method) + fmt.Sprintf(format, args...)})
		}
		kind, declared := facts.GRPCMethods[op.method]
		switch {
		case facts.GRPCMethods == nil:
			fault("needs the grpc-application component; add the component or remove the method")
			continue
		case !methodName.MatchString(op.method):
			fault("is not package.Service/Method")
			continue
		case !declared:
			fault("no service of the proto files of the grpc-application component declares; declare it there or name a declared method (known: %v)", knownMethods(facts.GRPCMethods))
			continue
		}
		allowed := false
		for _, k := range op.allowed {
			allowed = allowed || k == kind
		}
		if !allowed {
			fault("is a %s; a %s is served by %s", kind, op.kind, allowedKinds(op.allowed))
		}
		if other, used := first[op.method]; used {
			fault("is already the method of %s; give each command and query its own method", other)
			continue
		}
		first[op.method] = fmt.Sprintf("%s %q", op.kind, op.name)
	}
	return faults
}

// QueryStreams gives the names of the queries whose gRPC method is a server stream of the proto files of the
// grpc-application component (ST-DECL-INV73, ST-SUP-RC33-OC08). The compiler marks each as a stream query in
// gen.Context.QueryStreams.
func QueryStreams(service *types.ServiceDeclaration, facts Facts) map[string]bool {
	streams := map[string]bool{}
	for _, q := range service.Queries {
		if q.Method != "" && facts.GRPCMethods[q.Method] == MethodServerStream {
			streams[q.Name] = true
		}
	}
	return streams
}

// knownMethods lists the declared methods in order, at most ten, for a fault.
func knownMethods(methods map[string]MethodKind) []string {
	names := make([]string, 0, len(methods))
	for name := range methods {
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) > 10 {
		names = append(names[:10], "...")
	}
	return names
}

// allowedKinds names the kinds of method that an operation may have.
func allowedKinds(kinds []MethodKind) string {
	if len(kinds) == 1 {
		return "a " + string(kinds[0]) + " method"
	}
	return "a " + string(kinds[0]) + " method or a " + string(kinds[1])
}
