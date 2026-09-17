#!/usr/bin/env python3
"""Build and compare the Linux amd64 compiler in two isolated CI directories."""

import argparse
import base64
import hashlib
import json
import os
from pathlib import Path
import platform
import re
import selectors
import shutil
import signal
import stat
import subprocess
import time


GO_VERSION = "go1.26.8"
SETTINGS = {
    "GOENV": "off", "GOWORK": "off", "GOFLAGS": "", "GOTOOLCHAIN": "local",
    "GOOS": "linux", "GOARCH": "amd64", "GOAMD64": "v1", "CGO_ENABLED": "0",
    "GOEXPERIMENT": "", "GOFIPS140": "off", "GOCACHEPROG": "",
    "GOPROXY": "https://proxy.golang.org", "GOSUMDB": "sum.golang.org",
    "GOPRIVATE": "", "GONOPROXY": "", "GONOSUMDB": "", "GOINSECURE": "",
    "GOAUTH": "off", "GOVCS": "*:off", "GOMAXPROCS": "2", "GOMEMLIMIT": "1536MiB",
    "GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": "/dev/null",
    "GIT_CONFIG_COUNT": "0", "GIT_TERMINAL_PROMPT": "0",
    "LANG": "C", "LC_ALL": "C", "TZ": "UTC",
}
BUILD_FLAGS = ["build", "-mod=readonly", "-trimpath", "-buildvcs=true", "-p=2"]


class CheckError(Exception):
    pass


def canonical(value):
    return json.dumps(value, sort_keys=True, separators=(",", ":")).encode()


def digest(path):
    value = hashlib.sha256()
    with path.open("rb") as stream:
        for block in iter(lambda: stream.read(65536), b""):
            value.update(block)
    return value.hexdigest()


def inventory(root, *, skip_git=False, max_files=100000, max_bytes=2 << 30):
    """Hash regular files and executable bits; reject links and special files."""
    records = []
    total = 0
    entries = 0
    def failed_walk(_):
        raise CheckError("An input directory cannot be read")
    for directory, directories, files in os.walk(root, followlinks=False, onerror=failed_walk):
        directories.sort()
        if skip_git and Path(directory) == root and ".git" in directories:
            directories.remove(".git")
        for name in directories + sorted(files):
            entries += 1
            path = Path(directory) / name
            relative = path.relative_to(root).as_posix()
            if entries > 200000 or len(path.relative_to(root).parts) > 64:
                raise CheckError("The input directory inventory exceeds its limits")
            if skip_git and relative == ".git":
                continue
            info = path.lstat()
            if stat.S_ISDIR(info.st_mode):
                continue
            if not stat.S_ISREG(info.st_mode):
                raise CheckError("An input is not a regular file")
            total += info.st_size
            if len(records) >= max_files or total > max_bytes or info.st_size > 128 << 20:
                raise CheckError("The input inventory exceeds its limits")
            records.append([relative, digest(path), bool(info.st_mode & 0o111)])
    records.sort()
    return {"sha256": hashlib.sha256(canonical(records)).hexdigest(),
            "files": len(records), "bytes": total}


def environment(root, goroot):
    # Do not inherit credentials, Go settings, compiler flags, or user Git config.
    return dict(SETTINGS, PATH=f"{goroot / 'bin'}:/usr/bin:/bin", GOROOT=str(goroot),
                HOME=str(root / "home"), GOPATH=str(root / "gopath"),
                GOMODCACHE=str(root / "modules"), GOCACHE=str(root / "cache"),
                TMPDIR=str(root / "tmp"))


