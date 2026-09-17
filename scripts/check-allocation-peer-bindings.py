#!/usr/bin/env python3
"""Check generated grants between related allocations without creating Pods.

Use related/manifest.json and peer/manifest.json from the same account CI run.
The caller must hold the shared test lease and verify cleanup before this check.
"""

import argparse
import base64
import copy
import hashlib
import importlib.util
import json
from pathlib import Path
import uuid

spec = importlib.util.spec_from_file_location("account_identity", Path(__file__).with_name("check-allocation-account-identity.py"))
common = importlib.util.module_from_spec(spec)
spec.loader.exec_module(common)
DESTINATION_PREFIX = "stego-account-jobs-ci-"


def account_name(owner, alias):
    data = ["stego-allocation-service-account-v1", common.CONTROL, "widget-queue", "example.test/owner", owner, alias]
    tag = hashlib.sha256(json.dumps(data, separators=(",", ":")).encode()).digest()[:16]
    marker = hashlib.sha256((common.CONTROL + ".widget-queue").encode()).hexdigest()[:32]
    return "sa-" + marker + "-" + base64.b32encode(tag).decode().lower().rstrip("=")


def destination(suffix, owner):
    result = common.namespace(DESTINATION_PREFIX + suffix, common.CONTROL, owner)
    result["metadata"]["labels"]["stego.dev/allocation-profile"] = "jobs"
    result["metadata"]["annotations"]["stego.dev/service-account-runner"] = account_name(owner, "runner")
    return result


class Check(common.Check):
    allocation_prefixes = (common.PREFIX, DESTINATION_PREFIX)

    def allocate(self, obj, alias):
        actor = common.actor(common.CONTROL)
        stored = self.create(obj, actor)
        self.result.setdefault("namespace_identities", []).append({"name": stored["metadata"]["name"], "uid": stored["metadata"]["uid"], "owner": stored["metadata"]["labels"]["example.test/owner"]})
        labels = {k: v for k, v in obj["metadata"]["labels"].items() if not k.startswith("pod-security.")}
        self.create({"apiVersion": "v1", "kind": "ResourceQuota", "metadata": {"name": "stego-allocation", "namespace": obj["metadata"]["name"], "labels": labels},
                     "spec": {"hard": {"pods": "1", "limits.cpu": "1", "limits.memory": "256Mi", "limits.ephemeral-storage": "128Mi", "requests.storage": "1Gi"}}}, actor)
        account = common.service_account(obj)
        account["metadata"]["name"] = obj["metadata"]["annotations"]["stego.dev/service-account-" + alias]
        self.create(account, actor)
        return stored

    def exercise(self):
        suffix = uuid.uuid4().hex[:8]
        actor = common.actor(common.CONTROL)
        src = self.allocate(common.namespace(common.PREFIX + suffix, common.CONTROL, "owner-1"), "gateway")
        dst = self.allocate(destination(suffix, "owner-1"), "runner")
        marker = hashlib.sha256((common.CONTROL + ".widget-queue").encode()).hexdigest()[:32]
        labels = {k: v for k, v in dst["metadata"]["labels"].items() if not k.startswith("pod-security.")}
        grant = {"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "RoleBinding",
                 "metadata": {"name": "stego-" + marker + "-0", "namespace": dst["metadata"]["name"], "labels": labels,
                              "annotations": {common.ANNOTATION: account_name("owner-1", "gateway")}},
                 "roleRef": {"apiGroup": "rbac.authorization.k8s.io", "kind": "ClusterRole", "name": common.CONTROL + ".widget-queue.data"},
                 "subjects": [{"kind": "ServiceAccount", "name": account_name("owner-1", "gateway"), "namespace": src["metadata"]["name"]}]}
        dry = ["create", "--dry-run=server", "-f", "-", "-o", "json"]
        policy = common.CONTROL + ".widget-queue.allocation"
        self.probe("declared related grant accepted", dry, grant, actor, allowed=True)
        mutations = {
            "different namespace suffix denied": lambda value: value["subjects"][0].update(namespace=common.PREFIX + ("0" if suffix[0] != "0" else "1") + suffix[1:]),
            "different source profile denied": lambda value: value["subjects"][0].update(namespace=dst["metadata"]["name"]),
            "different owner account denied": lambda value: value["subjects"][0].update(name=account_name("owner-2", "gateway")),
            "different account alias denied": lambda value: value["subjects"][0].update(name=account_name("owner-1", "runner")),
            "different role denied": lambda value: value["roleRef"].update(name=common.CONTROL + ".widget-queue.review"),
            "additional subject denied": lambda value: value["subjects"].append(copy.deepcopy(value["subjects"][0])),
        }
        for name, mutate in mutations.items():
            candidate = copy.deepcopy(grant)
            mutate(candidate)
            self.probe(name, dry, candidate, actor, policy=policy)
        stored_grant = self.create(grant, actor)
        self.result["retained_grant"] = {"name": stored_grant["metadata"]["name"], "uid": stored_grant["metadata"]["uid"]}
        for ns in [dst["metadata"]["name"], common.CONTROL]:
            self.create({"apiVersion": "v1", "kind": "Secret", "metadata": {"name": "public-probe", "namespace": ns}, "stringData": {"value": "public-test-data"}})
        identity = "system:serviceaccount:" + src["metadata"]["name"] + ":" + account_name("owner-1", "gateway")
        read = ["get", "secret", "public-probe", "-n", dst["metadata"]["name"], "-o", "name"]
        self.probe("declared source reads destination", read, user=identity, allowed=True)
        self.probe("source cannot read control namespace", ["get", "secret", "public-probe", "-n", common.CONTROL, "-o", "name"], user=identity)
        self.probe("imported account annotation cannot change", ["patch", "namespace", dst["metadata"]["name"], "--type=merge", "--dry-run=server", "-p",
                   json.dumps({"metadata": {"annotations": {common.ANNOTATION: account_name("owner-2", "gateway")}}})],
                   user=actor, policy=common.CONTROL + ".widget-queue.ownership")
        self.remove(src)
        replacement = self.allocate(common.namespace(src["metadata"]["name"], common.CONTROL, "owner-2"), "gateway")
        self.probe("replacement owner cannot use retained grant", read,
                   user="system:serviceaccount:" + src["metadata"]["name"] + ":" + account_name("owner-2", "gateway"))
        self.remove(replacement)
        recovered = self.allocate(common.namespace(src["metadata"]["name"], common.CONTROL, "owner-1"), "gateway")
        assert len({src["metadata"]["uid"], replacement["metadata"]["uid"], recovered["metadata"]["uid"]}) == 3
        self.probe("original owner recovers related grant", read, user=identity, allowed=True)
        final_grant = self.get(stored_grant)
        assert final_grant["metadata"]["uid"] == stored_grant["metadata"]["uid"] and final_grant["subjects"] == stored_grant["subjects"]
        self.result["scope"] = "Live admission and RBAC through impersonation, with namespace replacement. No Pod token authentication or Sandbox execution was tested."


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--oc", required=True)
    parser.add_argument("--context", required=True)
    parser.add_argument("--manifest", type=Path, required=True)
    parser.add_argument("--peer-manifest", type=Path, required=True)
    parser.add_argument("--evidence", type=Path, required=True)
    check = Check(parser.parse_args())
    try:
        check.install()
        check.exercise()
        check.result["checks_passed"] = True
    except Exception as error:
        check.result["failure"] = str(error)
        raise
    finally:
        check.cleanup()
        check.save()


if __name__ == "__main__":
    main()
