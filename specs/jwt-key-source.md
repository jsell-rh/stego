The shared JWT generator supplies `JWKSVerifier` for consumers that use rotating
keys. `VerifyWithJWKS` remains available for a caller that already owns trusted
key retrieval. Both use the same key decoder and token verifier. The existing
static public-key verifier retains its startup-only key loading behavior.

`JWKSConfig` fixes the issuer, audience, and key source. File input takes precedence
over the HTTPS URL. Remote retrieval uses verified TLS 1.2 or later, system roots,
and optional private CA roots. It has no proxy, redirect, compression, or cookie
support. It does not use a URL from the token or key document. Startup fails if
trust settings or initial keys are invalid.

Key documents and CA files have a 64 KiB limit. A key document has at most 32
unique key IDs. Tokens have a 16 KiB limit. Remote headers have an 8 KiB limit;
retrieval has a five-second deadline and one connection per host. Errors do not
contain key data, tokens, server response bodies, or source addresses.

Successful loading creates an immutable map of verified RSA keys and parsers.
Requests use that map. A request can refresh it after five minutes, or when its
key ID is unknown. At most one retrieval can run. Attempts are at least thirty
seconds apart, including failed attempts. Other requests do not wait for an
active retrieval. Unknown keys remain denied until a successful refresh contains
them. Removed keys become invalid as soon as a replacement document is installed.

If refresh fails, a known key can remain valid for at most fifteen minutes from
the last successful load. Token expiry and all other trust checks still apply.
After that cache deadline, requests fail until retrieval succeeds. This bounded
outage policy means provider-side key removal is not immediate during an outage.
Use a static key only when operator-controlled process replacement meets the
installation's rotation requirements.

The verifier creates no background refresh task. Request cancellation and Stop
cancel remote retrieval. Stop clears cached keys and rejects later verification;
repeat calls are safe. A request already in verification can finish. There are
no global providers or shared HTTP clients.

The generated race tests use real TLS endpoints and signed tokens. They check
trusted and untrusted certificates, rotation, removal, cooldown, provider outage,
cache expiry, recovery, canceled requests, interrupted retrieval, repeated Stop,
empty runtime state, redirects, and local and remote input limits. These are
correctness checks, not capacity measurements. Measured cost remains part of
the wider performance requirements.

The telemetry candidate uses the selected common `otel-tracing` peer and the
runtime in the caller's context. With no peer or runtime, verification retains
its existing behavior. It creates no provider, exporter, or background task.
The process still owns telemetry startup and shutdown.

Each actual refresh emits one `auth.keys.refresh.completed` completion and an
`auth.keys.refresh` span. The fixed source classes are `file` and `https`.
The duration histogram is `stego.auth.keys.refresh.duration`; the active counter
is `stego.auth.keys.refresh.active`. The fixed outcomes are `success`, `failure`,
`canceled`, `deadline`, and `aborted`. Unknown input becomes a fixed fallback.
Completion is idempotent. Local output and OTLP export use the existing bounded
queues and shutdown deadline.

Cache hits, cooldown rejection, and stopped verifiers do not emit refresh work.
Completion runs after the key-cache lock is released. No record contains a
token, key ID, key document, file path, source URL, or provider error text.
This change covers key retrieval and compilation after source validation; it
does not add per-token logs or establish complete authentication observability.

Hosted qualification and consumer adoption are pending. The generated tests
cover both peer modes, all fixed outcomes, file and HTTPS refresh, bounded
outage use, recovery, cooldown, stop, cancellation, trace sampling, correlated
signals, private input removal, idempotent completion, and collector failure.
The change does not establish a capacity result or close C6.