def command(args, cwd, env, *, timeout=180, limit=4 << 20):
    """Bound command time and output, including inherited child output pipes."""
    process = subprocess.Popen(args, cwd=cwd, env=env, stdin=subprocess.DEVNULL,
                               stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                               start_new_session=True)
    output = bytearray()
    deadline = time.monotonic() + timeout
    complete = False
    try:
        with selectors.DefaultSelector() as selector:
            selector.register(process.stdout, selectors.EVENT_READ)
            while True:
                remaining = deadline - time.monotonic()
                if remaining <= 0:
                    raise CheckError("A build command exceeded its time limit")
                if selector.select(min(remaining, 0.25)):
                    block = os.read(process.stdout.fileno(), 65536)
                    if not block:
                        break
                    if len(output) + len(block) > limit:
                        raise CheckError("A build command exceeded its output limit")
                    output.extend(block)
                elif process.poll() is not None:
                    deadline = min(deadline, time.monotonic() + 2)
            if process.wait(timeout=max(0.01, deadline - time.monotonic())) != 0:
                # The command can include dependency or filesystem diagnostics.
                # Do not print that output as a public build record.
                raise CheckError("A build command failed; its private output is withheld")
        complete = True
        return bytes(output)
    finally:
        if not complete:
            try:
                os.killpg(process.pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
        if process.poll() is None:
            process.wait(timeout=5)
        process.stdout.close()


def module_records(data):
    decoder = json.JSONDecoder()
    text = data.decode()
    records = []
    while text.strip():
        value, end = decoder.raw_decode(text.lstrip())
        text = text.lstrip()[end:]
        if not isinstance(value, dict) or value.get("Error") or value.get("Replace"):
            raise CheckError("The module download contains an error or replacement")
        record = {key: value.get(key) for key in ["Path", "Version", "Sum", "GoModSum"]}
        if not all(isinstance(v, str) and v for v in record.values()):
            raise CheckError("A module download has no complete checksum record")
        if any(not re.fullmatch(r"h1:[A-Za-z0-9+/]{43}=", record[k]) for k in ["Sum", "GoModSum"]):
            raise CheckError("A module download checksum is invalid")
        if any(base64.b64encode(base64.b64decode(record[k][3:])).decode() != record[k][3:]
               for k in ["Sum", "GoModSum"]):
            raise CheckError("A module download checksum is not canonical")
        records.append(record)
    records.sort(key=lambda r: (r["Path"], r["Version"]))
    if not records or len({r["Path"] for r in records}) != len(records):
        raise CheckError("The module inventory is empty or has repeated paths")
    return records


def clean_source(git, source, revision, env):
    if command([git, "rev-parse", "HEAD"], source, env).decode().strip() != revision:
        raise CheckError("The source HEAD does not match the requested revision")
    if command([git, "status", "--porcelain=v1", "--untracked-files=all", "--ignored"], source, env):
        raise CheckError("The compiler source contains changed or extra files")


def build(source, revision, work, output, go):
    if not re.fullmatch(r"[0-9a-f]{40}", revision):
        raise CheckError("The source revision must be a full Git commit ID")
    source, work, output, go = (p.resolve() for p in [source, work, output, go])
    if source == work or source in work.parents or source == output or source in output.parents:
        raise CheckError("Build directories must be outside the source tree")
    if work == output or work in output.parents or output in work.parents:
        raise CheckError("Build and result directories must be separate")
    if any(p.exists() for p in [work, output]):
        raise CheckError("Build and result directories must not exist")
    goroot = go.parent.parent
    git = shutil.which("git", path="/usr/bin:/bin")
    if not git or not go.is_file():
        raise CheckError("The selected Go or Git tool is missing")
    work.mkdir(parents=True, mode=0o700)
    control = work / "control"
    for name in ["home", "tmp"]:
        (control / name).mkdir(parents=True, mode=0o700)
    env = environment(control, goroot)
    if command([str(go), "version"], source, env).decode().strip() != f"go version {GO_VERSION} linux/amd64":
        raise CheckError("The selected Go toolchain does not match the build policy")
    clean_source(git, source, revision, env)
    source_identity = inventory(source, skip_git=True, max_files=20000, max_bytes=512 << 20)
    toolchain = inventory(goroot)
    git_digest = digest(Path(git))
    results = []
    for name in ["first", "second-with-a-different-path"]:
        root = work / name
        for directory in ["source", "home", "tmp", "modules", "cache", "gopath"]:
            (root / directory).mkdir(parents=True, mode=0o700)
        checkout = root / "source"
        settings = environment(root, goroot)
        command([git, "init", "-q"], checkout, settings)
        command([git, "-c", "protocol.file.allow=always", "fetch", "-q", "--depth=1", str(source), revision], checkout, settings)
        command([git, "-c", "core.autocrlf=false", "-c", "core.hooksPath=/dev/null", "checkout", "-q", "--detach", "FETCH_HEAD"], checkout, settings)
        clean_source(git, checkout, revision, settings)
        if inventory(checkout, skip_git=True) != source_identity:
            raise CheckError("The copied compiler source does not match")
        command([str(go), "telemetry", "off"], checkout, settings)
        if command([str(go), "env", "GOTELEMETRY"], checkout, settings).strip() != b"off":
            raise CheckError("Go telemetry is not disabled for the build")
        module = json.loads(command([str(go), "mod", "edit", "-json"], checkout, settings))
        if module.get("Replace") or module.get("Go") != GO_VERSION.removeprefix("go") or module.get("Toolchain", "") not in ["", GO_VERSION]:
            raise CheckError("The compiler module does not match the build policy")
        modules = module_records(command([str(go), "mod", "download", "-json"], checkout, settings, timeout=240))
        settings["GOPROXY"] = "off"
        command([str(go), "mod", "verify"], checkout, settings)
        binary = root / "stego"
        command([str(go), *BUILD_FLAGS, "-o", str(binary), "./cmd/stego"], checkout, settings, timeout=420)
        command([str(go), "mod", "verify"], checkout, settings)
        version = json.loads(command([str(binary), "version", "--json"], checkout, settings, timeout=15))
        expected = {"go_version": GO_VERSION, "goos": "linux", "goarch": "amd64",
                    "revision": revision, "vcs": "git", "source_state": "clean"}
        if not isinstance(version, dict) or not isinstance(version.get("build"), dict) or any(version["build"].get(k) != v for k, v in expected.items()):
            raise CheckError("The compiler build record does not match the selected source and toolchain")
        clean_source(git, checkout, revision, settings)
        if inventory(checkout, skip_git=True) != source_identity:
            raise CheckError("The compiler source changed during the build")
        results.append({"binary": binary, "sha256": digest(binary), "size": binary.stat().st_size,
                        "modules": modules, "version": version})
    first, second = results
    if any(first[k] != second[k] for k in ["sha256", "size", "modules", "version"]):
        raise CheckError("The isolated compiler builds do not match")
    if inventory(goroot) != toolchain or digest(Path(git)) != git_digest:
        raise CheckError("The build tools changed during the check")
    clean_source(git, source, revision, env)
    if inventory(source, skip_git=True) != source_identity:
        raise CheckError("The original source changed during the check")
    output.mkdir(mode=0o700)
    artifact = output / "stego-linux-amd64"
    shutil.copyfile(first["binary"], artifact)
    artifact.chmod(0o755)
    if digest(artifact) != first["sha256"]:
        raise CheckError("The saved compiler artifact does not match")
    report = {"format": 1, "source_revision": revision, "source": source_identity,
              "go_version": GO_VERSION, "toolchain": toolchain, "git_sha256": git_digest,
              "build_flags": BUILD_FLAGS + ["-o", "<artifact>", "./cmd/stego"],
              "environment": SETTINGS, "isolated_source_trees_and_caches": 2,
              "build_module_proxy": "off", "telemetry_mode": "off", "modules": first["modules"],
              "version": first["version"], "artifact": {"name": artifact.name,
              "sha256": first["sha256"], "size": first["size"]}}
    (output / "build.json").write_text(json.dumps(report, indent=2, sort_keys=True) + "\n")
    (output / "SHA256SUMS").write_text("".join(f"{digest(output / name)}  {name}\n" for name in [artifact.name, "build.json"]))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--source", required=True, type=Path)
    parser.add_argument("--revision", required=True)
    parser.add_argument("--work", required=True, type=Path)
    parser.add_argument("--output", required=True, type=Path)
    parser.add_argument("--go", required=True, type=Path)
    args = parser.parse_args()
    if os.environ.get("CI") != "true" or platform.system() != "Linux" or platform.machine() != "x86_64":
        raise CheckError("This bounded build check requires Linux amd64 CI")
    def interrupted(_number, _frame):
        raise CheckError("The compiler build check was interrupted")
    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGINT, interrupted)
    os.umask(0o077)
    build(args.source, args.revision, args.work, args.output, args.go)
    print("Both isolated compiler builds match. The artifact and checksum record are saved.")


if __name__ == "__main__":
    try:
        main()
    except (CheckError, OSError, ValueError, subprocess.TimeoutExpired) as error:
        raise SystemExit(str(error)) from None
