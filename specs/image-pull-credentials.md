# Image pull credentials

Generated deployment options accept `ImagePullSecrets`. The render command
accepts repeated `--image-pull-secret NAME` arguments. Each reference names a
Secret in the target namespace. Names must be distinct DNS subdomains, with
at most eight references. Reference order does not change the output.

The renderer adds references only to the selected workload's Pod template.
HTTP services, workers, and separate RPC processes use the same checks. Local
applications share the Pod's pull references. Other fields, permissions, and
container mounts remain unchanged. Credentials are not generation inputs.

The operator must supply the referenced Secrets before the Pod starts.
Kubernetes requires an appropriate registry Secret in the Pod's namespace.
See the [Kubernetes private registry instructions](https://kubernetes.io/docs/tasks/configure-pod-container/pull-image-private-registry/).

## Managed namespace credentials

The generated Kubernetes client provides `EnsureImagePullSecret` and
`EnsureImagePullSecretFile`. The caller supplies the assigned namespace name,
its observed UID and owner labels, the Secret name and owner labels, and the
exact registry addresses. The runtime checks the namespace before and after
the operation. It creates a missing Secret or updates an owned Secret with
the observed UID and resource version. It does not retry conflicts. A final
read must match the expected data, UID, and resource version.

The input is Docker config JSON with only `auths` and an `auth` field for each
selected registry. Each `auth` value contains canonical base64 of a nonempty
`user:password` pair. The pair must contain printable ASCII without spaces.
The input is limited to 16 KiB and one through eight exact registry addresses.
Addresses are DNS names with an optional port. Unknown fields, repeated JSON
member names, paths, schemes, wildcards, and credential helpers cause an error.
`ValidateImagePullConfig` applies the same checks without API calls.

The file operation reads the private file on each call. Thus, a subsequent
reconciliation can install an operator's rotated credential. Projected files
are supported. Execute bits, group write access, and access for other users
are forbidden. The runtime rejects foreign, deleting, immutable, and malformed
Secrets. It also rejects extra data keys. Errors do not contain credential data.

These operations do not grant registry access, change ServiceAccounts, add
application mounts, restart Pods, or prove that a credential is valid. The
operator must limit the credential's registry permissions and maintain its
expiry. The Pod renderer selects the named Secret separately. Do not use a
controller or CI API credential as an application image pull credential.

Namespace reads and Secret writes are separate API operations. The reads can
detect namespace replacement, but cannot make the write atomic with an
allocation check. Admission must enforce the allocation during writes. This
runtime operation does not replace that control.

Focused generated-runtime tests cover creation, repeated reconciliation,
rotation from a private file, conflicts, identity changes, malformed inputs,
and response changes. The managed Hypershell test still needs a restricted
registry identity and deployment integration. A rendered reference and a mock
API test do not prove an authenticated image pull.
