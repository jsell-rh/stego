#!/usr/bin/env python3
"""Stage signed application images, then publish them from checked source."""

import argparse
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import re
import shutil
import signal
import tempfile


def module(name, file):
    spec = importlib.util.spec_from_file_location(name, Path(__file__).with_name(file))
    result = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(result)
    return result


records = module("records", "verify-application-records.py")
declaration = module("declaration", "application-build-matrix.py")
CheckError = records.CheckError
SELECTION = {"name", "module", "target", "entrypoint"}
DIGESTS = {"build_record_sha256", "image_record_sha256", "manifest_sha256"}


def save(path, value):
    with path.open("x") as stream:
        os.fchmod(stream.fileno(), 0o600)
        stream.write(json.dumps(value, indent=2, sort_keys=True) + "\n")


def digest(value):
    return hashlib.sha256(value).hexdigest()


def new_output(path):
    path = path.absolute()
    if path.exists() or path.is_symlink() or not path.parent.is_dir():
        raise CheckError("The result directory must be new and its parent must exist")
    return path


def stage(declared, policy_file, artifacts, run, attempt, output, gh):
    images = declaration.matrix(declared)["include"]
    policy = records.decode(records.verification.regular_bytes(policy_file, 16 << 10))
    if (set(policy) != records.REUSABLE_FIELDS - {"module", "target", "entrypoint"}
            or type(policy.get("format")) is not int or policy["format"] != 2):
        raise CheckError("The image set requires an explicit reusable signer policy")
    if any(not re.fullmatch(r"[1-9][0-9]{0,19}", str(value)) for value in [run, attempt]):
        raise CheckError("The workflow run and attempt must be positive integers")
    if artifacts.is_symlink() or not artifacts.is_dir():
        raise CheckError("Select a real artifact directory")
    output = new_output(output)
    with tempfile.TemporaryDirectory(prefix=".stego-images-", dir=output.parent) as directory:
        root = Path(directory)
        result = {"format": 1, "images": []}
        for image in images:
            selected = dict(policy, **{key: image[key] for key in ["module", "target", "entrypoint"]})
            policy_path = root / (image["name"] + "-policy.json")
            save(policy_path, selected)
            suffix = image["name"] + "-" + str(run) + "-" + str(attempt)
            inputs = artifacts / ("consumer-image-" + suffix) / "image-evidence/first"
            bundle = artifacts / ("authenticated-records-" + suffix) / "provenance.jsonl"
            target = root / image["name"]
            verified = records.verify(inputs, bundle, policy_path, target, gh, inputs / "oci")
            signed = records.decode((target / "image.json").read_bytes())
            result["images"].append(dict(image, build_record_sha256=verified["build_record_sha256"],
                                         image_record_sha256=verified["image_record_sha256"],
                                         manifest_sha256=signed["manifest"]["sha256"]))
        save(root / "images.json", result)
        output.mkdir(mode=0o700)
        try:
            for image in images:
                (root / image["name"]).rename(output / image["name"])
            (root / "images.json").rename(output / "images.json")
        except BaseException:
            shutil.rmtree(output)
            raise
    return result


def load_set(root, expected, work):
    data = records.verification.regular_bytes(root / "images.json", 64 << 10)
    if not re.fullmatch(r"[0-9a-f]{64}", expected) or digest(data) != expected:
        raise CheckError("The image set digest differs from the trusted selection")
    selected = records.decode(data)
    if (set(selected) != {"format", "images"} or type(selected["format"]) is not int
            or selected["format"] != 1 or not isinstance(selected["images"], list)
            or not 1 <= len(selected["images"]) <= 64):
        raise CheckError("The image set fields or format differ")
    for image in selected["images"]:
        if not isinstance(image, dict) or set(image) != SELECTION | DIGESTS:
            raise CheckError("An image selection has unsupported fields")
        if any(not isinstance(image[key], str) or not re.fullmatch(r"[0-9a-f]{64}", image[key]) for key in DIGESTS):
            raise CheckError("An image selection has an invalid digest")
    # Use the same name, target, and entry point policy as the build workflow.
    saved = work / "declaration.json"
    save(saved, {"format": 1, "images": [{key: image[key] for key in SELECTION} for image in selected["images"]]})
    declaration.matrix(saved)
    return selected


