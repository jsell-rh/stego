# Private browser application

A browser service can declare one local application under the
`kubernetes-service.local_applications` list. STEGO generates the browser
session runtime, captured UI, private HTTP and WebSocket client, dependency
monitor, and two-container Pod from this declaration.

```yaml
overrides:
  health-check:
    database: true
  browser-backend:
    api_prefix: /api/v1
    routes: [/]
    asset_bundle: ui/build.zip
  kubernetes-service:
    local_applications:
      - port: 8000
        image: registry.example.test/team/application@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
        listen_env: LISTEN_ADDRESS
        port_env: PORT
        env_secret: application-env
        files_secret: application-files
        health_path: /api/v1/readyz
```

The image above is a syntax example. Select a verified application image.
The image must honor the declared listener variables. The generated environment
sets its address to `127.0.0.1` and fixes its port. A manifest check cannot prove
that an external binary honors this contract. Test the selected image before
production use.

Only the browser container has a Service port. Generated ingress rules permit
only its HTTPS port, 8443. A peer cannot select the local application port.
Neither container has a service account token. Both have CPU, memory, and
storage limits, a read-only root filesystem, no added capabilities, and the
runtime's default seccomp profile. The Pod does not share process namespaces.

The browser and application have separate temporary volumes and secret mounts.
The application reads its files at `/var/run/stego-application`. The browser
keeps its existing `/var/run/stego` mount. The compiler rejects shared secret
names, including aliases between environment and file secrets. Application
configuration must not contain browser OAuth or session keys. The compiler
cannot inspect the contents of operator-created Secrets.

The browser validates captured assets and serves only the declared files.
Session checks apply to documents, assets, HEAD, and conditional requests.
The private application receives only the server-held bearer token and the
selected request headers. Browser identity headers and cookies are removed.
The existing bounded WebSocket runtime applies to upgrades.

Readiness samples the session database and the declared application health
path. The checks share the existing 500-millisecond deadline. Only HTTP 200
marks the application ready. Redirects, timeouts, and other statuses fail the
check. The health request carries no bearer token or browser cookie. Public
probe requests read the cached result and cannot create upstream calls.
Process liveness remains separate from dependency readiness.

The first version permits one application and requires captured assets. It
does not combine a bearer-token API, Kubernetes API permissions, allocation
roles, controller workers, or separate RPC processes in this browser service.
The same declaration is checked by the browser, health, and deployment
components before generation. Hypershell keeps its Gateway image selection,
configuration values, access rules, and resource lifecycle policy.

## Verification

Small local checks cover unsafe declarations, secret aliases, fixed ports,
restricted container resources, absence of an application Service port, and
stable command generation. The command check first allows STEGO to create
`go.mod`, then checks stable state on a repeated apply. Its first test draft
compared state before the generated module became a captured input; that draft
failed. The corrected test passed and preserves the failed output separately.

CI must build and test the declared application runtime with PostgreSQL. It
checks readiness loss and recovery through the generated local client, session
and WebSocket behavior, and shutdown. The real upstream dashboard image,
rendered editor and terminal, cluster network boundary, and full Gateway
lifecycle still require application evidence. Do not infer those results from
this declaration or its manifest tests.

The production revision `02e7494f7fbfef542f83a71166e86ff36c9979f1` passed
all six jobs in [CI 35111380095](https://github.com/jsell-rh/stego/actions/runs/35111380095).
The generated browser package, including the declared application and readiness
checks, passed with required PostgreSQL and race detection in 201.525 seconds.
The complete log has SHA-256
`db9ba8dc9561504dd44d1803a30aedc1773039553ee41e67a49a41876ba2ee0b`.

The real upstream dashboard image also passed its separate
[build and private generation check](https://github.com/jsell-rh/hypershell-stego/actions/runs/35111676342).
The archived image binary matches the checked source binary. Its generated
private browser compiled and repeated generation retained identical state.
That run covered the root document path only. Neither result proves a deployed
cluster boundary, rendered editor and terminal, or full Gateway lifecycle.
Those application checks remain required.
