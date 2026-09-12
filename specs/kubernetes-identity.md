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
