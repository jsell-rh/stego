#!/usr/bin/env python3
"""Authenticate application records against an explicit consumer policy."""

import argparse
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import re
import shutil
import signal
import subprocess
import tempfile

spec = importlib.util.spec_from_file_location("compiler_verification", Path(__file__).with_name("verify-compiler-artifact.py"))
verification = importlib.util.module_from_spec(spec)
spec.loader.exec_module(verification)
CheckError = verification.CheckError
FIELDS = {"format", "repository", "workflow", "reference", "revision", "compiler_revision",
          "compiler_sha256", "application_revision", "module", "target", "entrypoint", "trust_store_sha256"}


def decode(data):
    result = json.loads(data, object_pairs_hook=verification.unique_object)
    if not isinstance(result, dict):
        raise CheckError("A record must be a JSON object")
    return result


def policy_bytes(path):
    data = verification.regular_bytes(path, 16 << 10)
    policy = decode(data)
    if set(policy) != FIELDS or type(policy["format"]) is not int or policy["format"] != 1:
        raise CheckError("The consumer policy fields or format differ")
    patterns = {
        "repository": r"[A-Za-z0-9_-]+/[A-Za-z0-9_.-]+",
        "workflow": r"\.github/workflows/[A-Za-z0-9_-]+\.ya?ml",
        "reference": r"refs/heads/[A-Za-z0-9_./-]+",
        "revision": r"[0-9a-f]{40}", "compiler_revision": r"[0-9a-f]{40}",
        "application_revision": r"[0-9a-f]{40}", "compiler_sha256": r"[0-9a-f]{64}",
        "trust_store_sha256": r"[0-9a-f]{64}", "entrypoint": r"[a-z][a-z0-9_-]{0,62}",
        "module": r"\.|[A-Za-z0-9_-][A-Za-z0-9_./-]*",
        "target": r"[A-Za-z0-9_-][A-Za-z0-9_./-]*",
    }
    for name, pattern in patterns.items():
        value = policy[name]
        if not isinstance(value, str) or len(value) > 256 or not re.fullmatch(pattern, value):
            raise CheckError("The consumer policy has an invalid " + name)
        if name in {"module", "target", "reference"} and value != ".":
            if any(part in {"", ".", ".."} for part in value.split("/")) or value.endswith(".lock"):
                raise CheckError("The consumer policy path or reference is invalid")
    if policy["entrypoint"] == "etc":
        raise CheckError("The consumer policy entry point is reserved")
    return data, policy


def check_binding(build, image, build_bytes, policy):
    for record in [build, image]:
        if type(record.get("format")) is not int or record["format"] != 1:
            raise CheckError("The signed record format is not supported")
    for key, selected in [("source_revision", "application_revision"), ("module", "module"), ("target", "target")]:
        if build.get(key) != policy[selected]:
            raise CheckError("The signed application selection differs: " + key)
    for record, identity, artifact in [(build, "build_compiler", "build_compiler_artifact"),
                                       (image, "packer_compiler", "packer_compiler_artifact")]:
        compiler = record.get(identity)
        expected = {"revision": policy["compiler_revision"], "source_state": "clean",
                    "vcs": "git", "goos": "linux", "goarch": "amd64"}
        if not isinstance(compiler, dict) or any(compiler.get(k) != v for k, v in expected.items()):
            raise CheckError("The signed compiler source differs")
        value = record.get(artifact)
        if not isinstance(value, dict) or value.get("sha256") != policy["compiler_sha256"]:
            raise CheckError("The signed compiler bytes differ")
    if build.get("build_compiler_artifact") != image.get("packer_compiler_artifact"):
        raise CheckError("The build and image compilers differ")
    if image.get("build_record_sha256") != hashlib.sha256(build_bytes).hexdigest():
        raise CheckError("The signed image does not bind the signed build record")
    if image.get("application") != build.get("artifact") or not isinstance(build.get("artifact"), dict):
        raise CheckError("The signed executable records differ")
    if image.get("entrypoint") != policy["entrypoint"]:
        raise CheckError("The signed entry point differs")
    trust = image.get("trust_store")
    if not isinstance(trust, dict) or trust.get("sha256") != policy["trust_store_sha256"]:
        raise CheckError("The signed CA selection differs")


def verify(inputs, bundle, policy_path, output, gh):
    policy_data, policy = policy_bytes(policy_path)
    if inputs.is_symlink() or not inputs.is_dir():
        raise CheckError("The input directory must be a real directory")
    if not gh.is_absolute() or not gh.is_file() or not os.access(gh, os.X_OK):
        raise CheckError("Select an installed GitHub CLI by its absolute path")
    output = output.absolute()
    if output.exists() or output.is_symlink() or not output.parent.is_dir():
        raise CheckError("The result directory must be new and its parent must exist")
    with tempfile.TemporaryDirectory(prefix=".stego-records-", dir=output.parent) as temporary:
        stage = Path(temporary)
        for name, limit in [("build.json", 16 << 20), ("image.json", 64 << 10)]:
            (stage / name).write_bytes(verification.regular_bytes(inputs / name, limit))
        (stage / "policy.json").write_bytes(policy_data)
        saved_bundle = stage / "provenance.jsonl"
        saved_bundle.write_bytes(verification.regular_bytes(bundle, 4 << 20))
        environment = verification.verifier_environment()
        for name in ["build.json", "image.json"]:
            verification.authenticate_subject(gh, stage / name, saved_bundle, policy["revision"],
                                              environment, policy["repository"], policy["workflow"], policy["reference"])
        build_data = (stage / "build.json").read_bytes()
        image_data = (stage / "image.json").read_bytes()
        check_binding(decode(build_data), decode(image_data), build_data, policy)
        result = {"format": 1, "policy": policy, "policy_sha256": hashlib.sha256(policy_data).hexdigest(),
                  "build_record_sha256": hashlib.sha256(build_data).hexdigest(),
                  "image_record_sha256": hashlib.sha256(image_data).hexdigest(),
                  "bundle_sha256": verification.control.digest(saved_bundle),
                  "records_authenticated": True, "image_contents_checked": False,
                  "registry_publication_checked": False}
        # The caller must next pass this authenticated digest to stego image verify.
        # Record signatures do not replace image content or deployment checks.
        output.mkdir(mode=0o700)
        try:
            for name in ["build.json", "image.json", "policy.json", "provenance.jsonl"]:
                with (output / name).open("xb") as stream:
                    stream.write((stage / name).read_bytes())
                (output / name).chmod(0o600)
            with (output / "verified.json").open("x") as stream:
                stream.write(json.dumps(result, indent=2, sort_keys=True) + "\n")
            (output / "verified.json").chmod(0o600)
        except BaseException:
            shutil.rmtree(output)
            raise
    return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ["inputs", "bundle", "policy", "output", "gh"]:
        parser.add_argument("--" + name, required=True, type=Path)
    args = parser.parse_args()
    def interrupted(_number, _frame):
        raise CheckError("Application record verification was interrupted")
    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGINT, interrupted)
    os.umask(0o077)
    print(json.dumps(verify(args.inputs, args.bundle, args.policy, args.output, args.gh), indent=2))


if __name__ == "__main__":
    try:
        main()
    except (CheckError, OSError, ValueError, TypeError, AttributeError, subprocess.TimeoutExpired) as error:
        raise SystemExit(str(error)) from None