def token_credentials(path, username, output):
    if not re.fullmatch(r"[A-Za-z0-9_.-]{1,128}", username):
        raise CheckError("The registry token username is invalid")
    # The operator selects this token file. Kubernetes projected volumes use
    # links. Resolve the selected path, then capture bounded regular bytes.
    data = records.verification.regular_bytes(path.resolve(strict=True), 8 << 10)
    if not re.fullmatch(rb"[A-Za-z0-9._~+/-]+=*", data):
        raise CheckError("The selected registry token is invalid")
    credentials = {"username": username, "password": data.decode("ascii")}
    with output.open("xb") as stream:
        os.fchmod(stream.fileno(), 0o600)
        stream.write(json.dumps(credentials, separators=(",", ":")).encode() + b"\n")


def registry_policy(path):
    if path is None:
        return {"token_origins": [], "blob_origins": []}
    value = records.decode(records.verification.regular_bytes(path, 16 << 10))
    if (set(value) != {"format", "token_origins", "blob_origins"}
            or type(value["format"]) is not int or value["format"] != 1):
        raise CheckError("The registry destination policy fields or format differ")
    for name in ["token_origins", "blob_origins"]:
        origins = value[name]
        if (not isinstance(origins, list) or len(origins) > 8
                or any(not isinstance(origin, str) or not 1 <= len(origin) <= 2048 for origin in origins)
                or len(origins) != len(set(origins))):
            raise CheckError("The registry destination policy has invalid origins")
    # The compiler validates HTTPS origin syntax, separation of blob and
    # credential origins, and the boundary of every actual request.
    return {key: value[key] for key in ["token_origins", "blob_origins"]}


