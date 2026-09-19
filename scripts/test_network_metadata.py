"""Check the stored Pod boundary and dry-run status probes without a cluster."""
import copy
import importlib.util
import json
from pathlib import Path
from types import SimpleNamespace
import subprocess
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location("network", Path(__file__).with_name("check-network-metadata.py"))
network = importlib.util.module_from_spec(spec)
spec.loader.exec_module(network)


class Fixture(network.Check):
    def __init__(self):
        self.args = SimpleNamespace(node="worker.test")
        self.handler = "stego-no-runtime-test"
        self.created = [{"kind": "Namespace", "metadata": {"name": network.common.PREFIX + "12345678", "uid": "namespace-uid"}}]
        self.runtime = {"kind": "RuntimeClass", "metadata": {"name": network.pods.RUNTIME, "uid": "runtime-uid"}, "handler": self.handler}
        self.created.append(copy.deepcopy(self.runtime))
        self.intents, self.result, self.commands = [], {}, []
        self.status_pod = None
        self.stored = network.pending_pod(self.created[0]["metadata"]["name"], "account", self.args.node)
        self.stored["metadata"].update(uid="pod-uid", resourceVersion="1")

    def save(self):
        pass

    def get(self, obj):
        return copy.deepcopy(self.runtime if obj["kind"] == "RuntimeClass" else self.stored)

    def probe(self, name, command, obj=None, user=None, allowed=False, policy=None):
        self.commands.append((command, user, allowed, policy))
        admitted = copy.deepcopy(self.stored)
        changes = json.loads(command[command.index("-p") + 1])["metadata"]["annotations"]
        annotations = admitted["metadata"].setdefault("annotations", {})
        for key, value in changes.items():
            if value is None:
                annotations.pop(key, None)
            else:
                annotations[key] = value
        return subprocess.CompletedProcess(command, 0 if allowed else 1, json.dumps(admitted), "")


class NetworkMetadataTests(unittest.TestCase):
    def test_fixture_has_two_execution_barriers_and_limits(self):
        obj = network.pending_pod("namespace", "account", "worker.test")
        self.assertEqual(obj["spec"]["runtimeClassName"], network.pods.RUNTIME)
        self.assertEqual(obj["spec"]["activeDeadlineSeconds"], 120)
        self.assertEqual(obj["spec"]["terminationGracePeriodSeconds"], 0)
        self.assertFalse(obj["spec"]["automountServiceAccountToken"])
        self.assertEqual(len(obj["spec"]["containers"]), 1)
        self.assertNotIn("initContainers", obj["spec"])
        container = obj["spec"]["containers"][0]
        self.assertTrue(container["image"].startswith("registry.invalid/"))
        self.assertIsNot(container["securityContext"].get("privileged"), True)
        self.assertFalse(container["securityContext"]["allowPrivilegeEscalation"])
        self.assertEqual(container["securityContext"]["capabilities"], {"drop": ["ALL"]})
        for field in ["requests", "limits"]:
            self.assertEqual(container["resources"][field], {"cpu": "10m", "memory": "16Mi", "ephemeral-storage": "16Mi"})

    def test_only_one_recorded_namespace_pod_can_be_created(self):
        check = Fixture()
        with patch.object(network.common.Check, "create", return_value=check.stored) as create:
            with self.assertRaisesRegex(RuntimeError, "recorded allocation"):
                check.create_status_pod("unrelated", "account")
            self.assertFalse(create.called)
            check.create_status_pod(check.stored["metadata"]["namespace"], "account")
            self.assertEqual(create.call_count, 1)
            self.assertEqual(check.result["pods_created"], 1)
            with self.assertRaisesRegex(RuntimeError, "Only one"):
                check.create_status_pod(check.stored["metadata"]["namespace"], "account")
            self.assertEqual(create.call_count, 1)
        with self.assertRaisesRegex(RuntimeError, "persistence is forbidden"):
            Fixture().create(check.stored)

    def test_changed_runtime_blocks_pod_creation(self):
        for key, value in [("uid", "other"), ("handler", "default")]:
            check = Fixture()
            if key == "uid":
                check.runtime["metadata"][key] = value
            else:
                check.runtime[key] = value
            with patch.object(network.common.Check, "create") as create:
                with self.assertRaisesRegex(RuntimeError, "runtime changed"):
                    check.create_status_pod(check.stored["metadata"]["namespace"], "account")
                self.assertFalse(create.called)

    def test_runtime_handler_is_unique_to_the_check(self):
        check = Fixture()
        runtime = {"kind": "RuntimeClass", "handler": "original"}
        with patch.object(network.isolated.Check, "create") as create:
            check.create(runtime)
            self.assertEqual(create.call_args.args[0]["handler"], check.handler)
        self.assertEqual(runtime["handler"], "original")

    def test_status_changes_use_dry_run_uid_and_node_group(self):
        check = Fixture()
        check.status_pod = check.stored
        check.metadata_probe("allowed", {network.MULTUS: "[]"}, "system:multus:worker.test", "system:multus", True)
        command, user, allowed, policy = check.commands[0]
        self.assertIn("--dry-run=server", command)
        self.assertIn("--subresource=status", command)
        self.assertIn("--as-group=system:multus", command)
        self.assertEqual(user, "system:multus:worker.test")
        self.assertEqual(json.loads(command[-1])["metadata"]["uid"], "pod-uid")
        self.assertTrue(allowed)
        self.assertIsNone(policy)
        self.assertNotIn(network.MULTUS, check.stored["metadata"].get("annotations", {}))
        check.metadata_probe("denied", {network.MULTUS: "[]"}, "workload")
        self.assertEqual(check.commands[-1][3], network.common.CONTROL + ".widget-queue.pods.tenant")

    def test_changed_uid_or_started_container_stops_the_check(self):
        for mode in ["uid", "running", "terminated", "containerID", "lastState", "limits"]:
            check = Fixture()
            check.status_pod = copy.deepcopy(check.stored)
            if mode == "uid":
                check.stored["metadata"]["uid"] = "other"
            elif mode == "limits":
                check.stored["spec"]["containers"][0]["resources"]["limits"]["cpu"] = "20m"
            elif mode == "lastState":
                check.stored["status"] = {"containerStatuses": [{"lastState": {"terminated": {}}}]}
            else:
                status = {"containerID": "test"} if mode == "containerID" else {"state": {mode: {"startedAt": "now"}}}
                check.stored["status"] = {"containerStatuses": [status]}
            with self.assertRaises(RuntimeError):
                check.observe_status_pod()

    def test_pod_cleanup_failure_keeps_namespace_and_policies(self):
        check = Fixture()
        check.created.append(check.stored)
        with patch.object(check, "remove", side_effect=RuntimeError("retained")), patch.object(network.isolated.Check, "cleanup") as cleanup:
            with self.assertRaisesRegex(RuntimeError, "retain the policies"):
                check.cleanup()
            self.assertFalse(cleanup.called)
            self.assertFalse(check.result["cleanup_complete"])

    def test_uncertain_pod_create_is_recovered_before_cleanup(self):
        check = Fixture()
        check.intents = [{"object": check.stored, "resolved": False}]
        order = []
        def recover(intent):
            order.append("recover")
            check.created.append(check.stored)
            intent["resolved"] = True
        with patch.object(check, "recover_intent", side_effect=recover), patch.object(check, "remove", side_effect=lambda obj: order.append("pod")), patch.object(network.isolated.Check, "cleanup", side_effect=lambda: order.append("policies")):
            check.cleanup()
        self.assertEqual(order, ["recover", "pod", "policies"])


if __name__ == "__main__":
    unittest.main()
