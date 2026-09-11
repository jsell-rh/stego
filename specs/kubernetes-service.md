# Kubernetes service deployment

The `kubernetes-service` component supplies common service deployment files.
It requires `health-check` and either `rest-api` or `http-application`. Bind
`health-endpoint` to `health-check` in the archetype. Add
`grpc-application` to expose its TLS listener. This version uses Go 1.26.8 and
a pinned builder image. A different Go target fails validation.

The component writes these files under its output namespace:

- `Containerfile`: a Go build stage and a scratch runtime image. The runtime
  contains the service binary and public system CA roots.
- `Containerfile.dockerignore`: an explicit build context. The component
  includes module files, generated output, and `source_directories`. Hidden
  files and private-key file extensions are excluded.
- `render`: a small Go command and an embedded resource template.

Run the container build from the project root. `source_directories` must contain
distinct top-level directory names. List all domain source directories needed
by the service. Do not put credentials in source files. The allowlist limits
the build context; it is not a secret scanner. Docker uses the
[Dockerfile-specific ignore file](https://docs.docker.com/build/concepts/context/).

Render resources after the image build:

```sh
go run ./out/deploy/render --image registry.example/team/service@sha256:FULL_DIGEST --namespace services
```

The image must have a SHA-256 digest. Tags are rejected. The namespace must
already exist. `--fs-group` selects the group for mounted files; its default is
65532. On OpenShift, select a group from the target namespace's allocated range.
The image uses UID and GID 65532. The Pod permits the platform to assign another
non-root UID. Rendered resources contain no credential values.

The command emits a ServiceAccount, Deployment, ClusterIP Service, and
NetworkPolicy. It does not apply them. The ServiceAccount has no generated RBAC
grant and its token is not mounted. The Pod requires non-root execution, drops
all capabilities, prohibits privilege escalation, uses the runtime seccomp
profile, and has a read-only root filesystem. See the
[restricted Pod security standard](https://kubernetes.io/docs/concepts/security/pod-security-standards/).

The container requests 100 millicores and 128 MiB. Its limits are one CPU,
512 MiB of memory, and 128 MiB of ephemeral storage. The Go memory target is
384 MiB. Its `/tmp` volume permits
64 MiB. These are initial bounds, not a capacity claim. The Deployment has one
replica, one extra Pod during rollout, a 60-second termination grace period,
and a 180-second progress deadline. Slow domain shutdown can still reach the
platform's forced termination limit.

Provide two Secrets. `env_secret` defaults to `SERVICE-runtime` and supplies
environment settings. `files_secret` defaults to `SERVICE-files` and supplies
files at `/var/run/stego`, with mode `0440`. Include `tls.crt` and `tls.key`.
Set `DATABASE_URL_FILE` and other runtime file settings to their mounted paths.
The generated explicit settings require HTTP TLS, bind HTTPS on 8443, prohibit
the database plaintext exception, and bind gRPC on 9090 when enabled. These
settings take precedence over the environment Secret. Configure image-pull
credentials on the ServiceAccount when the registry requires them.

Startup and liveness probes use `/livez`. Readiness uses `/readyz`. All probes
use HTTPS. The existing health component controls the dependency check; select
`health-check.database: true` for database readiness. TLS client verification
requires a separate API test because Kubernetes probes skip it.

The network policy denies traffic unless a rule permits it. `network_peers`
permits at most 32 entries. Each entry requires `direction`, `namespace`,
`pod_label`, `pod_value`, `port`, and `protocol`. The namespace `self` means the
rendered namespace. Each rule selects both the namespace and a Pod label.
This version accepts unqualified label keys. Ingress must use an enabled API
port. Egress can use explicit TCP or UDP ports. DNS is permitted to
`dns_namespace` on `dns_port`, which default to `kube-system` and 53. Select
the cluster's actual DNS namespace and port. A NetworkPolicy-capable network
plugin is required.

External CIDR peers, public ingress, autoscaling, disruption budgets, image
signing, deployment migrations, and certificate renewal remain separate work.
The component does not install databases, brokers, or domain controllers.
The Hypershell test bed supplies its domain sources and test peer settings.
