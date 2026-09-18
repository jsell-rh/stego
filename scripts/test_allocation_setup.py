"""Check recovery of uncertain setup writes without hiding failed assertions."""
import copy
import importlib.util
import json
from pathlib import Path
import subprocess
import unittest

spec = importlib.util.spec_from_file_location("safety", Path(__file__).with_name("test_allocation_account_identity.py"))
safety = importlib.util.module_from_spec(spec)
spec.loader.exec_module(safety)
accounts = safety.accounts


class SetupFixture(safety.Fixture):
    def __init__(self, outcomes):
        super().__init__()
        self.intents = []
        self.outcomes = list(outcomes)
        self.saved_intents = []
        self.create_calls = 0

    def save(self):
        self.saved_intents = copy.deepcopy(self.intents)

    def run(self, args, obj=None, user=None):
        if args[0] != "create":
            return super().run(args, obj, user)
        self.commands.append((args, obj, user))
        self.create_calls += 1
        assert self.saved_intents[-1]["object"] == obj and not self.saved_intents[-1]["resolved"]
        kind, commit = self.outcomes.pop(0)
        if commit:
            stored = copy.deepcopy(obj)
            stored["metadata"]["uid"] = "created-uid"
            if commit == "foreign":
                stored["metadata"]["annotations"][accounts.INTENT_ANNOTATION] = "another-intent"
            if commit == "changed":
                stored["metadata"]["labels"]["example.test/owner"] = "other-owner"
            self.objects[accounts.path(stored)] = stored
        if kind == "local-timeout":
            raise subprocess.TimeoutExpired(args, 15)
        if kind == "timeout":
            return subprocess.CompletedProcess(args, 1, "", "Client.Timeout exceeded while awaiting headers")
        if kind == "forbidden":
            return subprocess.CompletedProcess(args, 1, "", "Error from server (Forbidden): denied")
        if kind == "conflict":
            return subprocess.CompletedProcess(args, 1, "", "Error from server (AlreadyExists)")
        assert kind == "success" and commit
        return subprocess.CompletedProcess(args, 0, json.dumps(stored), "")


class SetupRecoveryTests(unittest.TestCase):
    def test_lost_response_recovers_exact_owned_create(self):
        for failure in ["timeout", "local-timeout"]:
            check = SetupFixture([(failure, True)])
            desired = accounts.namespace(accounts.PREFIX + "12345678", accounts.CONTROL, "owner-1")
            stored = check.create(desired)
            self.assertEqual(stored["metadata"]["uid"], "created-uid")
            self.assertEqual(check.create_calls, 1)
            self.assertEqual(check.created[0]["metadata"]["uid"], "created-uid")
            self.assertTrue(check.intents[0]["resolved"])
            self.assertNotIn(accounts.INTENT_ANNOTATION, desired["metadata"]["annotations"])

    def test_absent_write_repeats_identical_create_once(self):
        check = SetupFixture([("timeout", False), ("success", True)])
        check.create(accounts.namespace("stego-setup-check"))
        creates = [obj for args, obj, user in check.commands if args[0] == "create"]
        self.assertEqual(len(creates), 2)
        self.assertEqual(creates[0], creates[1])
        self.assertEqual(len(check.created), 1)

    def test_late_first_commit_cannot_be_overwritten(self):
        check = SetupFixture([("timeout", False), ("conflict", True)])
        check.create(accounts.namespace("stego-setup-check"))
        self.assertEqual(check.create_calls, 2)
        self.assertFalse(any(args[0] in ["patch", "replace", "apply"] for args, obj, user in check.commands))

    def test_repeated_failure_stops_after_two_creates(self):
        check = SetupFixture([("timeout", False), ("timeout", False)])
        with self.assertRaisesRegex(RuntimeError, "creation failed"):
            check.create(accounts.namespace("stego-setup-check"))
        self.assertEqual(check.create_calls, 2)
        self.assertEqual(check.created, [])
        self.assertFalse(check.intents[0]["resolved"])

    def test_foreign_or_changed_content_is_not_adopted(self):
        for changed in ["foreign", "changed"]:
            check = SetupFixture([("timeout", changed)])
            obj = accounts.namespace(accounts.PREFIX + "12345678", accounts.CONTROL, "owner-1")
            with self.assertRaisesRegex(RuntimeError, "different owner or content"):
                check.create(obj)
            self.assertEqual(check.created, [])
            with self.assertRaisesRegex(RuntimeError, "cleanup is incomplete"):
                check.cleanup()
            self.assertEqual(len(check.objects), 1)
            self.assertFalse(any(args[0] == "delete" for args, obj, user in check.commands))

    def test_authoritative_denial_is_not_retried(self):
        check = SetupFixture([("forbidden", False)])
        with self.assertRaisesRegex(RuntimeError, "creation failed"):
            check.create(accounts.namespace("stego-setup-check"))
        self.assertEqual(check.create_calls, 1)
        self.assertFalse(check.result["setup_observations"][0]["transient"])

    def test_cleanup_recovers_a_late_owned_commit(self):
        check = SetupFixture([("timeout", False), ("timeout", False)])
        with self.assertRaisesRegex(RuntimeError, "creation failed"):
            check.create(accounts.namespace("stego-setup-check"))
        late = copy.deepcopy(check.intents[0]["object"])
        late["metadata"]["uid"] = "late-uid"
        check.objects[accounts.path(late)] = late
        check.cleanup()
        self.assertEqual(check.objects, {})
        self.assertTrue(check.result["cleanup_complete"])
        self.assertFalse(check.result["complete"])
        deletes = [obj for args, obj, user in check.commands if args[0] == "delete"]
        self.assertEqual(deletes[0]["preconditions"], {"uid": "late-uid"})

    def test_denial_message_cannot_be_classified_as_transport_failure(self):
        self.assertFalse(accounts.transient_create_error("Error from server (Forbidden): Client.Timeout exceeded"))
        self.assertFalse(accounts.transient_create_error("ValidatingAdmissionPolicy failed: connection refused"))
        self.assertFalse(accounts.transient_create_error("Unauthorized"))


if __name__ == "__main__":
    unittest.main()
