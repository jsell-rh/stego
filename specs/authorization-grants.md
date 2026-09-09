The `jwt-auth` component version 2.2.0 generates an immutable grant policy.
It supplements token verification. It does not replace authentication or infer
resource ownership.

Each grant names the verified issuer and subject, a resource type, an operation,
and an exact target. Resource and operation names are application-defined ASCII
names. The target is an opaque scope value. Empty targets match only empty
targets. There are no wildcard, prefix, role, or administrator fallbacks.

```go
policy, err := auth.NewGrantPolicy([]auth.Grant{{
    Issuer: "https://issuer.example", Subject: "worker-a",
    Resource: "Lease", Operation: "cleanup.worker", Target: "region-a",
}})
```

Call `policy.Allows(identity, resource, operation, target)` with an authenticated
identity. A nil policy or zero value denies all operations. The policy copies
its input and permits concurrent reads without mutation. To revoke a grant,
construct and install a new policy. The library does not reload files or modify
policies during requests.

`ParseGrantPolicy` accepts a JSON array with all five lowercase string fields:
`issuer`, `subject`, `resource`, `operation`, and `target`. It rejects unknown,
repeated, missing, incorrectly cased, null, or coerced members. It also rejects
invalid Unicode, trailing documents, repeated grants, and excessive input.
It accepts at most 1024 grants and 262144 JSON bytes. Issuer, subject, and target
values have limits of 1024, 512, and 256 UTF-8 bytes. Resource and operation names
have a 128-byte limit. Values cannot have outer whitespace or NUL bytes.

The application must choose the resource, operation, and target from trusted
routing and current state. It must enforce the policy at every protected entry
point, before the mutation. A target grant does not establish that the resource
has that target, that its revision is current, or that cleanup is complete.
Keep storage preconditions and events in the same transaction.

Generated tests cover every matching dimension, role fallback refusal, empty
and literal wildcard targets, constructor input mutation, concurrent reads,
strict JSON, and bounds. Hypershell supplies the domain operation names and
applies this policy to Gateway and database cleanup observations.

This contract does not yet provide policy storage, runtime revocation across
processes, dynamic ownership rules, or a complete application permission model.

A local Linux amd64 measurement on 2026-09-09 used an Intel Core Ultra 9 185H.
Exact grant checks averaged 83.64 ns with one grant and 87.55 ns with 1024 grants,
with no Go allocations in either case. These samples used short identity and
scope values. They exclude token verification, RPC, and storage, and do not
establish a production request latency. Repeat with:

```sh
STEGO_BENCH_AUTH=1 go test -count=1 -v ./internal/generator/jwtauth -run '^TestGeneratedAuthenticationRuntime$'
```
