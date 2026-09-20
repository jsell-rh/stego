# Static Go application images

This is candidate code. Native images for the independent example and current
Hypershell API passed the artifact and container-engine checks. The next
candidate also passed real record signature checks. Registry publication
remains open. C3 is not complete.

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

The image record does not authenticate itself. The signed-record candidate
checks its origin against an explicit consumer policy. Actual registry
publication, retrieval, and use in the Hypershell publisher remain required. The actual Hypershell API and the independent example
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
records, or replace the live Gateway workflow. Production publisher integration
is still required.

[Full compiler run 35507655349](https://github.com/jsell-rh/stego/actions/runs/35507655349)
failed in the generated PostgreSQL transaction suite. Its one-minute package
limit expired during database cleanup. The log contains 59.33 seconds of
completed test cases, including subtests. It reports no earlier assertion
failure or race. The result is a failure, not a qualified compiler release.

The test harness now gives the complete suite three minutes and disables cached
results. Each database setup and cleanup still has a five-second limit. This
changes the test budget only. The corrected source must pass full CI before
promotion.

## Signed record candidate

`scripts/verify-application-records.py` accepts an explicit consumer policy and
checks GitHub attestations for both the native build record and the image record.
The policy selects the repository, workflow, branch, and full workflow commit.
It also selects the application commit and build target, the compiler commit and
executable digest, the entry point, and the CA bundle digest. Each selection is
required. A consumer must obtain this policy from its trusted configuration. A
policy supplied with an untrusted image does not establish trust.

The verifier uses the same bounded GitHub CLI call as the compiler installer.
It requires the GitHub Actions issuer, the exact workflow certificate identity,
SLSA provenance, and GitHub-hosted runners. It first captures private copies of
the records and signature bundle. Both signatures and all selected bindings
must pass before it creates the result directory. It does not execute the
application or the compiler.

The result includes the authenticated image record digest. The consumer must
then give that digest and the retained records to `stego image verify` to check
the actual image. Signatures do not replace image content checks, vulnerability
checks, or the live application workflow.

The application build workflow has separate build and signing jobs. Build jobs
have no signing permission. Signing jobs download records by artifact ID and
compare their digests with the build job outputs before signing. Each job then
checks the real signatures and rejection paths. Candidate branches can produce
candidate signatures; the consumer policy must select their exact source. Such
a signature does not authorize a production release.

[Application run 35508205453](https://github.com/jsell-rh/stego/actions/runs/35508205453)
passed for source `4e9991e0e220f5900aefd75fdd452e641a3df52f` and unchanged
Hypershell source `0589cfc08d40591e3fc0b36ac1538e6f2ae0d917`. Each application
passed both real signature checks and all seven rejection cases. Independent
verification authenticated the downloaded records and matched them to the
saved native builds, OCI images, and container-engine results. All 25 common
build and image test groups passed, with 83 cases including subtests.

This candidate still uses the test CA fixture. Full compiler CI remains pending.
Registry publication and the Hypershell production publisher remain open.

## Registry transport candidate

`stego image publish` takes an authenticated image record digest, the native
record, and the OCI image. It checks the complete image before it makes a private
copy for publication. It writes a tag derived from the manifest digest, reads
that tag back, retrieves the image by digest, and checks the native executable
and complete image again. `stego image retrieve` performs the same retrieval
checks. A result record is written only after all checks pass.

Registry access requires an explicit repository and a CA bundle with its trusted
digest. Publication also requires a private credentials file with canonical JSON:
`{"username":"selected-user","password":"selected-secret"}` followed by a
newline. The file must have no group or other permissions. The command does not
use an ambient credential helper, keychain, proxy, or host trust store.

The client requires verified HTTPS for every connection, including loopback.
Separate token origins require explicit approval. Token requests must keep the
selected repository scope. Separate blob origins also require approval and must
not receive credentials or request bodies. Request counts, response headers,
response bytes, total bytes, connections, and operation time have limits. Remote
errors do not include credentials, token responses, or redirect query strings.

The transport uses the pinned registry library for authentication and image
publication. It retrieves the three known manifest, configuration, and layer
objects directly with bounded reads. It does not accept a remote image index,
an alternate platform, or a different manifest in place of the selected image.

The candidate CI gate will publish the actual example and Hypershell API images
to a private TLS registry, repeat publication, and retrieve them. It also checks
rejection of changed records, registry CA selections, and credentials. These
checks are pending. They do not establish a production registry deployment.

A separate bounded CI job will capture the public CA file from the same pinned
SDK image used by the existing publisher. It creates a restricted container to
copy the file and never starts that container. It records the source image,
image configuration, bundle digest, certificate digests, and cleanup result.
Capture and adoption of that CA bundle are still pending.
