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

The managed-namespace integration still needs a common runtime operation that
validates and installs narrowly scoped registry credentials. That operation
must preserve namespace ownership, permit credential rotation, keep credentials
out of logs and application mounts, and reject foreign Secrets. A rendered
reference alone does not prove an authenticated image pull.
