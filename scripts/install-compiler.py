#!/usr/bin/env python3
"""Install an authenticated Linux amd64 compiler from an explicit source commit."""

import argparse
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import platform
import re
import signal
import subprocess
import sys
import tempfile


spec = importlib.util.spec_from_file_location(
    "verification", Path(__file__).with_name("verify-compiler-artifact.py"))
verification = importlib.util.module_from_spec(spec)
spec.loader.exec_module(verification)
CheckError = verification.CheckError
ASSETS = {verification.BINARY: 64 << 20, "build.json": 1 << 20,
          "SHA256SUMS": 1024, "provenance.jsonl": 4 << 20}
API = "repos/" + verification.REPOSITORY + "/releases/"


def api(gh, path, directory, *, limit, binary=False):
    accept = "application/octet-stream" if binary else "application/vnd.github+json"
    return verification.control.command(
        [str(gh), "api", "--hostname", "github.com", "--method", "GET",
         "-H", "Accept: " + accept, "-H", "X-GitHub-Api-Version: 2026-03-10", path],
        directory, verification.verifier_environment(), timeout=90, limit=limit)


def release_assets(record, revision):
    if not isinstance(record, dict):
        raise CheckError("The release record is invalid")
    if (record.get("tag_name") != "compiler-" + revision
            or record.get("target_commitish") != revision
            or record.get("draft") is not False
            or record.get("immutable") is not True
            or record.get("prerelease") is not False):
        raise CheckError("Select a published immutable release for the exact source commit")
    if type(record.get("id")) is not int or record["id"] <= 0:
        raise CheckError("The release identity is invalid")
    assets = record.get("assets")
    if not isinstance(assets, list) or len(assets) != len(ASSETS):
        raise CheckError("The release must contain exactly the four compiler inputs")
    found = {}
    ids = set()
    for asset in assets:
        if not isinstance(asset, dict):
            raise CheckError("The release asset is invalid")
        name, identity, size = asset.get("name"), asset.get("id"), asset.get("size")
        if not isinstance(name, str) or name not in ASSETS or name in found:
            raise CheckError("The release contains an unknown or repeated input")
        if (type(identity) is not int or identity <= 0 or identity in ids
                or type(size) is not int or not 0 < size <= ASSETS[name]
                or asset.get("state") != "uploaded"
                or not isinstance(asset.get("digest"), str)
                or not re.fullmatch(r"sha256:[0-9a-f]{64}", asset["digest"])):
            raise CheckError("A release asset identity, size, state, or digest is invalid")
        found[name] = asset
        ids.add(identity)
    return found


def install(revision, output, gh, package=None):
    if not re.fullmatch(r"[0-9a-f]{40}", revision):
        raise CheckError("Select a full lowercase source commit ID")
    if sys.platform != "linux" or platform.machine().lower() not in ["x86_64", "amd64"]:
        raise CheckError("This compiler package requires Linux amd64")
    if not gh.is_absolute() or not gh.is_file() or not os.access(gh, os.X_OK):
        raise CheckError("Select an installed GitHub CLI by its absolute path")
    output = output.absolute()
    if output.exists() or output.is_symlink() or not output.parent.is_dir():
        raise CheckError("The result directory must be new and its parent must exist")
    if package is not None:
        return verification.verify(package, package / "provenance.jsonl", revision, output, gh)
    with tempfile.TemporaryDirectory(prefix=".stego-download-", dir=output.parent) as temporary:
        stage = Path(temporary)
        raw = api(gh, API + "tags/compiler-" + revision, stage, limit=1 << 20)
        record = json.loads(raw, object_pairs_hook=verification.unique_object)
        assets = release_assets(record, revision)
        # Use fixed repository API paths and integer IDs. Never use an input URL
        # or an input file name as a download destination.
        for name in ASSETS:
            asset = assets[name]
            data = api(gh, API + "assets/" + str(asset["id"]), stage,
                       limit=asset["size"], binary=True)
            if (len(data) != asset["size"]
                    or "sha256:" + hashlib.sha256(data).hexdigest() != asset["digest"]):
                raise CheckError("The downloaded input does not match the selected asset")
            (stage / name).write_bytes(data)
        # Release metadata and checksums do not authenticate themselves. Both
        # signatures must pass the common fixed source and signer policy.
        return verification.verify(stage, stage / "provenance.jsonl", revision, output, gh)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--revision", required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--gh", type=Path, required=True)
    source = parser.add_mutually_exclusive_group(required=True)
    source.add_argument("--release", action="store_true")
    source.add_argument("--package", type=Path)
    args = parser.parse_args()
    def interrupted(_number, _frame):
        raise CheckError("Compiler installation was interrupted")
    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGINT, interrupted)
    os.umask(0o077)
    print(json.dumps(install(args.revision, args.output, args.gh, args.package), indent=2))


if __name__ == "__main__":
    try:
        main()
    except (CheckError, OSError, ValueError, TypeError, AttributeError, subprocess.TimeoutExpired) as error:
        raise SystemExit(str(error)) from None
