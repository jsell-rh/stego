# Static Go application images

This is candidate code. It is not a completed image publication or provenance
check. Do not close C3 from these unit tests.

The common image command accepts a native application build record, its trusted
digest, and the matching executable. It also requires an explicit CA bundle and
trusted bundle digest. It does not copy the host's trust store. The caller must
authenticate these inputs. The image record identifies the image compiler and
its executable, the native build record, application, CA bundle, configuration,
manifest, compressed layer, and uncompressed layer.

The image uses the OCI format through go-containerregistry v0.22.1. The selected
profile contains one static Go executable and one CA bundle. It uses Linux amd64,
user 65532:65532, read-only files, and a fixed entry point. The container platform
must still apply its Pod security, resource, and network policies.

Verification checks exact configuration and filesystem content. It rejects
extra files, links, device files, changed ownership or modes, unknown metadata,
changed hashes, and excessive sizes. It checks the executable extracted from the
image against the native build record without executing it. Partial work stays
in a new private directory for inspection; it is not a successful image result.

The image record does not authenticate itself. Signed records, actual registry
publication and retrieval, container engine checks, and use in the Hypershell
publisher remain required. The actual Hypershell API and the independent example
must exercise the same common image mechanism before it can be adopted.

`stego image export` verifies the image and its native executable record before
it writes a Docker-format transport archive. The OCI manifest remains the
publication identity. The export record identifies the transport bytes and the
unchanged configuration. The saved archive is read again with the library, and
its configuration, layer, and filesystem hashes must match. CI loads this
archive into Docker and reads the files from a stopped container. This check
does not claim that a registry published the OCI manifest.
