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
Label keys can have a valid DNS prefix. Ingress must use an enabled API
port. Egress can use explicit TCP or UDP ports. DNS is permitted to
`dns_namespace` on `dns_port`, which default to `kube-system` and 53. Select
the cluster's actual DNS namespace and port. A NetworkPolicy-capable network
plugin is required.

External CIDR peers, public ingress, autoscaling, disruption budgets, image
signing, deployment migrations, and certificate renewal remain separate work.
The component does not install databases, brokers, or domain controllers.
The Hypershell test bed supplies its domain sources and test peer settings.

Hypershell commit `609d8b5` also adopts the existing worker contract for its
database, Gateway workload, and sandbox-count processes. Provider setup stays
in the application; STEGO supplies their main functions, signals, probes,
monitor, safe failure output, images, and Deployment templates. This adoption
needs no compiler change. Its focused startup and input-manifest checks passed
under race detection on jshell. Two generation passes and the post-test check
preserved all 129 output, state, and dependency hashes. The Job completed and
its namespace was deleted. The real provider workflows require their own CI
results. Kubernetes API egress, provider credentials, and RBAC still require
site configuration. See the [application record](https://github.com/jsell-rh/hypershell-stego/blob/609d8b5/acceptance/generated-workload-workers.md).

The first deployed Gateway gate passed on jshell on 2026-09-11 with compiler
`ae4f1a28226a726bb637faa5e3325c94ed443818`. The test used Go 1.26.8, PostgreSQL
18.6 with SCRAM and verified TLS, and the TLS Kafka protocol fixture. The
application test took 10.71 seconds; its race-enabled test package took 11.757
seconds. The service image used a static binary and public CA roots. The API
Pod used the generated resource limits and a namespace-assigned file group.

The gate checked REST and gRPC reads and filtered lists, the atomic owner grant,
rollback after a rejected event write, event delivery, and Pod replacement.
Both API instances exported correlated request logs and traces, plus request
metrics. The test checked that private input and credential data was absent.
Repeated generation and the post-test check preserved 112 output, state, and
dependency hashes. The test Job completed, and namespace deletion was verified.

The run exposed the [Kafka file-group gap](kafka-secret-files.md). Two earlier
test images also failed before the application gate: the first upload lacked
public CA roots in the custom registry trust bundle, and the first image
metadata omitted the entry point. The test publisher now checks the image
configuration before deployment. These failed runs remain in the local test
records. Full compiler CI passed for the deployment and Kafka revisions. The
[Containerfile CI job](https://github.com/jsell-rh/hypershell-stego/actions/runs/34619444307/job/103329456378)
also passed. It built the generated Containerfile and checked the image user
and entry point.

The repository test command then passed from a fresh namespace. The Gateway
test took 11.57 seconds; its race-enabled package took 12.621 seconds. The fresh
compiler build reproduced all 112 file hashes and the same service image digest.
The Job reached `Complete`, and namespace deletion was verified.

The test bed then added real Gateway identity reconciliation to the same
cluster gate. Application commit `6fe35d41fc58dfd38dfcc0204785ce270cdee06b`
passed on jshell on 2026-09-11 with the same compiler and service image. The
application test took 100.05 seconds; its race-enabled package took 101.094
seconds. All 112 generated, state, and dependency hashes matched across two
generation passes, the post-test check, and the local application checkout.

The generated API and real Keycloak provider ran in separate Pods. The identity
controller ran as a separate process inside the bounded test Pod. It used the
generated gRPC client, reconciliation runtime, recovery scan, and condition
writes. The gate checked identity creation, recovery after a controller restart,
and recovery after API Pod replacement. It retained the owner-grant, access,
rollback, event, and telemetry checks. A Kafka offset boundary prevents an old
identity event from satisfying the final image-update event check.

The extension exposed test-fixture faults: an invalid Deployment deadline,
insufficient collector buffer space at the test export rate, and a race between
Pod and Service readiness. It also showed that Keycloak development mode opened
an HTTP listener despite the supplied setting. The fixture now uses a bounded
Pod, standard server mode, a verified Service readiness loop, and a ten-second
test metrics interval. It checks both local ports. The successful run required
two HTTPS requests before the Service was ready. HTTPS was open; HTTP was closed.
The Job completed, and all four test namespaces were deleted.

No compiler or domain runtime change was needed for that extension. Generated
controller images, health probes, and deployment resources were added after that
run. See the
[application check](https://github.com/jsell-rh/hypershell-stego/blob/6fe35d41fc58dfd38dfcc0204785ce270cdee06b/acceptance/kubernetes_identity_test.go)
and the [test record](https://github.com/jsell-rh/hypershell-stego/blob/main/acceptance/kubernetes-service.md).
The earlier full application CI run 34619444307 passed. The new revision's CI
is separate from the recorded cluster result.

Worker declarations add generated controller entry points and images. Each
`workers` item has `name`, `package`, and `function`. The optional settings are
`env_secret`, `files_secret`, and `network_peers`. A worker requires the
`controller` component. Its source package must be inside a declared human
source directory. The compiler reads and records `PACKAGE/worker.go`. That file
must declare the exported function with signature
`func(context.Context, *controller.Metrics) error`. The metrics import must name
the generated controller package. Build constraints on this declaration file
are rejected. The Go build still checks the callback body and its dependencies.

The generated main calls `controller.Main`. Provider construction and domain
rules stay in the callback. Worker files are under `deploy/workers/NAME`.
Build that directory's Containerfile from the project root. Render its resources
with `--worker NAME`, the worker image digest, namespace, and file group.
Unknown worker names fail before output is written.

Each worker gets its own ServiceAccount, network policy, and Deployment. It has
no service-account token or ingress rule. Only declared egress peers and DNS
are permitted. File and environment Secret names default to
`SERVICE-NAME-files` and `SERVICE-NAME-runtime`. The resource and filesystem
limits match the API. Probes run the worker binary against its loopback endpoint;
they do not start domain actions. The generated OTEL service name identifies the
worker. No probe port or Kubernetes Service is exposed.

Workers use one replica and `Recreate`. This avoids intentional rollout overlap.
It does not supply a cross-process lease or fence a process on an unreachable
node. Distributed exclusion remains open work. A worker callback must retain
safe repeated effects and authoritative state checks. The generated network
policy is emitted before each Deployment.

The separate generated worker passed the Hypershell cluster gate on 2026-09-11.
Compiler `cb0326dd659e3904ed17654e09778564b8660fa7` generated the worker main,
probes, Containerfile, and Deployment. Application commit `56759c4` records the
provider callback and test changes. The Gateway test took 108.26 seconds; its
race-enabled package took 109.305 seconds. It retained the REST, gRPC, owner-grant,
rollback, access-filter, and event checks. It also checked identity repair after
worker Pod replacement and API Pod replacement against real Keycloak.

Both API instances and both worker instances supplied correlated logs and
traces, plus metrics. Selected private values were absent. Both generation
passes, the post-test check, and the local application checkout matched all
117 generated, state, and dependency hashes. The Job completed. Its namespace
and private fixture files were removed. The
[application record](https://github.com/jsell-rh/hypershell-stego/blob/main/acceptance/kubernetes-service.md)
includes image digests and the named scale-subresource fixture amendment made
before the application tests started.

This worker integration required common credential readers to accept private
projected files with read-only group access. HTTP and gRPC readers still reject
execute bits, group-write access, and other access. It also required qualified
Kubernetes label keys in declared network peers. These fixes belong in STEGO.
Hypershell keeps provider setup and domain rules.

[Compiler CI](https://github.com/jsell-rh/stego/actions/runs/34624175055) passed
for the pinned revision, including race tests and the vulnerability check.
Full application CI is separate from the cluster result. The generated worker
still uses one replica. Distributed exclusion and production capacity remain
open work.
