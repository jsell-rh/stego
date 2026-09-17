#!/usr/bin/env python3
"""Check and extract the pinned compiler SDK without executing it."""

import argparse
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import shutil
import signal
import stat
import subprocess
import tarfile
import tempfile

spec = importlib.util.spec_from_file_location("control", Path(__file__).with_name("check-compiler-artifact.py"))
control = importlib.util.module_from_spec(spec)
spec.loader.exec_module(control)
CheckError = control.CheckError


def capture_archive(source, target):
    """Authenticate a bounded private snapshot before archive parsing."""
    expected = control.GO_RELEASE
    descriptor = os.open(source, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    digest = hashlib.sha256()
    with os.fdopen(descriptor, "rb") as incoming:
        info = os.fstat(incoming.fileno())
        if not stat.S_ISREG(info.st_mode) or info.st_size != expected["size"]:
            raise CheckError("The SDK archive has an invalid type or size")
        remaining = expected["size"]
        with target.open("xb") as outgoing:
            while remaining:
                block = incoming.read(min(65536, remaining))
                if not block:
                    raise CheckError("The SDK archive ended before its declared size")
                outgoing.write(block)
                digest.update(block)
                remaining -= len(block)
            if incoming.read(1):
                raise CheckError("The SDK archive exceeds its declared size")
    if digest.hexdigest() != expected["sha256"]:
        raise CheckError("The SDK archive does not match the pinned release checksum")


def extract_archive(snapshot, root):
    """Extract regular files only, within the pinned size and inventory limits."""
    seen = set()
    total = 0
    count = 0
    expected = control.GO_RELEASE["inventory"]
    with tarfile.open(snapshot, mode="r|gz") as archive:
        for member in archive:
            name = member.name
            parts = name.split("/")
            if (not name or len(name.encode()) > 4096 or len(parts) > 64
                    or parts[0] != "go" or any(p in ["", ".", ".."] for p in parts)
                    or "\\" in name or name in seen or len(seen) >= 30000):
                raise CheckError("The SDK archive has an invalid or repeated path")
            seen.add(name)
            target = root.joinpath(*parts[1:])
            if member.isdir():
                target.mkdir(parents=True, exist_ok=True, mode=0o755)
                continue
            if (len(parts) == 1 or not member.isfile() or member.sparse is not None
                    or member.mode & 0o7000 or not 0 <= member.size <= 128 << 20):
                raise CheckError("The SDK archive contains an unsupported file")
            count += 1
            total += member.size
            if count > expected["files"] or total > expected["bytes"]:
                raise CheckError("The SDK archive exceeds its extraction limits")
            target.parent.mkdir(parents=True, exist_ok=True, mode=0o755)
            with archive.extractfile(member) as incoming, target.open("xb") as outgoing:
                remaining = member.size
                while remaining:
                    block = incoming.read(min(65536, remaining))
                    if not block:
                        raise CheckError("An SDK file ended before its declared size")
                    outgoing.write(block)
                    remaining -= len(block)
            target.chmod(0o755 if member.mode & 0o111 else 0o644)
    control.require_toolchain(root / "bin" / "go")


def prepare(source, output):
    # mkdir is exclusive. An existing directory, file, or link is never replaced.
    output.mkdir(mode=0o700)
    try:
        with tempfile.TemporaryDirectory(prefix="capture-", dir=output) as temporary:
            scratch = Path(temporary)
            if source is None:
                source = scratch / "download"
                # Use the system TLS roots. Do not inherit proxy, loader, curl,
                # credential, or user configuration from the caller.
                env = {"PATH": "/usr/bin:/bin", "HOME": str(scratch), "LANG": "C", "LC_ALL": "C"}
                control.command([
                    "/usr/bin/curl", "-q", "--fail", "--silent", "--show-error",
                    "--proto", "=https", "--tlsv1.2", "--connect-timeout", "15",
                    "--max-time", "180", "--max-filesize", str(control.GO_RELEASE["size"]),
                    "--output", str(source), "--url", control.GO_RELEASE["url"],
                ], scratch, env, timeout=185, limit=65536)
            snapshot = scratch / "archive.tar.gz"
            capture_archive(source, snapshot)
            root = output / "go"
            root.mkdir(mode=0o755)
            extract_archive(snapshot, root)
        (output / "toolchain.json").write_text(json.dumps(control.GO_RELEASE, indent=2, sort_keys=True) + "\n")
    except BaseException:
        shutil.rmtree(output)
        raise


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    inputs = parser.add_mutually_exclusive_group(required=True)
    inputs.add_argument("--archive", type=Path)
    inputs.add_argument("--download", action="store_true")
    parser.add_argument("--output", required=True, type=Path)
    args = parser.parse_args()
    def interrupted(_number, _frame):
        raise CheckError("SDK preparation was interrupted")
    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGINT, interrupted)
    os.umask(0o077)
    prepare(args.archive.absolute() if args.archive else None, args.output.absolute())
    print("The pinned SDK archive and extracted file inventory match. No SDK program was executed.")


if __name__ == "__main__":
    try:
        main()
    except (CheckError, OSError, ValueError, tarfile.TarError, subprocess.TimeoutExpired) as error:
        raise SystemExit(str(error)) from None
