#!/usr/bin/env python3
"""Check CA capture expiry parsing and expired-root selection with no network."""

import datetime
import importlib.util
from pathlib import Path
import tempfile
import unittest


spec = importlib.util.spec_from_file_location("capture", Path(__file__).with_name("capture-image-ca.py"))
capture = importlib.util.module_from_spec(spec)
spec.loader.exec_module(capture)


class ExpiryChecks(unittest.TestCase):
    def test_parse_not_after_accepts_openssl_lines(self):
        for line, expected in [
            (b"notAfter=Sep 17 11:16:09 2036 GMT\n", datetime.datetime(2036, 9, 17, 11, 16, 9, tzinfo=datetime.timezone.utc)),
            (b"notAfter=May 12 23:59:59 2025 GMT\n", datetime.datetime(2025, 5, 12, 23, 59, 59, tzinfo=datetime.timezone.utc)),
            (b"notAfter=Dec  1 00:00:00 2030 GMT\n", datetime.datetime(2030, 12, 1, tzinfo=datetime.timezone.utc)),
        ]:
            self.assertEqual(capture.parse_not_after(line), expected)

    def test_parse_not_after_rejects_other_lines(self):
        for line in [b"", b"notAfter=Sep 17 11:16:09 2036 GMT", b"depth=0 C = US\n",
                     b"notAfter=Sep 17 11:16:09 2036 GMT\nextra\n"]:
            with self.assertRaises(ValueError):
                capture.parse_not_after(line)

    def test_expired_certificates_selects_expired_roots(self):
        certificates = ["a" * 64, "b" * 64, "c" * 64]
        utc = datetime.timezone.utc
        expiries = [datetime.datetime(2025, 5, 12, tzinfo=utc),
                    datetime.datetime(2036, 9, 17, tzinfo=utc),
                    datetime.datetime(2026, 9, 26, 0, 0, 0, tzinfo=utc)]
        captured = datetime.datetime(2026, 9, 26, 0, 0, 0, tzinfo=utc)
        # The root expiring exactly at capture time counts as expired.
        self.assertEqual(capture.expired_certificates(certificates, expiries, captured), ["a" * 64, "c" * 64])
        # One second later the same roots are expired; the 2036 root is not.
        self.assertEqual(capture.expired_certificates(certificates, expiries, captured + datetime.timedelta(seconds=1)), ["a" * 64, "c" * 64])

    def test_expired_certificates_rejects_length_mismatch(self):
        with self.assertRaises(ValueError):
            capture.expired_certificates(["a" * 64], [], datetime.datetime.now(datetime.timezone.utc))


if __name__ == "__main__":
    unittest.main()
