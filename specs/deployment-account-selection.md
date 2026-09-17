# Externally managed workload accounts

This change is under qualification. It has not been released or adopted by
Hypershell.

An allocator can own a workload's ServiceAccount. The deployment renderer must
then reference that account without creating or changing it. Applications must
not rewrite generated Pod identities to achieve this separation.

`deployment.Options.ExistingServiceAccount` selects an existing account in the
target namespace. The command option is `--existing-service-account`. The name
must be a DNS label with at most 63 bytes. An empty value preserves the existing
generated account behavior.

The option requires namespace scope. The complete generated resource list must
contain one ServiceAccount and one Deployment, with matching account references.
Only Service and NetworkPolicy resources can accompany them. The renderer
rejects generated RBAC, admission policies, and other resource types before it
filters by scope. It does not transfer an existing permission grant to a new
identity.

The renderer omits the account and changes only the Pod's serviceAccountName.
Token mounting, security settings, image pull references, probes, owner labels,
and all other workload fields retain their generated values. Rendering performs
no API request. The caller must verify the selected account's ownership and
readiness before it creates the workload. With a STEGO allocator, use its
ServiceAccountName method and allocation readiness checks. Do not copy the
account-name algorithm into an application.

The generated runtime tests check unchanged workload fields, command and typed
API agreement, repeat generation, invalid identities, and rejection of both
namespace and cluster permission grants. This contract does not add related
namespace bindings or remove the shared-cluster Sandbox guard.
