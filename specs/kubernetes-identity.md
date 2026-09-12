# Kubernetes API identity

`kubernetes-service` version 1.5.0 accepts `kubernetes_api: true` on the
primary service, a worker, or an RPC process. The option applies only to that
target. It requires a declared `kubernetes` external endpoint. The deployment
renderer requires explicit endpoint IP and port bindings through `--egress`.

The target receives a read-only projection at `/var/run/stego-kubernetes`.
`token` contains a Pod-bound ServiceAccount token with a requested lifetime of
3600 seconds. Its omitted audience selects the API server audience.
`ca.crt` comes from the namespace's `kube-root-ca.crt` ConfigMap. File mode is
0440, with the generated Pod file group. Automatic token mounting remains off.
The mount does not use `subPath`, so Kubernetes can replace the projected files.
See the [Kubernetes projection contract](https://kubernetes.io/docs/concepts/storage/projected-volumes/#serviceaccounttoken-projected-volumes).

Kubernetes owns token rotation. The generated Kubernetes client reads its
configured token file for each request. Set its server to
`https://kubernetes.default.svc`, its CA file to the projected `ca.crt`, and its
token file to the projected `token`. Do not copy that token into a Secret or
log it. Existing client tests cover token reload. A deployment test that ends
before token rotation does not prove a complete rotation cycle.

`kubernetes_permissions` declares RBAC rules for this target. Each rule has
`scope` (`namespace` or `cluster`), `api_group` (empty for core resources),
`resources`, and `verbs`. Optional `resource_names` restricts named objects.
The compiler rejects unknown fields, wildcards, duplicate list entries,
`bind`, `escalate`, `impersonate`, and `deletecollection`. It rejects named
`create` rules because Kubernetes cannot enforce that combination. A rule set
has at most 32 rules, with at most 32 entries in each list.

Namespace rules produce a Role and RoleBinding in the target namespace.
Cluster rules produce a ClusterRole and ClusterRoleBinding. Cluster object
names join the namespace and service name with a dot. Neither input permits
a dot, so distinct pairs cannot produce the same cluster object name.
Bindings select only this target's ServiceAccount. No permissions are inferred
from source code or network access. Without the option, no token is projected.

For example, a worker can read one ConfigMap in its own namespace:

```yaml
kubernetes_api: true
external_endpoints: [kubernetes]
kubernetes_permissions:
  - scope: namespace
    api_group: ""
    resources: [configmaps]
    verbs: [get]
    resource_names: [settings]
```

Cluster scope requires an explicit application declaration. Kubernetes RBAC
cannot restrict a ClusterRole rule by an object's owner label. Controllers
that manage dynamic namespaces therefore need a separate production isolation
policy. These declarations do not replace that policy or the API server's
RBAC escalation checks.

The bounded jshell check in `/tmp/stego-kube-identity-nzlp9_nh` passed the
race-enabled deployment generator suite in 3.836 seconds and registry suite
in 2.234 seconds. It covers all three deployment targets, token permissions,
scoped binding names, invalid permissions, and generated renderer execution.
The source is `source.tar`; results are `test.log`. Runtime worker adoption is
a separate application check.

Hypershell now uses the generated identity and permission rules for its
database and Gateway workers. The full browser workflow passed with all
three workers in separate Deployments, including worker replacement and
correlated telemetry from both instances. See the
[application evidence](https://github.com/jsell-rh/hypershell-stego/blob/f755dae/acceptance/worker-deployment.md).
Full compiler CI also passed for the RBAC name-collision correction in
[run 34660866379](https://github.com/jsell-rh/stego/actions/runs/34660866379).

## Live token rotation check

Run `scripts/check-kubernetes-token-rotation.py` with Python 3, an explicit
`--context`, and `--source` set to the STEGO checkout. Stage new test files
first; the source archive contains files from `git ls-files`. The script creates
a dedicated namespace and one Job. It adds no RBAC permissions. It leaves the
Job in place if an observation fails, so inspect that Job before another run.

The Job has a twenty-minute deadline, one CPU, 3 GiB of memory, and 6 GiB of
temporary storage. It uses a read-only token projection with a requested
lifetime of 600 seconds and file mode 0440. The normal generated deployment
still requests 3600 seconds. The test is not a capacity or performance test.

`TestLiveProjectedTokenRotation` uses one generated client for the full check.
It requests a
[SelfSubjectReview](https://kubernetes.io/docs/reference/kubernetes-api/definitions/self-subject-review-v1-authentication/)
every five seconds for at most twelve minutes. Each response must confirm the
ServiceAccount and Pod identity. The API server's credential identifier must
match the projected JWT identifier. Kubernetes includes this identifier in the
[authenticated ServiceAccount attributes](https://kubernetes.io/docs/reference/access-authn-authz/service-accounts-admin/).
The local claims do not grant authority; the server must authenticate them.

A successful result requires the server to confirm two different credentials
through the same client. A changed file alone is not a pass. Secret access must
remain denied after rotation. This check proves use of the new token; it does
not claim that the old token has expired or been revoked.

The normal generated-client suite runs with and without the OTEL transport.
The live check enables OTEL and exports logs, metrics, and traces through a
verified TLS collector. The collector keeps counters and rejects selected
private values. It does not retain export batches while rotation is pending.
The result files contain counts, durations, and test outcomes. They contain no
token or credential identifier.

The jshell check passed on 2026-09-12 UTC. The live OTEL case took 410.03
seconds. It made 83 identity requests, confirmed two credentials, used no
client restart, and received `403` for Secret access after rotation. The
race-enabled runtime suite took 414.174 seconds. The full generator check,
including the case without OTEL, took 488.421 seconds.

Results are in `/tmp/stego-token-rotation-4af00kxw`. The Job reached `Complete`.
All 402 archived compiler and check source files match the checked source.
The three generated Kubernetes files and the generated HTTP client match
Hypershell at `31242f5`, after module-path substitution and Go formatting.
The test namespace is absent. An earlier check without OTEL also passed in
465.01 seconds; its results are `/tmp/stego-token-rotation-z1zcaa0n`, and its
namespace is absent. These results prove the shared generated client behavior.
They do not prove token rotation in a deployed Hypershell worker.
