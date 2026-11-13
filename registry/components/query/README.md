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
the sealed cursor of the runtime.

A query with a gRPC method is also served over gRPC by the grpc-application
component (ST-SUP-RC07-OC19), with the same fill, decision and refusals:
query.ServeRPC reads the request from the method's message and writes the
JSON body of the HTTP handler into its response message. The compiler marks a
query whose method is a server stream as a stream query from the proto files
(ST-SUP-RC33-OC08). A stream query is served over gRPC only, through
query.ServeStream on its run function on query.RunStream, and gets no HTTP
route.

A secret field is never read: the read-only value and the reads leave it out.

An entity may declare `expected_cardinality: small`. Its lists then read only
the rows that the authorization provider's resource search gives (ST-SUP-RP11-LC20). An
ordered list takes at most 10000 keys. Above that, the list fails, and the
runtime counts it in the metric `query.list.over_bound`, with the entity as
an attribute. Alert on this metric. When it fires, the entity is not small:
declare it `expected_cardinality: large`, or remove the setting. There is no
quota at create (ST-SUP-Q116, decided 2026-11-12).

The component needs pooled tenancy with the key tenant_id and the
domain-probe component. The stage is ga.
