package compiler

import "slices"

// The query component, whose generator carries the reads of a service on the runtime data path with the command
// component's (ST-SUP-RC33), and the PostgreSQL adapter, which emits the tables and the migration ledger of that
// path (ST-SUP-RC01-OC41).
const (
	queryComponent   = "query"
	adapterComponent = "postgres-adapter"
)

// runtimeDataPath tells whether the compiler wires the runtime library into the service (gen.Context.RuntimeData):
// the service has data access and the command and query components, whose generated commands and queries carry all
// of it on the runtime data path, the only data path (ST-SUP-RC20-OC39). A service that declares no entity, no
// command and no query has no data access and gets no runtime wiring (ST-SUP-Q101). A service with data access and
// without the two components gets no runtime wiring either; the components that serve data refuse it, because the
// GORM store that served it before is removed (postgres-adapter, rest-api and grpc-application each name the
// command and query components and ST-SUP-RC20-OC39).
func runtimeDataPath(noData bool, names []string) bool {
	return !noData && slices.Contains(names, commandComponent) && slices.Contains(names, queryComponent)
}
