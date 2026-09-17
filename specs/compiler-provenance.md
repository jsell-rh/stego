# Compiler artifact origin verification

The compiler artifact workflow has a separate signing job. It runs only after
a successful build on a push to `main` in `jsell-rh/stego`. Pull requests,
development branches, and manual runs do not publish these signatures. The
build job retains read-only repository permission. Only the signing job has
OIDC and attestation write permission.

The signing job retrieves the build job's exact artifact ID from the same run.
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

A signature authenticates a statement from the selected workflow. It does not
prove that the source is correct or that the hosted builder and Go toolchain
were uncompromised. Main signing does not wait for the separate full compiler
workflow; check that result before selecting a compiler revision for production.
This is not a permanent release channel or automatic consumer installation.
Supported-platform coverage, complete application build inputs, independent
toolchain trust, release policy, and full offline distribution remain open.
