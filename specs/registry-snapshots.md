# Registry input snapshots

STEGO now captures registry YAML and protobuf files before generation. Metadata,
slot contracts, and direct slot imports use these captured bytes. They do not
read the files again during generation. A caller receives a copy when it reads
a captured file.

The compiler checks the registry again before it returns a plan and before it
commits an apply transaction. The apply check runs under the project lock.
Dependency resolution checks the registry before commands run and before it
commits their results. A changed registry causes an error that requires a new
plan. Fill creation also uses captured slot bytes and checks the registry before
it writes the scaffold.

The previous compiler accepted registry edits during generation and between plan
and apply. Regression tests first failed against that behavior. It also skipped
missing or malformed slot imports. Imports now require valid relative paths and
captured protobuf files. Invalid imports stop generation without output. The
optional `stego/` import prefix remains supported.

## Saved identity

Applied state now contains `registry_content_sha256`. The existing `registry_sha`
and component `sha` fields retain their supplied references. A reference can be
a local label, such as `gateway-workflow`; it is not proof of registry content.

The content digest uses this encoding:

1. Start SHA-256 with the bytes `stego-registry-content-v1` and one zero byte.
2. Sort the relative file paths in byte order. Paths use `/` as the separator.
3. For each file, append its path length as an unsigned 64-bit big-endian integer,
   the path bytes, its content length with the same integer encoding, and the
   32 raw bytes of its SHA-256 content digest.
4. Store the final digest as 64 lowercase hexadecimal characters.

All regular `.yaml` and `.proto` files under the registry root are included.
The top-level `.git` entry is excluded. Other regular files are ignored.
Directory paths are checked separately in a live plan. Thus, a new empty artifact
directory invalidates an existing plan, although directory names alone do not
change the saved content digest. Moving the same registry to another root does
not change its digest.

Old state without this field remains readable. Its registry content identity is
unknown until the next apply records it. An input comment can change the digest
without changing generated code; apply then updates only state. Older compiler
versions can reject the new field under strict state decoding. Upgrade the
compiler pin and state together.

## Bounds and limits

Registry reads stay within an `os.Root`. The loader rejects symbolic links and
special files. Each input file is limited to 4 MiB. The full captured input set
is limited to 64 MiB and 4,096 files. Directory traversal is limited to 16,384
entries and 16 levels below the root. Relative paths are limited to 1,024 bytes.
The top-level `.git` entry counts as one entry; its contents are not visited.

These are byte and traversal bounds. They are not a time limit on filesystem I/O.
STEGO does not lock external registry writers or take an atomic filesystem
snapshot. Verification cannot prevent a writer from changing files after the
last check. Generated output still comes from the captured input bytes. Existing
transaction recovery uses its recorded transaction, not the current registry.

The digest does not identify the compiler executable, fill code, application
dependencies, or all configuration inputs. It is not a signature or proof that
an input is trusted. Complete build identity, input manifests, and isolated
builds remain open. This change also retains the current limited protobuf
parser; it does not add full protobuf or transitive import support.

## Verification and cost

Tests cover captured-byte isolation, edits during generation and after planning,
apply after lock contention, dependency-time edits, changed plan identity, state
upgrade, unchanged generated output, invalid imports, registry relocation, input
bounds, and invalid file types. Digest tests change file names and equal-length
content. The full repository race suite passed. A final focused race run passed
for the compiler, registry, parser, and CLI packages. `go vet ./...` passed.

Three local benchmark samples used Go 1.26.8 on Linux amd64 with an Intel Core
Ultra 9 185H. The registry contained 28 input files with 9,514 bytes in total.
Each sample used a 200 ms benchmark interval.

| Operation | Time per call | Bytes allocated per call | Allocations per call |
| --- | --- | --- | --- |
| Previous load | 601,609–640,226 ns | 310,837–310,963 | 4,638 |
| Captured load | 1,159,225–1,230,746 ns | 418,797–418,941 | 6,173 |
| Verify captured registry | 587,390–614,702 ns | 104,239–104,293 | 1,375 |

The new loader costs more because it reads and hashes the full input set.
Verification adds another read. A shared read buffer limits allocation cost.
These measurements cover a small local registry. They do not establish complete
compilation cost or a production performance limit.

## Hypershell evidence

Hypershell commit `028c54f32f2ed5c7a3fe6a7100849c623f94e525` pins compiler
`2837ae62d042dc11e54643d3c2df8a89a26c8992`. Generation added the registry
digest to state. All 73 generated and dependency files kept the same hashes.
An independent Python calculation used the encoding above and matched the saved
digest for the 11 local registry inputs:
`a75ade9496abf00b6ff64ecc32a99c48197057779f5005906cf28c4b1b217f8f`.

Contract race tests passed in 1.424 seconds. The application build passed.
After the application commit, `scripts/generate.sh --check` passed and all 74
generated, dependency, and state hashes matched. Both feature commits were
pushed to `main`.

The PostgreSQL, Keycloak, and Kubernetes workflows were not repeated locally for
this state and compiler pin change. Application source, generated code, and
dependencies are unchanged. The previous application evidence remains recorded
in [gRPC stream headers](grpc-stream-headers.md) and
[identity query reads](identity-query-reads.md).
