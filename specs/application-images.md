# Static Go application images

Compiler `c92f591` is published as an authenticated immutable release. Common
image delivery source `a82691b` passed the source, native image, real signature,
and private TLS registry checks for the example and Hypershell API. The complete
Hypershell workflow with this new image path remains unproved. C3 is not complete.
See the [current delivery evidence](application-delivery-evidence.json). Later
sections retain source-specific results from earlier candidates.

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

The optional `--image` argument captures the selected OCI files in `output/oci`
after authentication. The capture checks the signed manifest, configuration,
and layer sizes and digests. It rejects missing files, extra entries, links,
and special files. It uses directory descriptors and bounded reads, and writes
private copies. The OCI marker and index have size limits but are not accepted
as valid OCI data by this step. The result adds `image_blob_bytes_checked: true`
and keeps `image_contents_checked: false`. The consumer must still use the
compiler to check the complete image. A failed capture leaves no result
directory.

`scripts/application-images.py stage` applies a trusted reusable signer policy
to all targets in an application declaration. The policy omits `module`,
`target`, and `entrypoint`; these fields come from the trusted declaration.
The caller supplies the selected workflow run, attempt, and downloaded artifact
directory. The command authenticates and captures every image before it writes
the complete set. `images.json` records the selected targets and digests. Its
SHA-256 digest is an input to the publication step and must travel through a
trusted channel with the publishing compiler digest.

`scripts/application-images.py publish` first checks that set digest and copies
the selected compiler after a digest check. It checks the full source snapshot
and each image with this private compiler before it reads registry credentials
or publishes an image. It accepts either a private canonical credential file
or an explicit token file and username. The token option permits Kubernetes
projected files. Token contents do not enter command arguments or result files.
Compiler subprocesses use a restricted environment, bounded output, and time
limits. The command has a 15-minute limit for the complete set.

The publisher checks each registry receipt before it returns a digest reference.
Only `publication.json` marks a complete set. A failed publication can leave
individual images in the registry and diagnostic records in the result
directory. It removes private compiler and credential copies on success or
failure. The caller must retain the records and inspect the failed operation
before a retry. The default delivery profile uses one registry origin for
authentication and storage. The optional `--registry-policy` file can approve
external destinations. It has format 1 and two required arrays, `token_origins`
and `blob_origins`. Each array can select at most eight explicit HTTPS origins.
The common compiler validates the origins and keeps credential destinations
separate from blob destinations. Blob requests cannot send registry credentials
or request bodies. The publisher checks the same origin lists in each receipt.
The registry CA bundle must cover all selected destinations. The caller must
obtain this policy from trusted operator configuration, not from a redirect.

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

