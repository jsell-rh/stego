# Allocation ServiceAccount identity

This change is under qualification. Hypershell does not yet use it. The shared
cluster Sandbox guard remains in place. Cross-namespace binding configuration
and live admission tests are still required before that guard can be removed.

An allocation profile can declare `service_accounts` with one through eight
aliases. Each alias is a distinct DNS label with at most ten bytes. `default`
is reserved. When this list is present, each binding to an allocated account
must select one of these aliases. Control and external subjects keep their
explicit names.

```yaml
service_accounts: [gateway]
bindings:
  - role: gateway-worker
    service_account: gateway
    namespace: allocated
```

STEGO generates the actual name from the alias and a full SHA-256 digest.
The input is a JSON array with these strings, in order:

1. `stego-allocation-service-account-v1`
2. The control namespace
3. The allocator ServiceAccount name
4. The profile's owner label key
5. The allocation owner ID
6. The alias

The digest uses lowercase base32 without padding. The result is the alias,
one hyphen, and 52 digest characters. It fits the 63-byte ServiceAccount name
limit. No owner ID is placed directly in the name. The name remains stable
across allocator restarts and namespace replacement for the same owner.
Another owner receives a different name. This is a cryptographic identity
derivation; it is not a credential or an authorization decision.

`Allocator.ServiceAccountName` returns this name without an API request.
It validates the profile, namespace, owner, and alias. Callers must still wait
for allocation before they start a workload. The application selects an alias;
it does not copy the name algorithm or create the account.

The allocator records the names in reserved namespace annotations. It applies
the quota and network limits before it creates the accounts or their bindings.
Each account has `automountServiceAccountToken: false`. A Pod that needs a
Kubernetes token must explicitly enable its token mount. The generated
bindings use the owner-specific name, including the restricted cluster roles.

Admission rules protect the reserved namespace annotations from replacement.
They permit only the allocator to create a declared owner account. Account
updates must keep the generated name, owner labels, and false automount value.
Kubernetes can maintain image pull references and its unbound default account.
Namespace deletion can remove the accounts. Other actors cannot create an
account with the previous owner's name after namespace reuse.

The runtime rejects a changed stored name before it writes. Read-only namespace
verification also checks the names. A first upgrade can add a missing annotation
through the allocator; an existing value cannot be replaced. The policies must
be installed before the allocator receives its permissions.

This mechanism avoids a permanent Kubernetes ownership record for each deleted
namespace. Kubernetes role bindings identify a ServiceAccount by namespace and
name. A grant that remains in another namespace therefore cannot identify a
new owner's generated account. See the
[Kubernetes ServiceAccount contract](https://kubernetes.io/docs/concepts/security/service-accounts/).

The allocator remains a trusted control-plane identity. It must use current,
authorized application state. Namespace names and aliases are not authority.
The generated runtime computes the digest; the admission rules check the
declared names and their immutable namespace records. They do not recompute
SHA-256 in CEL. Cluster binding checks retain the declared metadata-only and
identity-review role limits. This does not add cross-process writer fencing.

## Required evidence

Generated runtime tests cover a fixed digest vector, distinct owner and
installation identities, limits before permissions, changed annotations,
namespace reuse, competing owners, and quota failure. The namespace reuse
fixture checks generated object identities. It does not run Kubernetes RBAC.

Qualification must also use a real API server to check the generated CEL,
denied name and annotation changes, namespace deletion and reuse, and access
from the replacement account while an old cross-namespace grant remains.
That check requires no privileged Pod or Kata runtime. The deferred VM test
does not replace this admission and authorization check.
