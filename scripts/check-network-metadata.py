#!/usr/bin/env python3
"""Check network metadata admission on one bounded Pod that cannot run.

Hold the shared cluster test lease. Use a fixed source copy and CI manifests.
The fixture uses an invalid image registry and an uninstalled runtime handler.
Only metadata probes use dry run. The stored Pod has a UID journal and is
removed before its namespace and policies. This does not test Kata or traffic.
"""
import argparse
import copy
import importlib.util
import json
from pathlib import Path
import time
import uuid

spec = importlib.util.spec_from_file_location("isolated", Path(__file__).with_name("check-isolated-allocation.py"))
isolated = importlib.util.module_from_spec(spec)
spec.loader.exec_module(isolated)
common, pods = isolated.common, isolated.pods
OVN = "k8s.ovn.org/pod-networks"
MULTUS = "k8s.v1.cni.cncf.io/network-status"


def pending_pod(namespace, account, node):
    obj = pods.pod(namespace, account)
    obj["metadata"]["name"] = "network-metadata-check"
    obj["spec"].update(nodeName=node, activeDeadlineSeconds=120, terminationGracePeriodSeconds=0)
    return obj


class Check(isolated.Check):
    def __init__(self, args):
        super().__init__(args)
        self.handler = "stego-no-runtime-" + uuid.uuid4().hex
        self.status_pod = None

    def create(self, obj, user=None):
        if obj["kind"] == "RuntimeClass":
            obj = copy.deepcopy(obj)
            obj["handler"] = self.handler
        return super().create(obj, user)

    def create_status_pod(self, namespace, account):
        if self.status_pod is not None or any(x["kind"] == "Pod" for x in self.created):
            raise RuntimeError("Only one status fixture Pod is permitted")
        owned = [x for x in self.created if x["kind"] == "Namespace" and x["metadata"]["name"] == namespace]
        if len(owned) != 1 or not namespace.startswith(common.PREFIX):
            raise RuntimeError("Status fixture requires its recorded allocation namespace")
        runtime = {"apiVersion": "node.k8s.io/v1", "kind": "RuntimeClass", "metadata": {"name": pods.RUNTIME}}
        recorded = [x for x in self.created if x["kind"] == "RuntimeClass" and x["metadata"]["name"] == pods.RUNTIME]
        current = self.get(runtime)
        if len(recorded) != 1 or current is None or current["metadata"].get("uid") != recorded[0]["metadata"]["uid"] or current.get("handler") != self.handler:
            raise RuntimeError("The recorded test runtime changed before Pod creation")
        obj = pending_pod(namespace, account, self.args.node)
        self.status_pod = common.Check.create(self, obj)
        self.result.update(pods_created=1, dry_run_only=False, runtime_handler_installed=False,
                           runtime_handler=self.handler, network_traffic_checked=False, kata_execution_checked=False)
        self.save()
        return self.status_pod

    def observe_status_pod(self):
        stored = self.get(self.status_pod)
        if stored is None or stored["metadata"]["uid"] != self.status_pod["metadata"]["uid"]:
            raise RuntimeError("Status fixture Pod identity changed")
        expected = pending_pod(stored["metadata"]["namespace"], self.status_pod["spec"]["serviceAccountName"], self.args.node)["spec"]
        if not common.contains(stored.get("spec"), expected) or stored["spec"].get("initContainers") or stored["spec"].get("ephemeralContainers"):
            raise RuntimeError("The metadata fixture lost its execution barriers or limits")
        state = stored.get("status", {})
        for field in ["containerStatuses", "initContainerStatuses", "ephemeralContainerStatuses"]:
            if any(x.get("containerID") or any(key in x.get(part, {}) for key in ["running", "terminated"] for part in ["state", "lastState"])
                   for x in state.get(field, [])):
                raise RuntimeError("The metadata fixture started a container")
        self.result.setdefault("pod_observations", []).append({
            "uid": stored["metadata"]["uid"], "resource_version": stored["metadata"]["resourceVersion"],
            "phase": state.get("phase"), "annotation_keys": sorted(stored["metadata"].get("annotations", {})),
            "container_started": False,
        })
        self.save()
        return stored

    def metadata_probe(self, name, changes, user=None, group=None, allowed=False, status=True):
        before = self.observe_status_pod()
        meta = before["metadata"]
        # The node roles can update status but cannot GET pods/status. Send a
        # direct PUT; oc patch performs that extra read before its PATCH.
        # Retain the observed UID and resource version in the complete object.
        path = common.path(before) + ("/status" if status else "")
        command = ["replace", "--raw=" + path + "?dryRun=All&fieldValidation=Strict", "-f", "-"]
        if group:
            command += ["--as-group=" + group, "--as-group=system:authenticated"]
        desired = copy.deepcopy(before)
        annotations = desired["metadata"].setdefault("annotations", {})
        for key, value in changes.items():
            if value is None:
                annotations.pop(key, None)
            else:
                annotations[key] = value
        policy = None if allowed else common.CONTROL + ".widget-queue.pods.tenant"
        result = self.probe(name, command, obj=desired, user=user, allowed=allowed, policy=policy)
        if allowed:
            admitted = json.loads(result.stdout)
            if admitted["metadata"]["uid"] != meta["uid"]:
                raise RuntimeError("Metadata dry run changed Pod identity")
            for key, value in changes.items():
                if admitted["metadata"].get("annotations", {}).get(key) != value:
                    raise RuntimeError("Metadata dry run did not return the requested value")
        after = self.observe_status_pod()
        for key in changes:
            if before["metadata"].get("annotations", {}).get(key) != after["metadata"].get("annotations", {}).get(key):
                raise RuntimeError("Metadata dry run left a stored change")

    def network_metadata(self):
        node = self.run(["get", "node", self.args.node, "-o", "json"])
        if node.returncode:
            raise RuntimeError("The selected fixture node cannot be read")
        node = json.loads(node.stdout)
        if node["metadata"]["name"] != self.args.node or not any(c["type"] == "Ready" and c["status"] == "True" for c in node.get("status", {}).get("conditions", [])):
            raise RuntimeError("The selected fixture node is not ready")
        namespace = next(x["metadata"]["name"] for x in self.created
                         if x["kind"] == "Namespace" and x["metadata"]["name"].startswith(common.PREFIX))
        account = common.account_name(common.CONTROL, "owner-1")
        labels = common.namespace(namespace, common.CONTROL, "owner-1")["metadata"]["labels"]
        labels = {k: v for k, v in labels.items() if not k.startswith("pod-security.")}
        metadata = {"name": "stego-allocation", "namespace": namespace, "labels": labels}
        self.create({"apiVersion": "v1", "kind": "ResourceQuota", "metadata": metadata,
                     "spec": {"hard": {"pods": "1", "limits.cpu": "1", "limits.memory": "256Mi", "limits.ephemeral-storage": "128Mi", "requests.storage": "1Gi"}}}, common.actor(common.CONTROL))
        self.create({"apiVersion": "networking.k8s.io/v1", "kind": "NetworkPolicy", "metadata": metadata,
                     "spec": {"podSelector": {}, "policyTypes": ["Ingress", "Egress"]}}, common.actor(common.CONTROL))
        self.create({"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "Role",
                     "metadata": {"namespace": namespace, "name": "status-writer"},
                     "rules": [{"apiGroups": [""], "resources": ["pods", "pods/status"], "verbs": ["get", "update"]}]})
        self.create({"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "RoleBinding",
                     "metadata": {"namespace": namespace, "name": "status-writer"},
                     "roleRef": {"apiGroup": "rbac.authorization.k8s.io", "kind": "Role", "name": "status-writer"},
                     "subjects": [{"kind": "ServiceAccount", "namespace": common.CONTROL, "name": "pod-writer"}]})
        self.create_status_pod(namespace, account)
        end = time.monotonic() + 45
        while True:
            stored = self.observe_status_pod()
            value = stored["metadata"].get("annotations", {}).get(OVN)
            if value:
                json.loads(value)
                break
            if time.monotonic() >= end:
                raise RuntimeError("The real OVN controller did not publish Pod network metadata")
            time.sleep(1)
        self.result["real_ovn_metadata_observed"] = True
        self.save()
        self.metadata_probe("assigned OVN node can write metadata", {OVN: value + " "},
                            "system:ovn-node:" + self.args.node, "system:ovn-nodes", True)
        self.metadata_probe("assigned Multus node can write metadata", {MULTUS: "[]"},
                            "system:multus:" + self.args.node, "system:multus", True)
        writer = "system:serviceaccount:" + common.CONTROL + ":pod-writer"
        self.metadata_probe("workload can preserve network output", {"example.test/workload": "updated"}, writer, allowed=True, status=False)
        self.metadata_probe("workload cannot add network output through status", {MULTUS: "[]"}, writer)
        self.metadata_probe("workload cannot add network output through main API", {MULTUS: "[]"}, writer, status=False)
        self.metadata_probe("operator cannot bypass network ownership", {MULTUS: "[]"})
        self.metadata_probe("status cannot add runtime override", {"io.katacontainers.config.hypervisor.shared_fs": "none"}, writer)
        self.metadata_probe("status cannot request another network", {"k8s.v1.cni.cncf.io/networks": "other"}, writer)
        self.metadata_probe("other Multus node cannot write metadata", {MULTUS: "[]"},
                            "system:multus:stego-other-node", "system:multus")
        self.result["network_metadata_admission_checked"] = True
        self.observe_status_pod()

    def cleanup(self):
        # Delete the recorded Pod first. Its spec has a zero grace period.
        # Recover an uncertain create from its intent before deleting policies.
        self.deadline = time.monotonic() + 180
        pod_errors = []
        for intent in getattr(self, "intents", []):
            if intent["object"]["kind"] == "Pod" and not intent["resolved"]:
                try:
                    self.recover_intent(intent)
                except Exception as error:
                    pod_errors.append(str(error))
        for obj in self.created:
            if obj["kind"] == "Pod":
                try:
                    self.remove(obj)
                except Exception as error:
                    pod_errors.append(str(error))
        self.result["pod_cleanup_errors"] = pod_errors
        self.save()
        if pod_errors:
            self.result["cleanup_complete"] = False
            self.save()
            raise RuntimeError("Pod cleanup failed; retain the policies and test lease")
        super().cleanup()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ["oc", "context", "node"]:
        parser.add_argument("--" + name, required=True)
    for name in ["manifest", "peer-manifest", "evidence"]:
        parser.add_argument("--" + name, type=Path, required=True)
    args = parser.parse_args()
    check = Check(args)
    try:
        check.install()
        check.exercise()
        check.network_metadata()
        check.result["checks_passed"] = True
    except Exception as error:
        check.result["failure"] = str(error)
        raise
    finally:
        check.cleanup()
        check.save()


if __name__ == "__main__":
    main()
