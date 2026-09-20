# Application build boundary

This source review covers STEGO `3be6bce` and Hypershell `d24e9da`.
It separates generated build instructions from application artifact evidence.
It does not qualify the current application or close C3. The
[source record](application-build-boundary-evidence.json) identifies the reviewed
files. The current Gateway workflow must finish before another runtime change.

STEGO `kubernetes-service` supplies a common `imageFiles` function for service,
worker, and RPC images. It selects a builder image by digest. The generated
instructions disable workspace selection, automatic toolchain selection, and
CGO. They verify module content and use a read-only module build with trimmed
paths. The runtime image contains the executable and CA certificates, and uses
user 65532. Source directories must be distinct top-level names. The build
context excludes hidden files and selected private-key file types. This is a
file selection rule, not proof that every selected file contains no secret.

These controls already belong in STEGO. Hypershell supplies its packages and
entry points. It does not need its own copy of the container build generator.
The common renderer test checks repeated output and rejects a broad `COPY .`.
That test alone does not build each container or identify its final executable.

Hypershell has two application build paths. Its generated Gateway console CI
builds an executable directly and through the generated Containerfile. It
compares the executable bytes, publishes the image, pulls it by digest, and
compares the image identity. The live browser fixture instead builds service,
console, RPC, and worker executables in its bounded test Pod. Its handwritten
publisher creates image layers and reports image metadata. The live test also
records source transfer, repeated generation, and the signed compiler. These
are useful application checks, but the two build paths have different evidence.

The signed compiler package authenticates the compiler. It does not authenticate
each application executable or establish all application build inputs. The
generation input manifest also excludes some application and tool inputs by
design. The current common image generator emits build instructions and ignore
rules; it does not emit an application artifact record or a verifier for it.

The next common build capability must connect the selected source and generated
state to the actual executable. Its record must identify the compiler, selected
files, module and replacement content, toolchain, platform, build settings, and
executable digest. An image record must also identify the actual image and
included trust store. Authentication of that record is a separate requirement
from recording hashes. Do not record credentials or complete environment values.

Verify this capability with an independent service and Hypershell. Checks must
reject changed inputs, unexpected build settings, mismatched executables, and
mismatched image records. They must cover a second clean build and the actual
published artifact. Domain packages and image destinations remain application
choices. The build mechanism and record checks belong in STEGO. This review
defines the remaining evidence boundary; it adds no build runtime behavior.