This candidate still uses the test CA fixture.
[Full compiler run 35508205464](https://github.com/jsell-rh/stego/actions/runs/35508205464)
and the reproducible compiler artifact check also passed independent
verification. Registry publication and the Hypershell production publisher
remain open.

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

The qualified CI gate published the actual example and Hypershell API images
to a private TLS registry, repeated publication, and retrieved them. It also
checked rejection of changed records, registry CA selections, and credentials.
The complete live Hypershell browser gate used the shared publisher for all
seven images in run `35512430605`. These checks do not approve a production
registry deployment or application CA profile.

A separate bounded CI job captured the public CA file from the same pinned
SDK image used by the existing publisher. It created a restricted container to
copy the file and never started that container. It recorded the source image,
image configuration, bundle digest, certificate digests, and cleanup result.
Production adoption of that CA bundle remains open.

[Registry candidate run 35508605666](https://github.com/jsell-rh/stego/actions/runs/35508605666)
passed all 32 build, image, and registry test groups, with 101 cases including
subtests. It then failed the required dependency-lock check. The added indirect
dependencies were reviewed and committed. No existing dependency version changed.
The corrected source passed. Published compiler `c92f591` passed the full
compiler and application image checks. The original failed result is retained.

The same run captured the existing publisher CA bundle: 224,449 bytes, 150 CA
certificates, digest `714d457d580922dbf1d0be8bd35ba236a842b50b0072ae791582a19adef772a5`.
Independent inspection confirmed all certificate digests and CA constraints.
The bundle contains the Baltimore CyberTrust Root, which expired on 2025-05-12.
This is evidence about the current publisher input, not approval of that bundle
as the new production trust profile. Selection and update of the production root
set, its source notices, and live adoption remain open.

## Application target declaration

An application can select its build targets in `.ci/application-images.json`.
The format is an object with `format: 1` and an `images` array. Each image has
four fields: `name`, `module`, `target`, and `entrypoint`. The module path is
relative to the application source root. The target path is relative to that
module. The entry point is the executable name at the image root.

`scripts/application-build-matrix.py` checks this declaration before CI uses it.
It rejects unknown or repeated fields, repeated image names, unsafe paths, and
shell syntax. It reads only a regular file of at most 16 KiB. One matrix can
contain between 1 and 64 images. This CI bound does not limit Gateway capacity.

The `Consumer image targets` workflow selects the declaration from Hypershell
source `179293ccd172a9ef191613da47268add65eb22b8`. It checks seven targets: the
API, console, Gateway console backend, provisioner RPC process, namespace
allocator, identity worker, and workload worker. At most two target jobs run at
the same time. Each job builds twice, checks the native record, builds the image
twice, checks a stopped container, and publishes and retrieves through the
private TLS registry. STEGO owns these common checks. Hypershell owns the target
list.

The qualified consumer run `35511212112` built and signed all seven images from
fixture `bb1a494`. Each target passed independent source, native image, registry,
and signature checks. Live run `35512430605` then used these exact images for
the complete Gateway browser workflow. The images use the test CA fixture;
this result does not approve them for production deployment.

## Reusable signer policy

Application record policy format 2 separates the caller source from the reusable
signer. `repository`, `reference`, and `revision` select the caller repository,
branch, and exact commit. `signer_repository`, `signer_workflow`, and
`signer_revision` select the reusable workflow and exact commit. Format 2 replaces
the format 1 `workflow` field with these three signer fields. All application,
compiler, entry-point, and CA selections remain required.

The verifier checks the caller and signer certificate claims separately. It uses
`--signer-workflow` with the pinned signer digest. It does not combine that option
with `--cert-identity`; the GitHub CLI rejects that combination. Format 1 keeps
its existing exact certificate identity check. See the
[GitHub CLI verification reference](https://cli.github.com/manual/gh_attestation_verify).

The reusable signing job must use pinned STEGO verification code. It must not
execute verification code selected by the application caller. Each signing job
must retrieve its own build artifact by immutable artifact ID and check the
record digests from that build job. A shared matrix output cannot identify all
target artifacts. Real reusable-workflow signing checks remain required.


The common `.github/workflows/application-images.yml` workflow reads the selected
application declaration and calls `.github/workflows/application-target.yml`
once for each image. The latter workflow has separate build and signing jobs.
The build job has only read permission and checks that no OIDC request credentials
are present. The signing job downloads only the immutable record artifact from
that invocation. It compares both record digests before it signs them. It then
checks both signatures and eleven invalid record or policy selections.

The workflow pins compiler and verification code to
`ff0f902e258e6e85ed253daefc6767bf33c2f54d`. This pin is part of the reviewed workflow;
it is not a caller input. The consumer selects the application repository, exact
application commit, and exact signer workflow commit. Its independent policy
must still pin the expected compiler bytes, target, and CA digest. Signature
verification does not approve those choices for production.

The STEGO `Consumer image targets` workflow is a test caller for all seven
Hypershell images. Hypershell consumer run `35511212112` also passed real signing
through reusable signer `d863fcb`. All seven targets passed the eleven invalid
record or policy cases. The live workflow used the authenticated records and
checked the actual source and image contents before publication. These checks
still use the test CA. See the [current evidence](application-delivery-evidence.json).
