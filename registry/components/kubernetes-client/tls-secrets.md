# Server TLS Secrets

Version 1.8.0 supplies `VerifyServerTLSSecret` and `ServerTLSSecretTarget`.
The caller supplies the expected namespace, Secret name, DNS name, owner,
and explicit CA pool. The runtime checks the API type, Secret type, identity,
deletion state, certificate chain, hostname, expiry, server use, and private key.
It rejects absent trust. The Secret's `ca.crt` field cannot add trust anchors.

The function permits at most 512 KiB of certificate data, 64 KiB of private key
data, and 16 certificates. Failed checks return `ErrResourceObservation` without
certificate, key, hostname, or parser details. A successful result contains only
the certificate bytes. The function does not write, call an API, check certificate
revocation, or prove live connectivity. The caller must not change the CA pool
during verification.

The application supplies certificate creation policy and handles an absent or
pending Secret. It must verify the public service before it publishes an address.
The generated check applies to any service that consumes a Kubernetes TLS Secret.
