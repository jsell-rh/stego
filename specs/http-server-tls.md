# HTTP server TLS

The compiler can start the generated HTTP server with TLS 1.3. Set
`STEGO_HTTP_TLS_CERT` and `STEGO_HTTP_TLS_KEY` to absolute file paths. Set
`STEGO_HTTP_REQUIRE_TLS=1` to reject missing TLS settings. The switch accepts
only `0` and `1`. One missing file, an invalid key pair, or an unsafe key file
stops startup at `http.configure`. Error output does not contain the paths or
certificate data.

Each file must be a regular file of at most 65,536 bytes. The private key must
not have execute, group-write, or other permissions. Modes `0400`, `0600`,
`0440`, and `0640` are permitted. Projected Secret links are permitted. The
runtime opens the file without blocking on a FIFO, then checks the descriptor.
Use local mounted files. File-system I/O does not have a wall-clock deadline.

TLS file input requires Unix. Generated HTTP programs can still compile for
Windows and use their existing plaintext mode. With no TLS settings and no
requirement switch, the existing HTTP behavior remains available. The generated
Kubernetes service always requires TLS.

The server reads its key pair once at startup. Replace the Pod after a key
change. HTTP request limits and shutdown limits also apply to TLS connections.
The client must verify the server certificate. Kubernetes HTTPS probes do not
verify certificates; they check process health only. See the
[Kubernetes probe contract](https://kubernetes.io/docs/concepts/workloads/pods/probes/).

Generated runtime tests cover verified connections, unknown roots, wrong host
names, TLS 1.2 rejection, file modes, missing files, and projected links. The
Hypershell deployment test must also exercise the assembled server through the
generated HTTPS SDK.
