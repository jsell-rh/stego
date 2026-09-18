"""Check failure attribution and UID-based cleanup in the live test runner."""

import importlib.util
import json
from pathlib import Path
import subprocess
import time
import unittest

spec = importlib.util.spec_from_file_location("accounts", Path(__file__).with_name("check-allocation-account-identity.py"))
accounts = importlib.util.module_from_spec(spec)
spec.loader.exec_module(accounts)


class Fixture(accounts.Check):
    def __init__(self, objects=()):
        self.created = list(objects)
        self.objects = {accounts.path(obj): obj for obj in objects}
        self.probes = []
        self.result = {"complete": False, "probes": self.probes}
        self.deadline = time.monotonic() + 10
        self.commands = []
        self.reply = None
        self.reject_delete = False

    def save(self):
        pass

    def run(self, args, obj=None, user=None):
        self.commands.append((args, obj, user))
        if self.reply is not None:
            return self.reply
        target = args[1].removeprefix("--raw=")
        if target not in self.objects:
            return subprocess.CompletedProcess(args, 1, "", "Error from server (NotFound)")
        if args[0] == "get":
            return subprocess.CompletedProcess(args, 0, json.dumps(self.objects[target]), "")
        assert args[0] == "delete" and args[2:] == ["-f", "-"]
        stored = self.objects[target]
        assert obj["preconditions"]["uid"] == stored["metadata"]["uid"]
        if self.reject_delete:
            return subprocess.CompletedProcess(args, 1, "", "Error from server (Conflict)")
        del self.objects[target]
        if stored["kind"] == "Namespace":
            for key, item in list(self.objects.items()):
                if item["metadata"].get("namespace") == stored["metadata"]["name"]:
                    del self.objects[key]
        return subprocess.CompletedProcess(args, 0, "{}", "")


def namespace(uid):
    obj = accounts.namespace(accounts.PREFIX + "12345678", accounts.CONTROL, "owner-1")
    obj["metadata"]["uid"] = uid
    return obj


class AccountRunnerTests(unittest.TestCase):
    def test_expected_policy_is_required(self):
        check = Fixture()
        check.reply = subprocess.CompletedProcess([], 1, "", "Error from server (Forbidden): RBAC denied")
        with self.assertRaisesRegex(RuntimeError, "expected admission policy"):
            check.probe("policy", ["create"], policy="selected-policy")
        self.assertEqual(check.probes, [])

    def test_transport_failure_is_not_an_access_denial(self):
        check = Fixture()
        check.reply = subprocess.CompletedProcess([], 1, "", "connection refused")
        with self.assertRaisesRegex(RuntimeError, "access control"):
            check.probe("access", ["get"])
        self.assertEqual(check.probes, [])

    def test_equivalent_named_admission_guard_can_reject(self):
        check = Fixture()
        check.reply = subprocess.CompletedProcess([], 1, "", "The namespace is invalid: ValidatingAdmissionPolicy 'peer' with binding 'peer' denied request: reserved name")
        check.probe("reserved", ["create"], policy=["primary", "peer"])
        self.assertEqual(len(check.probes), 1)
        self.assertFalse(check.probes[0]["allowed"])

    def test_admission_error_and_unrelated_policy_are_not_denials(self):
        for text in [
            "ValidatingAdmissionPolicy 'primary' failed to evaluate: connection refused",
            "ValidatingAdmissionPolicy 'unrelated' with binding 'unrelated' denied request: reserved name",
            "ValidatingAdmissionPolicy 'primary-more' with binding 'primary-more' denied request: reserved name",
        ]:
            check = Fixture()
            check.reply = subprocess.CompletedProcess([], 1, "", text)
            with self.assertRaisesRegex(RuntimeError, "expected admission policy"):
                check.probe("reserved", ["create"], policy=["primary", "peer"])
            self.assertEqual(check.probes, [])

    def test_failed_probe_retains_bounded_response(self):
        check = Fixture()
        check.reply = subprocess.CompletedProcess([], 1, "", "connection refused " + "x" * 8192)
        with self.assertRaisesRegex(RuntimeError, "access control"):
            check.probe("access", ["get"])
        self.assertEqual(check.probes, [])
        observation = check.result["probe_observations"][0]
        self.assertEqual(observation["name"], "access")
        self.assertEqual(observation["returncode"], 1)
        self.assertEqual(len(observation["stderr"]), 4096)
        self.assertTrue(observation["stderr_truncated"])

    def test_denied_positive_case_is_not_a_pass(self):
        check = Fixture()
        check.reply = subprocess.CompletedProcess([], 1, "", "Error from server (Forbidden)")
        with self.assertRaisesRegex(RuntimeError, "Unexpected probe result"):
            check.probe("positive", ["get"], allowed=True)
        self.assertEqual(check.probes, [])

    def test_cleanup_uses_only_the_latest_namespace_uid(self):
        old, new = namespace("old-uid"), namespace("new-uid")
        check = Fixture([old, new])
        check.result["checks_passed"] = True
        check.cleanup()
        deletes = [obj for args, obj, _ in check.commands if args[0] == "delete"]
        self.assertEqual(len(deletes), 1)
        self.assertEqual(deletes[0]["preconditions"], {"uid": "new-uid"})
        self.assertEqual(check.objects, {})
        self.assertTrue(check.result["complete"])

    def test_changed_uid_is_preserved(self):
        old, new = namespace("old-uid"), namespace("new-uid")
        check = Fixture([old])
        check.objects[accounts.path(new)] = new
        with self.assertRaisesRegex(RuntimeError, "identity changed"):
            check.remove(old)
        self.assertFalse(any(args[0] == "delete" for args, _, _ in check.commands))
        self.assertEqual(check.objects[accounts.path(new)]["metadata"]["uid"], "new-uid")

    def test_delete_conflict_prevents_completion(self):
        check = Fixture([namespace("original-uid")])
        check.reject_delete = True
        check.result["checks_passed"] = True
        with self.assertRaisesRegex(RuntimeError, "cleanup is incomplete"):
            check.cleanup()
        self.assertFalse(check.result["complete"])
        self.assertFalse(check.result["cleanup_complete"])
        self.assertEqual(len(check.objects), 1)
        observation = check.result["cleanup_delete_observations"][0]
        self.assertEqual(observation["stderr"], "Error from server (Conflict)")
        self.assertEqual(observation["uid"], "original-uid")

    def test_cleanup_does_not_turn_a_failed_check_into_a_pass(self):
        check = Fixture([namespace("original-uid")])
        check.cleanup()
        self.assertTrue(check.result["cleanup_complete"])
        self.assertFalse(check.result["complete"])


if __name__ == "__main__":
    unittest.main()
