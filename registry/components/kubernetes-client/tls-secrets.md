# Server TLS Secrets

Version 1.8.1 supplies `VerifyServerTLSSecret` and `ServerTLSSecretTarget`.
The caller supplies the expected namespace, Secret name, DNS name, owner,
and explicit CA pool. The runtime checks the API type, Secret type, identity,
deletion state, certificate chain, hostname, expiry, server use, and private key.
It rejects absent trust. The Secret's `ca.crt` field cannot add trust anchors.

The function permits at most 512 KiB of certificate data, 64 KiB of private key
data, and 16 certificates. Failed checks return `ErrResourceObservation` without
certificate, key, hostname, or parser details. A successful result contains only
the certificate bytes. Private keys, other PEM blocks, malformed block prefixes,
and extra text in the certificate field are rejected. Version 1.8.0 used the
standard TLS parser without this additional check. That parser can skip such
blocks, which could return private data with the certificate. Regression checks
reproduced the defect before this correction.

The function does not write, call an API, check certificate
revocation, or prove live connectivity. The caller must not change the CA pool
during verification.

The application supplies certificate creation policy and handles an absent or
pending Secret. It must verify the public service before it publishes an address.
The generated check applies to any service that consumes a Kubernetes TLS Secret.

Version 1.9.0 adds `ParseServerTLSRoots` for operator-supplied certificate data.
It returns a private CA pool. It accepts at most 512 KiB and 256 certificates.
It rejects empty input, private material, malformed blocks, PEM headers, and
text outside the certificate blocks. It does not read system roots, files,
or workload Secrets. The caller supplies trusted installation data and bounds
any file read before this call. Changes to the input bytes do not change the
returned pool. Certificate parsing is shared with the Secret verifier.

The focused generated checks passed on 2026-09-15. They cover independent
trust, invalid input, and use after input changes. Full compiler CI and the
Hypershell adoption checks remain required.
