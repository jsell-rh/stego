"""Check that the admission fixture cannot persist a Pod or hide a changed result."""

import copy
import importlib.util
import json
from pathlib import Path
import subprocess
import unittest

spec = importlib.util.spec_from_file_location("pods", Path(__file__).with_name("check-allocation-pods.py"))
pods = importlib.util.module_from_spec(spec)
spec.loader.exec_module(pods)


class Fixture(pods.Check):
    def __init__(self, response, stored=None):
        self.response = response
        self.stored = stored
        self.commands = []

    def probe(self, name, command, obj=None, user=None, allowed=False, policy=None):
        self.commands.append((command, obj, user, allowed, policy))
        return subprocess.CompletedProcess(command, 0 if allowed else 1, json.dumps(self.response), "")

    def get(self, obj):
        return self.stored


class PodRunnerTests(unittest.TestCase):
    def test_persistence_is_rejected_before_a_request(self):
        check = Fixture(None)
        with self.assertRaisesRegex(RuntimeError, "persistence is forbidden"):
            check.create(pods.pod("fixture", "account"))
        self.assertEqual(check.commands, [])

    def test_request_requires_server_dry_run(self):
        obj = pods.pod("fixture", "account")
        check = Fixture(obj)
        check.pod_probe("valid", obj, "writer", allowed=True)
        self.assertEqual(check.commands, [(["create", "--dry-run=server", "-f", "-", "-o", "json"], obj, "writer", True, None)])

    def test_admission_cannot_replace_the_selected_identity(self):
        obj = pods.pod("fixture", "account")
        for field in ["serviceAccountName", "runtimeClassName"]:
            with self.subTest(field=field):
                response = copy.deepcopy(obj)
                response["spec"][field] = "other"
                with self.assertRaisesRegex(RuntimeError, "changed the selected"):
                    Fixture(response).pod_probe("changed", obj, allowed=True)

    def test_persisted_pod_is_not_a_success(self):
        obj = pods.pod("fixture", "account")
        for allowed in [False, True]:
            with self.subTest(allowed=allowed):
                with self.assertRaisesRegex(RuntimeError, "left a stored Pod"):
                    Fixture(obj, stored=obj).pod_probe("stored", obj, allowed=allowed)

    def test_fixture_has_resource_and_security_bounds(self):
        obj = pods.pod("fixture", "account")
        spec = obj["spec"]
        self.assertFalse(spec["automountServiceAccountToken"])
        self.assertTrue(spec["securityContext"]["runAsNonRoot"])
        for container in spec["containers"]:
            self.assertFalse(container["securityContext"]["allowPrivilegeEscalation"])
            self.assertEqual(container["securityContext"]["capabilities"], {"drop": ["ALL"]})
            self.assertEqual(set(container["resources"]["limits"]), {"cpu", "memory", "ephemeral-storage"})
        self.assertEqual(pods.Check.policy_count, 8)
        self.assertEqual(pods.common.Check.policy_count, 7)


if __name__ == "__main__":
    unittest.main()
