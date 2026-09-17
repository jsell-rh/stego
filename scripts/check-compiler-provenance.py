#!/usr/bin/env python3
"""Check real compiler signatures and rejection behavior in main-branch CI."""

import argparse
import importlib.util
import json
import os
from pathlib import Path
import shutil
import signal
import tempfile

spec = importlib.util.spec_from_file_location("verification", Path(__file__).with_name("verify-compiler-artifact.py"))
verification = importlib.util.module_from_spec(spec)
spec.loader.exec_module(verification)


def check(inputs, bundle, revision, output, gh):
    if os.environ.get("CI") != "true":
        raise verification.CheckError("The signed artifact gate requires CI")
    # The verified compiler is retained for consumers, but is not executed here.
    record = verification.verify(inputs, bundle, revision, output, gh)
    rejected = []
    with tempfile.TemporaryDirectory(prefix=".stego-provenance-test-", dir=output.parent) as temporary:
        root = Path(temporary)
        def deny(name, source, selected_revision=revision, selected_bundle=bundle):
            destination = root / (name + "-result")
            try:
                verification.verify(source, selected_bundle, selected_revision, destination, gh)
            except verification.CheckError:
                if destination.exists():
                    raise verification.CheckError("A rejected artifact left a result directory")
                rejected.append(name)
                return
            raise verification.CheckError("An invalid artifact passed verification")
        other_revision = ("0" if revision[0] != "0" else "1") + revision[1:]
        deny("different-source-commit", inputs, other_revision)
        changed = root / "changed"
        shutil.copytree(inputs, changed)
        binary = changed / verification.BINARY
        with binary.open("ab") as stream:
            stream.write(b"changed bytes")
        build = json.loads((changed / "build.json").read_text())
        build["artifact"]["size"] = binary.stat().st_size
        build["artifact"]["sha256"] = verification.control.digest(binary)
        (changed / "build.json").write_text(json.dumps(build))
        (changed / "SHA256SUMS").write_text("".join(
            f"{verification.control.digest(changed / name)}  {name}\n"
            for name in [verification.BINARY, "build.json"]))
        deny("changed-compiler-with-matching-checksums", changed)
        shutil.rmtree(changed)
        shutil.copytree(inputs, changed)
        build = json.loads((changed / "build.json").read_text())
        build["toolchain"]["sha256"] = "0" * 64
        (changed / "build.json").write_text(json.dumps(build))
        (changed / "SHA256SUMS").write_text("".join(
            f"{verification.control.digest(changed / name)}  {name}\n"
            for name in [verification.BINARY, "build.json"]))
        deny("changed-build-record", changed)
        corrupt_bundle = root / "corrupt.jsonl"
        corrupt_bundle.write_text('{"not":"an attestation"}\n')
        deny("invalid-signature-bundle", inputs, selected_bundle=corrupt_bundle)
        if verification.verify(inputs, bundle, revision, root / "valid-after-rejections", gh) != record:
            raise verification.CheckError("The valid artifact changed after the rejection checks")
    result = {"source_revision": revision, "authenticated": record["artifact"],
              "rejected": rejected, "valid_after_rejections": True,
              "downloaded_compiler_executed": False}
    (output / "rejection-checks.json").write_text(json.dumps(result, indent=2) + "\n")
    print(json.dumps(result, indent=2))


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ["inputs", "bundle", "output", "gh"]:
        parser.add_argument("--" + name, type=Path, required=True)
    parser.add_argument("--revision", required=True)
    args = parser.parse_args()
    def interrupted(_number, _frame):
        raise verification.CheckError("The signed artifact gate was interrupted")
    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGINT, interrupted)
    os.umask(0o077)
    check(args.inputs, args.bundle, args.revision, args.output, args.gh)
