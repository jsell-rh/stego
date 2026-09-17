# Compiler artifact origin verification

The compiler artifact workflow has a separate signing job. It runs only after
a successful build on a push to `main` in `jsell-rh/stego`. Pull requests,
development branches, and manual runs do not publish these signatures. The
build job retains read-only repository permission. Only the signing job has
OIDC and attestation write permission.

The signing job retrieves the build job's exact artifact ID from the same run.
It requires both downloaded file hashes to match the build job's outputs before
it permits signing. A download warning cannot bypass this explicit comparison.
It signs the executable and `build.json` with a pinned `actions/attest` action.
The action creates a SLSA provenance statement and a Sigstore signature. These
are the [GitHub artifact attestation mechanisms](https://docs.github.com/en/actions/how-tos/secure-your-work/use-artifact-attestations/use-artifact-attestations).
No long-lived signing key is stored in STEGO.

## Consumer policy

Use `scripts/verify-compiler-artifact.py` from a trusted STEGO checkout. Supply
an independently selected full source commit, the downloaded artifact directory,
the saved attestation bundle, and an installed GitHub CLI by absolute path.
The verifier must not come from an unverified artifact. The GitHub CLI and its
normal signature trust roots are trusted dependencies of this procedure.

```sh
python3 -B /trusted/stego/scripts/verify-compiler-artifact.py \
  --inputs /downloads/compiler \
  --bundle /downloads/provenance/provenance.jsonl \
  --revision FULL_LOWERCASE_COMMIT_SHA \
  --gh /usr/bin/gh \
  --output /private/verified-compiler
```

The parent of the result directory must exist. The result directory must not
exist. Input files have size limits and must be regular files. Links and special
files are rejected. Verification uses private copies, so a later change to a
downloaded file cannot replace the bytes copied to the result directory.

For both the executable and build record, the
[GitHub CLI verifier](https://cli.github.com/manual/gh_attestation_verify)
must accept all of these requirements:

- The repository is `jsell-rh/stego` on `github.com`.
- The signing workflow is `.github/workflows/compiler-artifact.yml`.
- The exact certificate identity ends in `@refs/heads/main`.
- The OIDC issuer is `https://token.actions.githubusercontent.com`.
- The source reference is `refs/heads/main`.
- The source commit and signing workflow commit equal the selected commit.
- The predicate is `https://slsa.dev/provenance/v1`.
- The runner is hosted by GitHub.

The exact certificate identity selects the workflow and branch. The CLI does
not permit a separate `--signer-workflow` selector with `--cert-identity`.

The wrapper fixes these requirements. It does not accept an alternate host,
workflow, reference, issuer, or trust root. It retains operator authentication
settings, but removes inherited debug, proxy, TLS-root, loader, and executable
path overrides. Each CLI command has a 90-second time limit and a 4 MiB output
limit. Failure or interruption stops its process group.

After both signatures pass, the verifier checks the signed source and target,
the binary size and hash, and the exact checksum list. It then copies the checked
bytes and bundle into a new private result directory. It writes `verified.json`
last. It does not execute the compiler. Use only that private result directory
for later execution. A failure before publication leaves no result directory;
a failed copy removes the incomplete directory.

## CI checks and limits

Eight small local checks cover the wrapper's failure paths and input capture.
They use a substitute signature checker. They do not prove cryptography.
The main signing job separately verifies real signatures, then requires denial
for a different source commit, a changed executable with matching replacement
checksums, a changed build record, and an invalid signature bundle. No success
for that live check is claimed until its result is inspected.

A successful signing job saves `compiler-provenance-<full-source-commit>` with
the verified compiler, build record, checksums, bundle, verification record, and
rejection results. The original unsigned build artifact remains separate. Both
artifacts have seven-day retention. Only the verified provenance result shows
that this consumer policy passed.

The separate `compiler-attestation-<full-source-commit>` artifact retains the
signature bundle before consumer verification. It permits inspection if that
check fails. Its presence alone does not establish a passing consumer check.

A signature authenticates a statement from the selected workflow. It does not
prove that the source is correct or that the hosted builder and Go toolchain
were uncompromised. Main signing does not wait for the separate full compiler
workflow; check that result before selecting a compiler revision for production.
This is not a permanent release channel or automatic consumer installation.
Supported-platform coverage, complete application build inputs, independent
toolchain trust, release policy, and full offline distribution remain open.

## First signature check and correction

[Main run 35191792983](https://github.com/jsell-rh/stego/actions/runs/35191792983)
built and signed the compiler at `842728b`. Consumer verification failed because
the command supplied two mutually exclusive identity selectors. No verified
package was published by that run. The run remains a failure.

The corrected command uses the exact certificate identity. All repository,
source commit, signer commit, issuer, predicate, and runner checks remain.
Independent checks with GitHub CLI 2.87.3 accepted both real signatures from
the failed run. They rejected another source commit, changed compiler bytes
with matching replacement checksums, a changed toolchain field in the build
record with matching replacement checksums, and an invalid bundle. Valid inputs
passed again after these rejection checks. The compiler was not executed.

The authenticated compiler SHA-256 was
`20fa98a3ec27f0fc6c6acf92072b70d1da1d61f065c2447fdefa572f7cb94f6a`.
The build-record SHA-256 was
`68c4936b7ab3912f87b78765d439cd36266f23ab17d6c9d2cfb895f555d09014`.
This proves the corrected consumer command against those retained signatures.
The corrected workflow still needs its own complete CI result.
