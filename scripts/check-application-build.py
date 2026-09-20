#!/usr/bin/env python3
"""Check the real application artifact and reject changed records and bytes."""

import argparse
import hashlib
import json
from pathlib import Path
import shutil
import subprocess


def sha(path):
    value = hashlib.sha256()
    with path.open("rb") as stream:
        for block in iter(lambda: stream.read(65536), b""):
            value.update(block)
    return value.hexdigest()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ["compiler", "result", "evidence", "source"]:
        parser.add_argument("--" + name, required=True, type=Path)
    parser.add_argument("--revision", required=True)
    parser.add_argument("--compiler-revision")
    parser.add_argument("--module", default="examples/user-management")
    parser.add_argument("--target", default="out")
    args = parser.parse_args()
    record_path = args.result / "build.json"
    binary = args.result / "application"
    record = json.loads(record_path.read_text())
    expected = sha(record_path)
    assert record["source_revision"] == args.revision
    assert record["module"] == args.module and record["target"] == args.target
    assert record["independent_builds"] == 2
    assert record["environment"]["GOPROXY"] == "off"
    assert record["artifact"]["sha256"] == sha(binary)
    assert record["build_compiler_artifact"]["sha256"] == sha(args.compiler)
    assert record["build_compiler"]["revision"] == (args.compiler_revision or args.revision)
    assert record["build_compiler"]["source_state"] == "clean"
    assert record["generation_state_sha256"] == sha(args.source / record["module"] / ".stego/state.yaml")
    cases = []

    def verify(name, selected_record, selected_binary, digest, success):
        process = subprocess.run([
            str(args.compiler), "build", "verify", "--record=" + str(selected_record),
            "--artifact=" + str(selected_binary), "--record-sha256=" + digest,
        ], capture_output=True, timeout=30)
        assert (process.returncode == 0) == success, name
        assert b"not canonical" not in process.stderr, name
        cases.append({"name": name, "expected_success": success, "exit_code": process.returncode})

    verify("original", record_path, binary, expected, True)
    # This is the source transfer used by a deployment job. Copy only inputs
    # from the record that the native verifier has already accepted.
    source_cases = []
    snapshot = args.evidence / "checked-source"
    snapshot.mkdir(mode=0o700)
    def verify_source(name, selected=snapshot, success=False, digest=expected):
        process = subprocess.run([
            str(args.compiler), "build", "verify-source", "--record=" + str(record_path),
            "--record-sha256=" + digest, "--source=" + str(selected),
        ], capture_output=True, timeout=30)
        assert (process.returncode == 0) == success, (name, process.stderr)
        source_cases.append({"name": name, "expected_success": success, "exit_code": process.returncode})
    try:
        for item in record["inputs"]:
            target = snapshot / item["path"]
            target.parent.mkdir(parents=True, exist_ok=True)
            shutil.copy2(args.source / item["path"], target)
        verify_source("original source", success=True)
        verify_source("wrong source record digest", digest="0" * 64)
        selected = snapshot / args.module / "go.mod"
        original_data, original_mode = selected.read_bytes(), selected.stat().st_mode & 0o777
        selected.write_bytes(original_data + b"\n")
        verify_source("changed source file")
        selected.unlink()
        verify_source("missing source file")
        selected.write_bytes(original_data)
        selected.chmod(original_mode ^ 0o100)
        verify_source("changed source executable bit")
        selected.chmod(original_mode)
        extra = snapshot / "unexpected.go"
        extra.write_text("package unexpected\n")
        verify_source("added source file")
        extra.unlink()
        selected.unlink()
        selected.symlink_to((args.source / args.module / "go.mod").resolve())
        verify_source("linked source file")
        selected.unlink()
        selected.write_bytes(original_data)
        selected.chmod(original_mode)
        linked_root = args.evidence / "linked-source"
        linked_root.symlink_to(snapshot.resolve(), target_is_directory=True)
        try:
            verify_source("linked source root", selected=linked_root)
        finally:
            linked_root.unlink()
        verify_source("restored source", success=True)
    finally:
        shutil.rmtree(snapshot)
    changed_binary = args.evidence / "changed-application"
    shutil.copyfile(binary, changed_binary)
    with changed_binary.open("ab") as stream:
        stream.write(b"changed")
    verify("changed executable", record_path, changed_binary, expected, False)
    changed_binary.unlink()
    verify("wrong trusted digest", record_path, binary, "0" * 64, False)

    def change_module_checksum(value):
        for module in value["modules"]:
            content = module.get("replacement", module)
            if content.get("sum"):
                current = content["sum"]
                content["sum"] = "h1:" + ("A" if current[3] != "A" else "B") + current[4:]
                return
        raise AssertionError("The fixture must contain a remote compiled module")

    # Even a matching digest cannot make a record with a different build policy valid.
    for name, change in [
        ("changed compiled module checksum", change_module_checksum),
        ("missing compiled module", lambda r: r["modules"].pop()),
        ("single build", lambda r: r.update(independent_builds=1)),
        ("ambient compiler flags", lambda r: r["environment"].update(GOFLAGS="-race")),
        ("wrong toolchain", lambda r: r["toolchain"].update(sha256="0" * 64)),
        ("changed source inventory", lambda r: r["inputs"][0].update(sha256="0" * 64)),
        ("changed executable settings", lambda r: r["binary_settings"].update(CGO_ENABLED="1")),
    ]:
        value = json.loads(record_path.read_text())
        change(value)
        # Preserve Go's field and map ordering from the original record.
        data = json.dumps(value, indent=2, ensure_ascii=False) + "\n"
        for char, escaped in [("&", r"\u0026"), ("<", r"\u003c"), (">", r"\u003e"), ("\u2028", r"\u2028"), ("\u2029", r"\u2029")]:
            data = data.replace(char, escaped)
        selected = args.evidence / "changed-build.json"
        selected.write_text(data)
        verify(name, selected, binary, sha(selected), False)
        selected.unlink()
    report = {"source": args.revision, "compiler_source": args.compiler_revision or args.revision,
              "module": args.module, "target": args.target, "record_sha256": expected,
              "artifact_sha256": sha(binary), "cases": cases,
              "source_cases": source_cases, "source_inputs_checked": len(record["inputs"]),
              "application_executed": False,
              "scope": "The selected generated application executable and record. Image contents, signing, and Hypershell adoption remain separate checks."}
    (args.evidence / "verification.json").write_text(json.dumps(report, indent=2) + "\n")
    print(json.dumps(report))


if __name__ == "__main__":
    main()
