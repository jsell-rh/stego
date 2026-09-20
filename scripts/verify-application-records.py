#!/usr/bin/env python3
"""Authenticate application records against an explicit consumer policy."""

import argparse
from contextlib import contextmanager
import hashlib
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

spec = importlib.util.spec_from_file_location("compiler_verification", Path(__file__).with_name("verify-compiler-artifact.py"))
verification = importlib.util.module_from_spec(spec)
spec.loader.exec_module(verification)
CheckError = verification.CheckError
FIELDS = {"format", "repository", "workflow", "reference", "revision", "compiler_revision",
          "compiler_sha256", "application_revision", "module", "target", "entrypoint", "trust_store_sha256"}
REUSABLE_FIELDS = (FIELDS - {"workflow"}) | {"signer_repository", "signer_workflow", "signer_revision"}


def decode(data):
    result = json.loads(data, object_pairs_hook=verification.unique_object)
    if not isinstance(result, dict):
        raise CheckError("A record must be a JSON object")
    return result


def policy_bytes(path):
    data = verification.regular_bytes(path, 16 << 10)
    policy = decode(data)
    if type(policy.get("format")) is not int or policy["format"] not in {1, 2}:
        raise CheckError("The consumer policy fields or format differ")
    if set(policy) != (FIELDS if policy["format"] == 1 else REUSABLE_FIELDS):
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
    if policy["format"] == 2:
        patterns["signer_repository"] = patterns["repository"]
        patterns["signer_workflow"] = patterns.pop("workflow")
        patterns["signer_revision"] = patterns["revision"]
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


@contextmanager
def image_directory(path, parent=None):
    descriptor = os.open(path, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW | os.O_NONBLOCK,
                         dir_fd=parent)
    try:
        yield descriptor
    finally:
        os.close(descriptor)


def directory_entries(descriptor, expected):
    # Stop on the first unexpected entry. Do not collect an unbounded directory.
    seen = set()
    with os.scandir(descriptor) as entries:
        for entry in entries:
            if entry.name not in expected or entry.name in seen:
                raise CheckError("The image directory entries differ")
            seen.add(entry.name)
    if seen != expected:
        raise CheckError("The image directory entries differ")


def capture_image_file(parent, name, output, limit, identity=None):
    descriptor = os.open(name, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK, dir_fd=parent)
    with os.fdopen(descriptor, "rb") as source:
        info = os.fstat(source.fileno())
        if not stat.S_ISREG(info.st_mode) or not 0 < info.st_size <= limit:
            raise CheckError("An image input is not a bounded regular file")
        if identity is not None and info.st_size != identity["size"]:
            raise CheckError("An image blob size differs")
        digest, count = hashlib.sha256(), 0
        with output.open("xb") as target:
            os.fchmod(target.fileno(), 0o600)
            while True:
                block = source.read(min(64 << 10, limit + 1 - count))
                if not block:
                    break
                count += len(block)
                if count > limit:
                    raise CheckError("An image input exceeded its size limit")
                digest.update(block)
                target.write(block)
        if count != info.st_size:
            raise CheckError("An image input changed size")
        if identity is not None and digest.hexdigest() != identity["sha256"]:
            raise CheckError("An image blob digest differs")


def capture_image(source, output, record):
    # These limits match the static Go image profile. This copy does not parse
    # or accept OCI semantics. The compiler must still verify the full image.
    blobs = {}
    for role, limit in [("manifest", 64 << 10), ("config", 64 << 10),
                        ("layer", (128 << 20) + (1 << 20) + (64 << 10))]:
        identity = record.get(role)
        if (not isinstance(identity, dict) or set(identity) != {"sha256", "size"}
                or not isinstance(identity["sha256"], str)
                or not re.fullmatch(r"[0-9a-f]{64}", identity["sha256"])
                or type(identity["size"]) is not int or not 0 < identity["size"] <= limit):
            raise CheckError("An image blob identity is invalid")
        if identity["sha256"] in blobs:
            raise CheckError("The image blob roles overlap")
        blobs[identity["sha256"]] = identity
    output.mkdir(mode=0o700)
    (output / "blobs").mkdir(mode=0o700)
    destination = output / "blobs/sha256"
    destination.mkdir(mode=0o700)
    # Use directory descriptors for every child. A replaced directory or link
    # cannot redirect the capture to another directory.
    with image_directory(source) as root:
        directory_entries(root, {"oci-layout", "index.json", "blobs"})
        with image_directory("blobs", root) as blob_root:
            directory_entries(blob_root, {"sha256"})
            with image_directory("sha256", blob_root) as hashes:
                directory_entries(hashes, set(blobs))
                for name, identity in blobs.items():
                    capture_image_file(hashes, name, destination / name, identity["size"], identity)
        for name, limit in [("oci-layout", 1024), ("index.json", 64 << 10)]:
            capture_image_file(root, name, output / name, limit)


def verify(inputs, bundle, policy_path, output, gh, image=None):
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
            if policy["format"] == 1:
                verification.authenticate_subject(gh, stage / name, saved_bundle, policy["revision"],
                                                  environment, policy["repository"], policy["workflow"], policy["reference"])
            else:
                verification.authenticate_reusable_subject(gh, stage / name, saved_bundle, environment,
                                                           policy["repository"], policy["reference"], policy["revision"],
                                                           policy["signer_repository"], policy["signer_workflow"], policy["signer_revision"])
        build_data = (stage / "build.json").read_bytes()
        image_data = (stage / "image.json").read_bytes()
        image_record = decode(image_data)
        check_binding(decode(build_data), image_record, build_data, policy)
        result = {"format": 1, "policy": policy, "policy_sha256": hashlib.sha256(policy_data).hexdigest(),
                  "build_record_sha256": hashlib.sha256(build_data).hexdigest(),
                  "image_record_sha256": hashlib.sha256(image_data).hexdigest(),
                  "bundle_sha256": verification.control.digest(saved_bundle),
                  "records_authenticated": True, "image_contents_checked": False,
                  "registry_publication_checked": False}
        if image is not None:
            capture_image(image, stage / "oci", image_record)
            result["image_blob_bytes_checked"] = True
        # The caller must next pass this authenticated digest to stego image verify.
        # Record signatures do not replace image content or deployment checks.
        output.mkdir(mode=0o700)
        try:
            if image is not None:
                (stage / "oci").rename(output / "oci")
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
    parser.add_argument("--image", type=Path, help="Capture the selected OCI files after record authentication")
    args = parser.parse_args()
    def interrupted(_number, _frame):
        raise CheckError("Application record verification was interrupted")
    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGINT, interrupted)
    os.umask(0o077)
    print(json.dumps(verify(args.inputs, args.bundle, args.policy, args.output, args.gh, args.image), indent=2))


if __name__ == "__main__":
    try:
        main()
    except (CheckError, OSError, ValueError, TypeError, AttributeError, subprocess.TimeoutExpired) as error:
        raise SystemExit(str(error)) from None
