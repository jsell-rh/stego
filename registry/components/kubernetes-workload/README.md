# Kubernetes workload construction

This component generates a pure Go library. It builds a Deployment and an
optional ClusterIP Service from typed inputs. It does not make API requests.
It has no dependency on an application protocol, identity provider, or client.

Applications declare image digests, command arguments, environment variables,
Secret references, file dependencies, resource requests and limits, named ports,
and HTTP health probes. They also select labels, a named ServiceAccount,
replica count, and deployment strategy. The application must authorize these
inputs before it calls `Build`. Namespace allocation, ownership checks,
NetworkPolicy, verified dependencies, and reconciliation remain required.

`Build` fixes non-root execution, RuntimeDefault seccomp, a read-only root
filesystem, denied privilege escalation, and removal of all Linux capabilities.
It permits only Secret, ConfigMap, and bounded disk emptyDir volumes. Secret and
ConfigMap mounts are read-only. Secret file mode is 0440. The declared file
group must have access under the cluster's admission policy. A zero user or
primary group omits that field so admission can assign it. A zero file group
is rejected. No raw Pod fields or security override is accepted. These settings
follow the [restricted Pod security requirements](https://kubernetes.io/docs/concepts/security/pod-security-standards/).

ServiceAccount token mounting is disabled by default. `KubernetesAPI` enables
it for the entire Pod. Use this only when every container in that Pod is trusted
to use the selected account. The builder does not create accounts or grants.
Image pull Secrets must be selected explicitly. Cluster admission remains the
permission boundary; a construction library cannot prevent later caller edits.

Each container must declare startup, readiness, and liveness HTTP probes.
Probe ports must belong to that container. Probe paths cannot contain another
host, credentials, a query, or a fragment. Deadlines and failure thresholds
are explicit and bounded. No shell probe is supported. The probes have distinct
roles; see the [Kubernetes probe contract](https://kubernetes.io/docs/concepts/workloads/pods/probes/).

Resource amounts use integer millicores and MiB. Requests must be positive and
must not exceed limits. Disk emptyDir capacity cannot exceed the total Pod
storage limit. These checks validate a declaration; they do not measure its
capacity or replace quota and admission. Replicas above 1,000 are supported.
A zero replica count permits a controlled stop. Recreate and RollingUpdate are
explicit choices. RollingUpdate uses one extra replica and zero unavailable
replicas; capacity for that extra replica is required. See the
[Deployment strategy contract](https://kubernetes.io/docs/concepts/workloads/controllers/deployment/).

`ConfigurationDigest` takes dependency kinds, names, and contents. `Build` requires
exactly the ConfigMaps and Secrets used by mounts and Secret environment
references. An environment key must exist in its declared Secret contents.
Image pull Secrets are selected separately because their contents do not enter
the containers. The digest sorts dependency and
key names and hashes a versioned, length-delimited encoding. Map order has no
effect. Changed contents or names change the digest. API metadata is excluded
because the caller supplies only contents. Nil and empty bytes are equivalent.
The builder puts the digest in the Pod annotation `stego.dev/config-sha256`.
Secret contents do not enter the returned resources or error text. A digest is
not encryption: low-entropy values can be guessed from a public digest. Supply
only authorized configuration, and keep credentials in Secret references.

The library bounds inputs before it builds objects. It rejects duplicate names,
ambiguous environment sources, overlapping mounts, missing probe or Service
ports, and unsafe paths. Errors have fixed text. Failure returns no resources.
Results do not share mutable maps or slices with the declaration. Empty optional
lists, empty literal environment values, and false writable-mount flags are
omitted. This matches the Kubernetes API response format and prevents repeated
patches caused by fields that disappear during serialization.

The first profile supports Linux containers, digest-pinned registry images,
TCP ports above 1023, HTTP health probes, and ephemeral files. Persistent claims,
init containers, Jobs, StatefulSets, device access, host resources, and arbitrary
Pod extensions are not part of this profile. Add a reviewed profile when a real
application requires another mechanism. Do not bypass validation with raw maps.

The implementation is a candidate. Generated tests, compiler checks, Hypershell
adoption, and a complete live workflow must pass before a production claim.
