"""Check the control-account audit without a cluster or real credentials."""
import copy
import importlib.util
from pathlib import Path
import subprocess
import unittest
from unittest.mock import Mock
import urllib.error


def load(name, filename):
    spec = importlib.util.spec_from_file_location(name, Path(__file__).with_name(filename))
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


audit = load("control_audit", "check-allocation-control-identity.py")
common_tests = load("common_tests", "test_allocation_account_identity.py")


class ControlAuditTests(unittest.TestCase):
    def test_transport_requires_verified_direct_https(self):
        for cluster in [
            {"server": "http://api.test"}, {"server": "https://user@api.test"},
            {"server": "https://api.test/other"}, {"server": "https://api.test?query"},
            {"server": "https://api.test", "insecure-skip-tls-verify": True},
            {"server": "https://api.test", "proxy-url": "http://proxy.test"},
            {"server": "https://api.test", "tls-server-name": "other.test"},
        ]:
            with self.subTest(cluster=cluster), self.assertRaises(RuntimeError):
                audit.transport(cluster)

    def test_redirect_never_forwards_credentials(self):
        with self.assertRaises(RuntimeError):
            audit.NoRedirect().redirect_request(None, None, 302, "", {}, "https://other.test")

    def test_private_transport_error_is_not_an_access_denial(self):
        opener = Mock()
        opener.open.side_effect = RuntimeError("private-token")
        with self.assertRaisesRegex(RuntimeError, "^The fixture credential request failed$"):
            audit.request_status("https://api.test", opener, "/api/v1/namespaces/fixture", "private-token")

    def test_only_authentication_and_authorization_errors_are_classified(self):
        for code in [401, 403, 404, 429, 500, 503]:
            opener = Mock()
            opener.open.side_effect = urllib.error.HTTPError("https://api.test", code, "private-token", {}, None)
            with self.subTest(code=code):
                if code in [401, 403]:
                    self.assertEqual(audit.request_status("https://api.test", opener, "/api/v1/namespaces/fixture", "private-token"), code)
                else:
                    with self.assertRaisesRegex(RuntimeError, "unexpected HTTP status"):
                        audit.request_status("https://api.test", opener, "/api/v1/namespaces/fixture", "private-token")

    def test_token_and_path_rejected_before_network_use(self):
        opener = Mock()
        for path, token in [("/other", "token"), ("/api/v1/namespaces/fixture", "token\n"), ("/api/v1/namespaces/fixture", "")]:
            with self.subTest(path=path), self.assertRaises(RuntimeError):
                audit.request_status("https://api.test", opener, path, token)
        opener.open.assert_not_called()

    def test_token_issuance_errors_do_not_expose_output(self):
        check = object.__new__(audit.Check)
        check.run = Mock(return_value=subprocess.CompletedProcess([], 1, "private-token", "private-token"))
        with self.assertRaisesRegex(RuntimeError, "^Fixture token issuance failed; private output is withheld$"):
            check.token("fixture", "account")

    def test_control_namespace_cleanup_uses_latest_uid(self):
        old = audit.common.namespace(audit.common.CONTROL)
        old["metadata"]["uid"] = "old"
        new = copy.deepcopy(old)
        new["metadata"]["uid"] = "new"
        fixture = common_tests.Fixture([old, new])
        fixture.cleanup()
        deletes = [obj for args, obj, user in fixture.commands if args[0] == "delete"]
        self.assertEqual(len(deletes), 1)
        self.assertEqual(deletes[0]["preconditions"], {"uid": "new"})
        self.assertTrue(fixture.result["cleanup_complete"])
        self.assertFalse(fixture.result["complete"])

    def test_unrecorded_control_replacement_is_preserved(self):
        old = audit.common.namespace(audit.common.CONTROL)
        old["metadata"]["uid"] = "old"
        new = copy.deepcopy(old)
        new["metadata"]["uid"] = "foreign"
        fixture = common_tests.Fixture([old])
        fixture.objects[audit.common.path(old)] = new
        with self.assertRaisesRegex(RuntimeError, "cleanup is incomplete"):
            fixture.cleanup()
        self.assertFalse(any(args[0] == "delete" for args, obj, user in fixture.commands))
        self.assertFalse(fixture.result["complete"])


if __name__ == "__main__":
    unittest.main()
