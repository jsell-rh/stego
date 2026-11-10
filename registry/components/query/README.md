This component generates the declared queries of a service. Each query gets
its slot contract in the package queries and one handler in the package
queries/serve.

The slot contract holds the read-only value of each entity that a query reads,
and for each query its request, response, handle and fill types, its
repositories and its refusals. The query slot (query.query) is this Go
contract, not a protobuf message. The fill of a query is the function Fill of
its package under fills. It gets the decoded request and one read-only query
handle.

Each get and list query gets one handler on query.Serve of the runtime library
(github.com/jsell-rh/stego/registry/runtime/query), which serve.Handlers.Routes mounts. On the runtime
data path the compiler makes the handlers with the query runtime, the verifier
and the probes, and main.go registers the route of each query. The handler
verifies the
caller, opens one READ ONLY transaction of the caller's tenant and calls the
fill once. A get query decides its declared permission on its resource before
the fill: a deny is not found, as a missing row is. A list query pages with
the sealed cursor of the runtime. A stream query gets a run function on
query.RunStream for its transport; a query with a gRPC method is refused until
the compiler tells a server stream from a unary method.

A secret field is never read: the read-only value and the reads leave it out.

The component needs pooled tenancy with the key tenant_id and the
domain-probe component. The stage is alpha.
