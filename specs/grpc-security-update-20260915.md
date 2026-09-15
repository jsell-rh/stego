# gRPC transport security update

The Go vulnerability database published GO-2026-6443 on 15 September 2026.
It describes a server panic when xDS routing receives a request with neither
`:authority` nor `Host`. Hypershell CI run 35011504542 reported the affected
transport path in gRPC v1.83.1. The generated runtime does not configure xDS.
This record does not claim that the reported panic was reproduced in Hypershell.

`grpc-application` 1.12.2 and `otel-tracing` 1.14.1 require gRPC v1.83.2.
Both example modules use that version. Regeneration and drift checks passed.
The generated gRPC runtime test sends a request without either host header.
It requires HTTP 400 and gRPC status Internal. Normal authenticated requests
must still succeed. The focused generated runtime check passed in 24.254
seconds, with race detection. Full compiler and consumer CI remain required.

The CNPG test that was active when CI reported the issue retains its frozen
source and v1.83.1. Its functional results do not qualify the old dependency
for release. New application checks must use the updated compiler and modules.

Sources:

- [Go vulnerability record](https://vuln.go.dev/ID/GO-2026-6443.json)
- [gRPC v1.83.2 release](https://github.com/grpc/grpc-go/releases/tag/v1.83.2)
- [Hypershell CI run](https://github.com/jsell-rh/hypershell-stego/actions/runs/35011504542)
