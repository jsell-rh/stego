# Configuration rollout input

The generated renderer accepts `Options.ConfigurationDigest` and the matching
`--configuration-digest` command option. The value must be a lowercase SHA-256
digest with 64 hexadecimal characters. The renderer sets the Pod template
annotation `stego.dev/config-sha256` on its selected Deployment. Changed contents
can then cause a rollout without application code that edits Kubernetes maps.

The caller must verify the dependency names, namespace, ownership, credentials,
and certificates before it supplies a digest. Use a common content-digest helper
for the selected dependencies. This renderer does not read Secrets or establish
that the supplied digest matches their contents. Secret contents do not belong
in this option. The option contains only the resulting digest.

An omitted value preserves the default generated manifest. It does not remove
an annotation that is already stored in Kubernetes. Callers that select this
mechanism must supply the current digest on each reconciliation. The option
requires `all` or `namespace` scope because `cluster` scope has no Deployment.
When splitting installation resources, supply the digest only for the namespace
manifest. Other selection, permission, and endpoint checks still apply.

Construction preserves all other annotations, resource fields, and resource
order. It rejects malformed or conflicting generated annotations and requires
exactly one generated Deployment. Invalid values return no resources or bytes;
errors do not include the supplied value. Each render owns its returned maps.

The generated tests use an independent widget service with API, worker, and RPC
Deployments. They compare the command and typed interfaces, changed digests,
repeated output, existing account selection, and rejected values and templates.
The compiler checks pass. Each consumer must still verify the generated output
and its complete workflow before adoption.
