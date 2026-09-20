#!/usr/bin/env python3
"""Check real application signatures and rejection behavior in CI."""

import argparse
import importlib.util
import json
import os
from pathlib import Path
import shutil
import signal
import tempfile

spec = importlib.util.spec_from_file_location("records", Path(__file__).with_name("verify-application-records.py"))
records = importlib.util.module_from_spec(spec)
spec.loader.exec_module(records)


def check(inputs, bundle, policy_path, output, gh):
    if os.environ.get("CI") != "true":
        raise records.CheckError("The real signature check requires CI")
    result = records.verify(inputs, bundle, policy_path, output, gh)
    rejected = []
    with tempfile.TemporaryDirectory(prefix=".stego-record-rejections-", dir=output.parent) as temporary:
        root = Path(temporary)
        def deny(name, source=inputs, policy=policy_path, selected_bundle=bundle, reason="A build command failed"):
            # A policy error must not hide a missing signature check.
            destination = root / (name + "-result")
            try:
                records.verify(source, selected_bundle, policy, destination, gh)
            except records.CheckError as error:
                if reason not in str(error):
                    raise records.CheckError("The rejection reason differs: " + name) from error
                if destination.exists():
                    raise records.CheckError("A rejected record left a result directory")
                rejected.append(name)
                return
            raise records.CheckError("An invalid record passed verification: " + name)
        original = json.loads(policy_path.read_bytes())
        changed_policy = root / "policy.json"
        selections = [
            ("different-workflow-source", "revision", ("0" if original["revision"][0] != "0" else "1") + original["revision"][1:]),
            ("different-workflow", "workflow" if original['format'] == 1 else "signer_workflow", ".github/workflows/untrusted.yml"),
            ("different-application", "application_revision", ("0" if original["application_revision"][0] != "0" else "1") + original["application_revision"][1:]),
            ("different-trust-store", "trust_store_sha256", "0" * 64),
        ]
        if original['format'] == 2:
            selections.extend([
                ("different-signer-source", "signer_revision", ("0" if original['signer_revision'][0] != "0" else "1") + original['signer_revision'][1:]),
                ("different-signer-repository", "signer_repository", "untrusted/other"),
                ("different-caller-repository", "repository", "untrusted/other"),
                ("different-caller-reference", "reference", "refs/heads/untrusted"),
            ])
        for name, key, value in selections:
            changed_policy.write_text(json.dumps(dict(original, **{key: value})))
            deny(name, policy=changed_policy, reason=("signed application selection" if key == "application_revision" else "signed CA selection" if key == "trust_store_sha256" else "A build command failed"))
        for name in ["build.json", "image.json"]:
            changed = root / (name + "-inputs")
            shutil.copytree(inputs, changed)
            record = json.loads((changed / name).read_bytes())
            record["format"] = 2
            (changed / name).write_text(json.dumps(record))
            deny("changed-" + name, source=changed)
        corrupt = root / "invalid-bundle.jsonl"
        corrupt.write_text('{"not":"an attestation"}\n')
        deny("invalid-signature-bundle", selected_bundle=corrupt)
        if records.verify(inputs, bundle, policy_path, root / "valid-after-rejections", gh) != result:
            raise records.CheckError("The valid records changed after the rejection checks")
    evidence = {"policy": result["policy"], "build_record_sha256": result["build_record_sha256"],
                "image_record_sha256": result["image_record_sha256"], "records_authenticated": True,
                "rejected": rejected, "valid_after_rejections": True,
                "image_contents_checked": False, "registry_publication_checked": False}
    (output / "rejection-checks.json").write_text(json.dumps(evidence, indent=2) + "\n")
    print(json.dumps(evidence, indent=2))


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ["inputs", "bundle", "policy", "output", "gh"]:
        parser.add_argument("--" + name, required=True, type=Path)
    args = parser.parse_args()
    def interrupted(_number, _frame):
        raise records.CheckError("The application signature check was interrupted")
    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGINT, interrupted)
    os.umask(0o077)
    check(args.inputs, args.bundle, args.policy, args.output, args.gh)
