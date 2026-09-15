# RPC probes with verified peer identity

`grpc-application` 1.12.0 supplies `client.NewTLSProbe`. Installation code gives
it an address, an explicit CA pool, and the SHA-256 digest of the expected leaf
certificate in DER form. The runtime copies the CA pool. TLS 1.3, hostname, and
chain checks remain required. The certificate digest is an additional identity
check. It cannot replace certificate validation or establish application access.

The probe uses the generated unary RPC limits, deadlines, disabled retries, and
telemetry. It removes caller metadata before it adds its own telemetry metadata.
It cannot read token files, accept per-call credentials, or open response streams.
The ordinary authenticated client still requires its CA and token files.

The application selects the probe methods and checks their responses. A health
response does not prove that an authenticated user can use the service. That
behavior requires a separate complete application test.

Focused tests use an independent generated Records service. They check valid
TLS, certificate mismatch, untrusted roots, hostname mismatch, metadata removal,
denied responses, message limits, cancellation, and invalid configuration. Both
telemetry modes include these tests in full CI. Hypershell will use this client
to check its public Gateway before publishing the endpoint.
