#!/usr/bin/env python3
"""Verify signed compiler inputs before copying them to a new private directory."""

import argparse
import importlib.util
import json
import os
from pathlib import Path
import re
import shutil
import signal
import stat
import subprocess
import tempfile


spec = importlib.util.spec_from_file_location("build_control", Path(__file__).with_name("check-compiler-artifact.py"))
control = importlib.util.module_from_spec(spec)
spec.loader.exec_module(control)
CheckError = control.CheckError
REPOSITORY = "jsell-rh/stego"
WORKFLOW = REPOSITORY + "/.github/workflows/compiler-artifact.yml"
REFERENCE = "refs/heads/main"
PREDICATE = "https://slsa.dev/provenance/v1"
BINARY = "stego-linux-amd64"


def regular_bytes(path, limit):
    descriptor = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    with os.fdopen(descriptor, "rb") as stream:
        info = os.fstat(stream.fileno())
        if not stat.S_ISREG(info.st_mode) or not 0 < info.st_size <= limit:
            raise CheckError("An artifact input is not a bounded regular file")
        data = stream.read(limit + 1)
        if len(data) != info.st_size or len(data) > limit:
            raise CheckError("An artifact input changed size or exceeded its limit")
        return data


def unique_object(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise CheckError("The build record has a repeated field")
        result[key] = value
    return result


def verifier_environment():
    # Authentication and its config are operator inputs. Host, proxy, TLS-root,
    # debug, loader, and executable-path overrides must not change this policy.
    allowed = ["HOME", "GH_CONFIG_DIR", "XDG_CONFIG_HOME", "XDG_CACHE_HOME",
               "GH_TOKEN", "GITHUB_TOKEN", "SYSTEMROOT"]
    return dict({k: os.environ[k] for k in allowed if k in os.environ},
                PATH="/usr/bin:/bin", LANG="C", LC_ALL="C", GH_PROMPT_DISABLED="1",
                GH_PAGER="cat", GH_HOST="github.com")


def authenticate(gh, path, bundle, revision, environment):
    authenticate_subject(gh, path, bundle, revision, environment,
                         REPOSITORY, ".github/workflows/compiler-artifact.yml", REFERENCE)


def authenticate_subject(gh, path, bundle, revision, environment, repository, workflow, reference):
    control.command([
        str(gh), "attestation", "verify", str(path), "--bundle", str(bundle),
        "--hostname", "github.com", "--repo", repository,
        "--signer-digest", revision,
        "--cert-identity", "https://github.com/" + repository + "/" + workflow + "@" + reference,
        "--cert-oidc-issuer", "https://token.actions.githubusercontent.com",
        "--source-ref", reference, "--source-digest", revision,
        "--predicate-type", PREDICATE, "--digest-alg", "sha256",
        "--deny-self-hosted-runners", "--format", "json",
    ], path.parent, environment, timeout=90, limit=4 << 20)


def verify(inputs, bundle, revision, output, gh):
    if not re.fullmatch(r"[0-9a-f]{40}", revision):
        raise CheckError("Select a full lowercase source commit ID")
    if inputs.is_symlink() or not inputs.is_dir():
        raise CheckError("The artifact directory must be a real directory")
    if not gh.is_absolute() or not gh.is_file() or not os.access(gh, os.X_OK):
        raise CheckError("Select an installed GitHub CLI by its absolute path")
    output = output.absolute()
    if output.exists() or output.is_symlink() or not output.parent.is_dir():
        raise CheckError("The result directory must be new and its parent must exist")
    environment = verifier_environment()
    # Hash and verify private copies. Changes to the input files after capture
    # cannot replace the bytes that are later copied to the result directory.
    with tempfile.TemporaryDirectory(prefix=".stego-verify-", dir=output.parent) as temporary:
        stage = Path(temporary)
        for name, limit in [(BINARY, 64 << 20), ("build.json", 1 << 20), ("SHA256SUMS", 1024)]:
            (stage / name).write_bytes(regular_bytes(inputs / name, limit))
        saved_bundle = stage / "provenance.jsonl"
        saved_bundle.write_bytes(regular_bytes(bundle, 4 << 20))
        for name in [BINARY, "build.json"]:
            authenticate(gh, stage / name, saved_bundle, revision, environment)
        record = json.loads((stage / "build.json").read_bytes(), object_pairs_hook=unique_object)
        if not isinstance(record, dict) or type(record.get("format")) is not int or record["format"] != 1:
            raise CheckError("The signed build record format is not supported")
        if record.get("source_revision") != revision:
            raise CheckError("The signed build record has a different source revision")
        binary = stage / BINARY
        expected = {"name": BINARY, "sha256": control.digest(binary), "size": binary.stat().st_size}
        if record.get("artifact") != expected:
            raise CheckError("The signed build record does not match the compiler")
        version_record = record.get("version")
        if not isinstance(version_record, dict) or not isinstance(version_record.get("build"), dict):
            raise CheckError("The signed compiler identity is missing")
        version = version_record["build"]
        settings = {"goos": "linux", "goarch": "amd64", "revision": revision,
                    "vcs": "git", "source_state": "clean"}
        if any(version.get(k) != v for k, v in settings.items()):
            raise CheckError("The signed compiler identity does not match the selection")
        checksums = "".join(f"{control.digest(stage / name)}  {name}\n" for name in [BINARY, "build.json"])
        if (stage / "SHA256SUMS").read_bytes() != checksums.encode():
            raise CheckError("The checksum list does not match the authenticated files")
        verified = {"format": 1, "repository": REPOSITORY, "workflow": WORKFLOW,
                    "reference": REFERENCE, "source_revision": revision,
                    "predicate_type": PREDICATE, "self_hosted_runners_allowed": False,
                    "artifact": expected, "build_record_sha256": control.digest(stage / "build.json"),
                    "bundle_sha256": control.digest(saved_bundle)}
        # mkdir must fail if another process has created the requested result.
        output.mkdir(mode=0o700)
        try:
            for name in [BINARY, "build.json", "SHA256SUMS", "provenance.jsonl"]:
                with (output / name).open("xb") as stream:
                    stream.write((stage / name).read_bytes())
                (output / name).chmod(0o700 if name == BINARY else 0o600)
            with (output / "verified.json").open("x") as stream:
                stream.write(json.dumps(verified, indent=2, sort_keys=True) + "\n")
            (output / "verified.json").chmod(0o600)
        except BaseException:
            shutil.rmtree(output)
            raise
    return verified


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--inputs", type=Path, required=True)
    parser.add_argument("--bundle", type=Path, required=True)
    parser.add_argument("--revision", required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--gh", type=Path, required=True)
    args = parser.parse_args()
    def interrupted(_number, _frame):
        raise CheckError("Compiler verification was interrupted")
    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGINT, interrupted)
    os.umask(0o077)
    print(json.dumps(verify(args.inputs, args.bundle, args.revision, args.output, args.gh), indent=2))


if __name__ == "__main__":
    try:
        main()
    except (CheckError, OSError, ValueError, TypeError, AttributeError, subprocess.TimeoutExpired) as error:
        raise SystemExit(str(error)) from None
