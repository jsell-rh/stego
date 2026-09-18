"""Check dry-run safety and unchanged workspace settings in the live runner."""
import copy
import importlib.util
import json
from pathlib import Path
import subprocess
import unittest

spec = importlib.util.spec_from_file_location("isolated", Path(__file__).with_name("check-isolated-allocation.py"))
isolated = importlib.util.module_from_spec(spec)
spec.loader.exec_module(isolated)


class Fixture(isolated.Check):
    def __init__(self, response, stored=None):
        self.response, self.stored, self.commands = response, stored, []
        self.result = {}

    def probe(self, name, command, obj=None, user=None, allowed=False, policy=None):
        self.commands.append(command)
        return subprocess.CompletedProcess(command, 0 if allowed else 1, json.dumps(self.response), "")

    def get(self, obj):
        return self.stored


class IsolatedRunnerTests(unittest.TestCase):
    def test_namespace_mode_requires_a_matching_guard(self):
        class ModeFixture(isolated.Check):
            def __init__(self, response):
                self.response, self.result, self.probes, self.commands = response, {}, [], []

            def run(self, command, obj=None, user=None):
                self.commands.append(command)
                return subprocess.CompletedProcess(command, 1, "", self.response)

            def save(self):
                pass

        for name in ["ownership", "allocation", "pods.tenant"]:
            policy = isolated.common.CONTROL + ".widget-queue." + name
            check = ModeFixture("ValidatingAdmissionPolicy '" + policy + "' with binding '" + policy + "' denied request: mode differs")
            if name == "pods.tenant":
                with self.assertRaisesRegex(RuntimeError, "expected admission policy"):
                    check.namespace_mode_probe("fixture")
                self.assertEqual(check.probes, [])
            else:
                check.namespace_mode_probe("fixture")
                self.assertEqual(len(check.probes), 1)
            self.assertIn("--dry-run=server", check.commands[0])
        with self.assertRaisesRegex(RuntimeError, "expected admission policy"):
            ModeFixture("Forbidden: access denied").namespace_mode_probe("fixture")

    def test_no_pod_persistence(self):
        check = Fixture(None)
        with self.assertRaisesRegex(RuntimeError, "persistence is forbidden"):
            check.create(isolated.isolated_pod("fixture", "account"))
        self.assertEqual(check.commands, [])

    def test_server_dry_run(self):
        obj = isolated.isolated_pod("fixture", "account")
        check = Fixture(obj)
        check.pod_probe("valid", obj, allowed=True)
        self.assertEqual(check.commands, [["create", "--dry-run=server", "-f", "-", "-o", "json"]])

    def test_changes_are_not_accepted(self):
        original = isolated.isolated_pod("fixture", "account")
        changes = [lambda o: o["spec"].update(runtimeClassName="other"),
                   lambda o: o["spec"].update(serviceAccountName="other"),
                   lambda o: o["spec"]["initContainers"][0]["securityContext"].update(runAsUser=1000),
                   lambda o: o["spec"]["volumes"][0]["emptyDir"].update(medium="Memory")]
        for change in changes:
            response = copy.deepcopy(original)
            change(response)
            with self.assertRaisesRegex(RuntimeError, "Admission changed"):
                Fixture(response).pod_probe("changed", original, allowed=True)

    def test_persisted_pod_fails(self):
        obj = isolated.isolated_pod("fixture", "account")
        for allowed in [False, True]:
            with self.assertRaisesRegex(RuntimeError, "stored Pod"):
                Fixture(obj, stored=obj).pod_probe("stored", obj, allowed=allowed)

    def test_application_checks_use_only_dry_run(self):
        namespace = isolated.common.PREFIX + "12345678"
        account = isolated.common.account_name(isolated.common.CONTROL, "owner-1")
        check = Fixture(isolated.isolated_pod(namespace, account))
        check.created = [{"kind": "Namespace", "metadata": {"name": namespace}}]
        check.application_probes()
        self.assertEqual(len(check.commands), 9)
        self.assertTrue(all(c == ["create", "--dry-run=server", "-f", "-", "-o", "json"] for c in check.commands))
        self.assertTrue(check.result["application_rules_checked"])
        self.assertTrue(check.result["related_network_admission_checked"])

    def test_fixture_is_bounded(self):
        obj = isolated.isolated_pod("fixture", "account")
        for c in obj["spec"]["containers"] + obj["spec"]["initContainers"]:
            self.assertEqual(set(c["resources"]["limits"]), {"cpu", "memory", "ephemeral-storage"})
            self.assertIsNot(c["securityContext"].get("privileged"), True)
        self.assertFalse(obj["spec"]["automountServiceAccountToken"])
        self.assertEqual(obj["spec"]["initContainers"][0]["securityContext"], {"runAsUser": 0})
        self.assertEqual(obj["spec"]["volumes"][0]["emptyDir"], {})


if __name__ == "__main__":
    unittest.main()
