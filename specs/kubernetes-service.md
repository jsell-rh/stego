# Kubernetes service deployment

The `kubernetes-service` component supplies common service deployment files.
It requires `health-check` and one of `rest-api`, `http-application`, or
`browser-backend`. Bind
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

Declare `external_endpoints` for TCP services outside Pod-label selection. The
list contains at most 32 distinct DNS label names. It is available on the
service and on each worker. Supply each selected workload's values at render
time, for example `--egress kubernetes=192.0.2.10:6443`. Use brackets for IPv6:
`--egress kubernetes=[2001:db8::1]:443`. Repeat the flag for multiple addresses.
The complete render command permits at most 32 address and port pairs.

Each pair produces a separate egress rule with a single `/32` or `/128` IP
prefix and one TCP port. The renderer does not combine addresses and ports
into a wider set. Missing, unknown, duplicate, or invalid bindings fail before
output. Hostnames, CIDR ranges, zero ports, unspecified addresses, loopback,
link-local addresses, multicast, IPv4-mapped IPv6, and interface zones are
rejected. Binding order does not change the output. Values apply only to the
selected workload. Unrendered endpoint placeholders are invalid Kubernetes
CIDRs and cannot become empty rules that permit all traffic.

The operator must supply the IP addresses visible to the network plugin at
its policy boundary. TLS must still verify the configured server identity.
DNS resolution is not a policy update mechanism. Address changes require a new
render and rollout. Network translation and policy enforcement need a test on
the target cluster. An IP rule does not authenticate a remote service or grant
Kubernetes permissions.

The external endpoint extension passed a live jshell check on 2026-09-11.
Hypershell commit `939937b` used compiler `77e2133` to render its database-worker
policy. A restricted probe could read only its own Pod through verified HTTPS.
Default denial blocked the request. The correct generated IP and port rule
restored access. The wrong port and wrong IP each blocked three requests.
Restoring the correct rule restored access. The test namespace was deleted.
See the [application evidence](https://github.com/jsell-rh/hypershell-stego/blob/939937b/acceptance/external-egress.md).

Deployment and registry package checks passed under race detection in 3.803 and
2.123 seconds. Hypershell's input-manifest check passed in 1.055 seconds. All
129 generated, state, and dependency hashes matched both generation passes,
the post-test check, and the checkout. The build Job completed and was removed
with its namespace. This evidence covers direct IPv4 endpoint enforcement.
Service-address translation, IPv6 enforcement, and production operations remain
separate checks. Full CI results are tracked separately.

Public ingress, autoscaling, disruption budgets, image
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

Version 1.3.0 also accepts `browser-backend`. The common `browser-service`
archetype includes this deployment component from version 1.1.0. It uses the
same generated TLS listener, health routes, image, resource bounds, and Pod
restrictions as an API service. It adds no gRPC listener. The browser service's
name supplies separate ServiceAccount and Secret names.

The application must declare browser ingress and the API, identity provider,
session database, and telemetry collector peers. The API must also permit the
browser backend's HTTPS traffic. A URL setting does not add a network rule.
Browser OAuth and session key files use the existing file Secret mount. The
common telemetry settings remain in the server environment; only public signal
flags and a sample ratio are sent to the browser.

The bounded jshell checks passed deployment tests under race detection in
3.883 seconds and registry tests in 2.173 seconds. The browser test requires
byte-identical restricted deployment output to the equivalent API declaration
and rejects undeclared gRPC ingress. Application deployment and production key
rotation remain separate gates.

## Separate RPC processes

A service can deploy its declared RPC processes with `rpc_processes`:

```yaml
  kubernetes-service:
    source_directories: [internal]
    rpc_processes:
      - name: provisioner
        component: grpc-application
        process: provisioner
        network_peers:
          - {direction: ingress, namespace: self, pod_label: app.kubernetes.io/name, pod_value: example, port: 9090, protocol: TCP}
```

`component` selects `grpc-application` or `grpc-processes`. `process` must select
exactly one process in that resolved component. Its factory must be in an
allowed source directory. The deployment `name` must be a DNS label. The full
service name, `SERVICE-NAME`, must have at most 50 bytes. Names must be distinct
from other RPC deployments and controller workers. The compiler supplies the
resolved peer declarations for these checks before it writes output.

Build with `out/deploy/rpc/NAME/Containerfile` from the project root. This image
builds the declared generated entry point. Render it with `--rpc-process NAME`
and the normal image digest, namespace, and file group arguments. Do not combine
`--worker` and `--rpc-process`. Unknown targets fail without output.

Each RPC target has its own ServiceAccount, Secrets, Deployment, Service, and
NetworkPolicy. It uses the same resource and container restrictions as the API.
The Service exposes only TLS on port 9090. Ingress peers can select only that
port. Declare egress peers separately. `env_secret`, `files_secret`, and
`external_endpoints` work as they do for workers. Parent network peers and
Secrets are not inherited. DNS settings are inherited.

The generated explicit environment binds the RPC listener on 9090, selects
`tls.crt` and `tls.key`, and sets a loopback monitor on `127.0.0.1:9082`.
It sets the process OTEL service name and disables the database plaintext
exception. Startup and readiness run `/rpc --stego-probe=ready`. Liveness runs
`/rpc --stego-probe=live`. A monitor endpoint is never exposed by the Service.
These probes do not check external dependencies. A separate application test
must check verified TLS, authorization, and real provider operations.

This component still requires a primary HTTP service and its health component.
It can add RPC deployments to that project. RPC-only project deployment remains
separate work.

Services can also declare a separate namespace allocator worker. See
[Namespace allocation](namespace-allocation.md) for profiles, permissions,
admission checks, lifecycle behavior, and cluster test requirements.
