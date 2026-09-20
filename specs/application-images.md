# Static Go application images

This is candidate code. Native images for the independent example and current
Hypershell API passed the artifact and container-engine checks. Registry
publication and record authentication remain open. C3 is not complete.

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
now use the same mechanism in the candidate CI check. The production publisher
has not adopted it.

`stego image export` verifies the image and its native executable record before
it writes a Docker-format transport archive. The OCI manifest remains the
publication identity. The export record identifies the transport bytes and the
unchanged configuration. The saved archive is read again with the library, and
its configuration, layer, and filesystem hashes must match. CI loads this
archive into Docker and reads the files from a stopped container. This check
does not claim that a registry published the OCI manifest.

## Candidate evidence

[Application run 35507655343](https://github.com/jsell-rh/stego/actions/runs/35507655343)
used compiler source `39c7c631b07c80869b2936b50e1be50e3cb797b5` and unchanged
Hypershell source `0589cfc08d40591e3fc0b36ac1538e6f2ae0d917`. Each application
built twice. Its image and Docker transport also matched across two output
paths. Docker loaded each archive, created a container without starting it,
and returned executable and CA files with the expected hashes. The container
was then removed. The check used no privileged container.

Independent artifact inspection checked the source files, native records, image
records, configuration, compressed and uncompressed layer hashes, file content,
ownership, modes, and Docker image identity. Each application passed ten native
verification cases and seven image verification cases. See the
[evidence record](application-images-evidence.json) for exact identities.

[Image check 35507655491](https://github.com/jsell-rh/stego/actions/runs/35507655491)
passed all 25 common build and image test groups, with 83 cases including
subtests. No group failed or was skipped. Dependency files stayed unchanged.
The tests require the expected rejection reason, including for images with new
hashes but unsafe user, environment, permissions, links, or CA content.

Earlier failed checks remain recorded. The first exposed an index descriptor
field that the library adds. The required profile now checks that exact field.
The next reached Docker, whose older loader rejected an OCI layout archive.
The common export command now supplies the Docker transport format and verifies
its saved bytes. The OCI publication identity remains separate.

The test CA is an explicit public fixture, not a production CA selection. These
checks did not start the application, publish to a registry, authenticate image
records, or replace the live Gateway workflow. Full compiler CI for this exact
candidate remains pending. Production publisher integration is still required.
