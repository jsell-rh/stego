STEGO saves a versioned project input manifest in `last_applied.inputs`.
It records the files captured for generation: `service.yaml`, `go.mod`, `go.sum`,
`.stego/config.yaml`, and every file declared by a generator's `InputFiles`
method. Optional files record absence explicitly. A missing file and an empty
file have different identities. Each present record has a content SHA-256 and
permission mode.

The options record contains the module name, Go version argument, relative
output directory, and registry reference supplied to `Reconcile`. The Go version
argument is not necessarily the final module version: a component can require
a higher minimum. Project and registry absolute paths are not part of the
manifest. Registry content and compiler build metadata remain separate applied
state fields. The previous state file is excluded from the input manifest;
transaction snapshots retain its role in apply and recovery.

Input changes update state even if generated content is unchanged. A fresh apply
can create or change `go.mod`; this becomes a new input on the next plan. That
next apply can therefore update state alone. Once inputs and options are stable,
repeat generation has no changes. `stego deps` permits a state-only change when
generated files and entity definitions are current. It still rejects unapplied
code. Use the existing `apply`, `deps`, `apply` sequence to record resolved module
inputs. Module files remain owned by the application.

Module merging now uses the captured `go.mod` bytes. It does not reread the file
after capture. Apply checks the live snapshots and the private manifest digest
before its transaction. A caller cannot substitute a new public manifest and
recompute its hash to pass that private check. Old state files can omit the
manifest; the next apply adds it. Old compilers can reject the new state field,
so upgrade the compiler pin and state together.

Manifest version 1 permits at most 1024 file records. Paths and option strings
have a 1024-byte limit and must contain valid UTF-8 with no NUL. Paths must be
canonical and relative. Generated output and private STEGO state cannot be
claimed as generator inputs. The only permitted `.stego` input is the project
configuration. The compiler also checks the total serialized state against the
4 MiB loader limit before direct save or transaction creation.

The digest codec is SHA-256 over these bytes, in this order:

1. The UTF-8 prefix `stego-project-inputs-v1` followed by a zero byte.
2. Module name, Go version argument, output directory, and registry reference.
   Each string has an unsigned 64-bit big-endian byte length followed by UTF-8
   bytes.
3. The file count as an unsigned 64-bit big-endian integer.
4. Each file record in byte order by relative path. A record contains the framed
   path, one existence byte (0 or 1), an unsigned 32-bit big-endian permission
   mode, and the framed lowercase hex content digest. An absent file has mode
   zero and an empty digest.

The record contains hashes rather than source contents. It supports input
comparison; it is not a signature, compiler artifact digest, or complete
application build manifest. Undeclared external reads by a custom generator,
external tool binaries, tool environment, dependency source trees, and domain
code used by the application build are not identified by this project manifest.
Controlled builds and full artifact verification remain open. Different input
permission modes also produce different manifest digests.

Tests cover changed inputs with identical output, creation of an optional module,
stable repeat generation, option changes, legacy state, manifest substitution,
malformed records, captured module bytes, and oversized state before writes.
A fixed digest vector was computed with an independent Python implementation.
The Hypershell test checks its declaration, configuration, module files, and ten
protobuf inputs against the saved records.

The full STEGO race suite and vet passed. The compiler package took 24.739
seconds in that full run. Further focused compiler checks cover the final
bounded codec and state validation.

On Go 1.26.8, Linux amd64, Intel Core Ultra 9 185H, three 200 ms samples of
manifest construction and validation measured 14.422–15.502 microseconds,
7752 bytes, and 125 allocations for 14 records. At the 1024-record limit, they
measured 1.172–1.291 ms, about 610.5 kB, and 8207 allocations. These measurements
exclude file reads, YAML encoding, component generation, and application builds.