def publish(images, expected, source, compiler, compiler_sha256, repository, ca, ca_sha256,
            output, credentials=None, token_file=None, username=None, policy_file=None):
    images, source, compiler, ca = (path.absolute() for path in [images, source, compiler, ca])
    if (credentials is None) == (token_file is None) or (token_file is None) != (username is None):
        raise CheckError("Select credentials or an explicit token file and username")
    if not isinstance(repository, str) or not repository or repository.endswith("/"):
        raise CheckError("Select a registry and repository prefix without a final slash")
    policy = registry_policy(policy_file)
    origins = []
    for field, flag in [("token_origins", "--token-origin"), ("blob_origins", "--blob-origin")]:
        for origin in policy[field]:
            origins.extend([flag, origin])
    output = new_output(output)
    output.mkdir(mode=0o700)
    # Failure leaves bounded diagnostic records. Only publication.json marks
    # a complete set. A failed set can have individual published images.
    with tempfile.TemporaryDirectory(prefix=".private-", dir=output) as directory:
        private = Path(directory)
        selected = load_set(images, expected, private)
        data = records.verification.regular_bytes(compiler, 64 << 20)
        if not re.fullmatch(r"[0-9a-f]{64}", compiler_sha256) or digest(data) != compiler_sha256:
            raise CheckError("The publishing compiler digest differs")
        executable = private / "stego"
        executable.write_bytes(data)
        executable.chmod(0o700)
        del data
        environment = {"PATH": "/usr/bin:/bin", "LANG": "C", "LC_ALL": "C",
                       "HOME": str(private), "TMPDIR": str(private), "GOMAXPROCS": "2", "GOMEMLIMIT": "512MiB"}
        def command(args):
            return records.verification.control.command([str(executable), *args], private, environment,
                                                        timeout=310, limit=1 << 20)
        for image in selected["images"]:
            target = images / image["name"]
            command(["build", "verify-source", "--record", str(target / "build.json"),
                     "--record-sha256", image["build_record_sha256"], "--source", str(source)])
            command(["image", "verify", "--record", str(target / "image.json"),
                     "--record-sha256", image["image_record_sha256"], "--build-record", str(target / "build.json"),
                     "--image", str(target / "oci"), "--work", str(output / (image["name"] + "-preflight"))])
        save(output / "source-check.json", {"format": 1, "image_set_sha256": expected,
                                           "compiler_sha256": compiler_sha256, "images": selected["images"]})
        if token_file is not None:
            credentials = private / "credentials.json"
            token_credentials(token_file, username, credentials)
        else:
            if credentials.lstat().st_mode & 0o077:
                raise CheckError("Registry credentials must be private")
            data = records.verification.regular_bytes(credentials, 16 << 10)
            credentials = private / "credentials.json"
            credentials.write_bytes(data)
            credentials.chmod(0o600)
        result = {"format": 1, "image_set_sha256": expected, "compiler_sha256": compiler_sha256, "images": {}}
        for image in selected["images"]:
            name = image["name"]
            target, work = images / name, output / name
            destination = repository + "/" + name
            command(["image", "publish", "--record", str(target / "image.json"),
                     "--record-sha256", image["image_record_sha256"], "--build-record", str(target / "build.json"),
                     "--image", str(target / "oci"), "--work", str(work), "--repository", destination,
                     "--registry-ca", str(ca), "--registry-ca-sha256", ca_sha256, "--credentials", str(credentials), *origins])
            receipt = records.decode(records.verification.regular_bytes(work / "registry.json", 64 << 10))
            reference = destination + "@sha256:" + image["manifest_sha256"]
            expected_receipt = {"format": 1, "operation": "publish", "repository": destination,
                                "reference": reference, "image_record_sha256": image["image_record_sha256"],
                                "registry_ca_sha256": ca_sha256, **policy,
                                "image_contents_verified": True}
            if receipt != expected_receipt:
                raise CheckError("The registry receipt differs from the selected image")
            result["images"][name] = reference
        save(output / "publication.json", result)
    return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    modes = parser.add_subparsers(dest="mode", required=True)
    staging = modes.add_parser("stage")
    for name in ["declaration", "policy", "artifacts", "output", "gh"]:
        staging.add_argument("--" + name, type=Path, required=True)
    for name in ["run", "attempt"]:
        staging.add_argument("--" + name, required=True)
    publishing = modes.add_parser("publish")
    for name in ["images", "source", "compiler", "registry-ca", "output"]:
        publishing.add_argument("--" + name, type=Path, required=True)
    for name in ["set-sha256", "compiler-sha256", "repository", "registry-ca-sha256"]:
        publishing.add_argument("--" + name, required=True)
    for name in ["credentials", "token-file", "registry-policy"]:
        publishing.add_argument("--" + name, type=Path)
    publishing.add_argument("--username")
    args = parser.parse_args()
    def interrupted(_number, _frame):
        raise CheckError("Application image delivery stopped or exceeded its time limit")
    for selected in [signal.SIGTERM, signal.SIGINT, signal.SIGALRM]:
        signal.signal(selected, interrupted)
    signal.alarm(900)
    os.umask(0o077)
    if args.mode == "stage":
        result = stage(args.declaration, args.policy, args.artifacts, args.run, args.attempt, args.output, args.gh)
    else:
        result = publish(args.images, args.set_sha256, args.source, args.compiler, args.compiler_sha256,
                         args.repository, args.registry_ca, args.registry_ca_sha256, args.output,
                         args.credentials, args.token_file, args.username, args.registry_policy)
    print(json.dumps(result, indent=2))


if __name__ == "__main__":
    try:
        main()
    except (CheckError, OSError, ValueError, TypeError, KeyError, RecursionError) as error:
        raise SystemExit(str(error)) from None
