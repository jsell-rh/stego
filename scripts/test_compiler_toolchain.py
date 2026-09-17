#!/usr/bin/env python3
"""Check SDK capture and extraction with small archives. Do not execute Go."""

import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import shutil
import tarfile
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location("sdk", Path(__file__).with_name("prepare-compiler-toolchain.py"))
sdk = importlib.util.module_from_spec(spec)
spec.loader.exec_module(sdk)


class ToolchainChecks(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.archive = self.root / "sdk.tar.gz"
        self.output = self.root / "result"
        self.files = [("go/bin/go", b"not an executable program", 0o755),
                      ("go/src/input.go", b"package fixture\n", 0o644)]

    def fixture(self, entries=None):
        entries = self.files if entries is None else entries
        with tarfile.open(self.archive, "w:gz") as archive:
            directory = tarfile.TarInfo("go")
            directory.type = tarfile.DIRTYPE
            archive.addfile(directory)
            for name, body, mode in entries:
                member = tarfile.TarInfo(name)
                member.mode = mode
                member.size = len(body)
                archive.addfile(member, io.BytesIO(body))
        records = sorted([[name.removeprefix("go/"), hashlib.sha256(body).hexdigest(), bool(mode & 0o111)]
                          for name, body, mode in self.files])
        return {"url": "https://dl.google.com/go/fixture.tar.gz",
                "sha256": hashlib.sha256(self.archive.read_bytes()).hexdigest(),
                "size": self.archive.stat().st_size,
                "inventory": {"sha256": hashlib.sha256(json.dumps(records, sort_keys=True, separators=(",", ":")).encode()).hexdigest(),
                              "files": len(self.files), "bytes": sum(len(body) for _, body, _ in self.files)}}

    def test_valid_archive_retains_file_bytes_and_executable_bits_without_execution(self):
        release = self.fixture()
        with patch.object(sdk.control, "GO_RELEASE", release), patch.object(sdk.control, "command") as command:
            sdk.prepare(self.archive, self.output)
            command.assert_not_called()
            for name, body, mode in self.files:
                file = self.output / name
                self.assertEqual(file.read_bytes(), body)
                self.assertEqual(file.stat().st_mode & 0o777, mode)
            self.assertEqual(json.loads((self.output / "toolchain.json").read_text()), release)
            self.assertEqual(sorted(p.name for p in self.output.iterdir()), ["go", "toolchain.json"])

    def test_bad_archive_is_rejected_before_parsing(self):
        release = self.fixture()
        body = bytearray(self.archive.read_bytes())
        body[-1] ^= 1
        self.archive.write_bytes(body)
        with patch.object(sdk.control, "GO_RELEASE", release), patch.object(sdk.tarfile, "open") as parse:
            with self.assertRaisesRegex(sdk.CheckError, "checksum"):
                sdk.prepare(self.archive, self.output)
            parse.assert_not_called()
        self.assertFalse(self.output.exists())

    def test_links_special_files_and_wrong_size_fail_without_output(self):
        release = self.fixture()
        link = self.root / "link"
        link.symlink_to(self.archive)
        fifo = self.root / "fifo"
        os.mkfifo(fifo)
        short = self.root / "short"
        short.write_bytes(b"x")
        with patch.object(sdk.control, "GO_RELEASE", release):
            for source in [link, fifo, short]:
                with self.subTest(source=source), self.assertRaises((sdk.CheckError, OSError)):
                    sdk.prepare(source, self.output)
                self.assertFalse(self.output.exists())

    def test_existing_output_is_never_removed_or_replaced(self):
        release = self.fixture()
        self.output.mkdir()
        marker = self.output / "unrelated"
        marker.write_bytes(b"keep")
        with patch.object(sdk.control, "GO_RELEASE", release), self.assertRaises(FileExistsError):
            sdk.prepare(self.archive, self.output)
        self.assertEqual(marker.read_bytes(), b"keep")
        link = self.root / "output-link"
        link.symlink_to(self.output, target_is_directory=True)
        with self.assertRaises(FileExistsError):
            sdk.prepare(self.archive, link)
        self.assertEqual(marker.read_bytes(), b"keep")

    def test_invalid_paths_repeated_files_and_setid_modes_fail(self):
        for entries in [[("go/../escape", b"x", 0o644)],
                        [("/go/escape", b"x", 0o644)],
                        [("go//escape", b"x", 0o644)],
                        [("go/./escape", b"x", 0o644)],
                        [("other/escape", b"x", 0o644)],
                        [("go/escape\\name", b"x", 0o644)],
                        [self.files[0], self.files[0]],
                        [("go/bin/go", b"x", 0o4755)]]:
            with self.subTest(entries=entries):
                release = self.fixture(entries)
                with patch.object(sdk.control, "GO_RELEASE", release), self.assertRaises(sdk.CheckError):
                    sdk.prepare(self.archive, self.output)
                self.assertFalse(self.output.exists())
                self.assertFalse((self.root / "escape").exists())

    def test_archive_links_devices_and_fifos_fail(self):
        for kind in [tarfile.SYMTYPE, tarfile.LNKTYPE, tarfile.FIFOTYPE, tarfile.CHRTYPE]:
            with self.subTest(kind=kind):
                release = self.fixture()
                with tarfile.open(self.archive, "w:gz") as archive:
                    member = tarfile.TarInfo("go/input")
                    member.type = kind
                    member.linkname = "../escape"
                    archive.addfile(member)
                release.update(size=self.archive.stat().st_size,
                               sha256=hashlib.sha256(self.archive.read_bytes()).hexdigest())
                with patch.object(sdk.control, "GO_RELEASE", release), self.assertRaises(sdk.CheckError):
                    sdk.prepare(self.archive, self.output)
                self.assertFalse(self.output.exists())

    def test_extraction_size_count_and_inventory_limits_fail(self):
        for field, value in [("files", 1), ("bytes", 1), ("sha256", "0" * 64)]:
            with self.subTest(field=field):
                release = self.fixture()
                release["inventory"][field] = value
                with patch.object(sdk.control, "GO_RELEASE", release), self.assertRaises(sdk.CheckError):
                    sdk.prepare(self.archive, self.output)
                self.assertFalse(self.output.exists())

    def test_changed_sdk_bytes_modes_and_extra_files_fail_before_use(self):
        release = self.fixture()
        with patch.object(sdk.control, "GO_RELEASE", release):
            sdk.prepare(self.archive, self.output)
            go = self.output / "go/bin/go"
            original = go.read_bytes()
            for change in [lambda: go.write_bytes(b"changed"), lambda: go.chmod(0o644),
                           lambda: (self.output / "go/extra").write_bytes(b"extra")]:
                change()
                with self.assertRaises(sdk.CheckError):
                    sdk.control.require_toolchain(go)
                go.write_bytes(original)
                go.chmod(0o755)
                (self.output / "go/extra").unlink(missing_ok=True)
            with self.assertRaises(sdk.CheckError):
                sdk.control.require_toolchain(go.with_name("other"))

    def test_capture_prevents_later_input_replacement(self):
        release = self.fixture()
        snapshot = self.root / "snapshot"
        with patch.object(sdk.control, "GO_RELEASE", release):
            sdk.capture_archive(self.archive, snapshot)
            self.archive.write_bytes(b"replacement")
            self.output.mkdir()
            sdk.extract_archive(snapshot, self.output)
            self.assertEqual((self.output / "bin/go").read_bytes(), self.files[0][1])

    def test_download_uses_fixed_https_policy_and_no_inherited_settings(self):
        release = self.fixture()
        def download(args, cwd, env, **limits):
            self.assertEqual(args[:2], ["/usr/bin/curl", "-q"])
            self.assertEqual(args[args.index("--url") + 1], release["url"])
            self.assertEqual(args[args.index("--max-filesize") + 1], str(release["size"]))
            self.assertEqual(args[args.index("--proto") + 1], "=https")
            self.assertNotIn("--location", args)
            self.assertEqual(set(env), {"PATH", "HOME", "LANG", "LC_ALL"})
            self.assertEqual(limits, {"timeout": 185, "limit": 65536})
            shutil.copyfile(self.archive, args[args.index("--output") + 1])
        with patch.object(sdk.control, "GO_RELEASE", release), patch.object(sdk.control, "command", side_effect=download):
            with patch.dict(os.environ, {"HTTPS_PROXY": "https://private.invalid", "LD_PRELOAD": "/private.so", "CURL_HOME": "/private"}):
                sdk.prepare(None, self.output)
        self.assertTrue((self.output / "toolchain.json").exists())


if __name__ == "__main__":
    unittest.main()
