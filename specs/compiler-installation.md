# Compiler installation

Use `scripts/install-compiler.py` from a trusted STEGO checkout. It installs the
Linux amd64 compiler for an explicit full source commit. It does not build or
execute the compiler. Consumers do not need their own download or signature
verification implementation.

```sh
python3 -B /trusted/stego/scripts/install-compiler.py \
  --release --revision FULL_LOWERCASE_COMMIT_SHA \
  --gh /usr/bin/gh --output /private/compiler
```

The output parent must exist. The output directory must be new. The installer
does not replace an existing compiler, select a newer revision, or fall back to
a source build. Linux amd64 is the only supported installation target.

The release tag is `compiler-<full-source-commit>`. The release must be published,
immutable, and assigned to that exact commit. It must contain exactly these four
assets: `stego-linux-amd64`, `build.json`, `SHA256SUMS`, and `provenance.jsonl`.
The installer checks asset IDs, states, sizes, and SHA-256 digests. It uses fixed
GitHub API paths. It does not follow URLs supplied in the release metadata.

Each API command has a 90-second limit. Release metadata is limited to 1 MiB.
Asset limits are 64 MiB for the compiler, 1 MiB for the build record, 1 KiB for
checksums, and 4 MiB for signatures. The command supervisor also limits output
and stops child processes on timeout or interruption. Downloaded files stay in
a private temporary directory until verification finishes.

Release metadata and checksums are not trust anchors. The installer calls the
[common signature verifier](compiler-provenance.md) on captured copies. Both
the compiler and build record must have valid signatures from the selected
main-branch workflow and source commit. Only those verified bytes enter the new
private output directory. Saved `verified.json` files are never accepted as a
substitute for signature verification.

An operator can supply the same four files in a local directory:

```sh
python3 -B /trusted/stego/scripts/install-compiler.py \
  --package /inputs/compiler --revision FULL_LOWERCASE_COMMIT_SHA \
  --gh /usr/bin/gh --output /private/compiler
```

This mode does not download a release. Signature verification still uses the
installed GitHub CLI and its normal trust roots. It is not a complete offline
build contract. Registry inputs, application dependencies, build tools, and an
offline signature trust-root procedure remain separate requirements.

## Release policy

Publish only a compiler whose exact source passed the full compiler checks and
the signed artifact checks. Check both completed runs. Signing alone does not
establish a passing compiler suite. Retain the source, run IDs, and artifact
hashes with the release record.

Enable repository release immutability before publication. Create a draft at the
full commit, attach the four already authenticated files, verify each uploaded
asset, then publish without making it the default latest release. Never replace
an existing release or tag. A missing release is an error, not permission to
select a different compiler. GitHub documents the
[immutable release behavior](https://docs.github.com/en/code-security/concepts/supply-chain-security/immutable-releases).

Automatic release qualification and Hypershell script integration are not yet
implemented. The installer does not establish source correctness, a trusted
builder operating system, or an independent toolchain build.


## First published package

The [immutable release for `00573709fb15a2a54de4242aa8fdbabee325179a`](https://github.com/jsell-rh/stego/releases/tag/compiler-00573709fb15a2a54de4242aa8fdbabee325179a)
contains the compiler already selected by Hypershell. All six compiler jobs
passed in run `35194639697`; build and signature jobs passed in `35194639679`.
Both runs were checked again before publication. The release has ID `390573511`.
Repository release immutability is enabled. The tag names the exact commit.
All four uploaded assets were downloaded and compared before publication.

Installer source `7bb4cc9` accepted both real signatures from the local package
and then from the published release. Both installations have identical files
and verification records. The 31,712,090-byte compiler SHA-256 is
`e5894237467e81c6a3e7a7c8abd436192a74174726384c2f30716e63db3101bb`.
The compiler was not executed on the workstation. Six small installer checks
and eight common verifier checks passed locally. The separate installation CI
job repeats real release and local package verification without compiler execution.
Its result is pending. Hypershell regeneration scripts are not yet changed.
