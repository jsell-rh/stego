#!/usr/bin/env python3
"""Check generated account admission and namespace reuse on a dedicated cluster.

Use the two manifests from the Allocation account identity CI artifact. This
check installs only policies, roles, and ServiceAccounts. It creates no Pod.
The caller must serialize this check with other live cluster tests.
"""

import argparse
import base64
import copy
import hashlib
import json
from pathlib import Path
import subprocess
import time
import uuid


KINDS = {
    "Namespace": ("/api/v1/namespaces", False),
    "ServiceAccount": ("/api/v1/namespaces/{namespace}/serviceaccounts", True),
    "ConfigMap": ("/api/v1/namespaces/{namespace}/configmaps", True),
    "ResourceQuota": ("/api/v1/namespaces/{namespace}/resourcequotas", True),
    "Role": ("/apis/rbac.authorization.k8s.io/v1/namespaces/{namespace}/roles", True),
    "RoleBinding": ("/apis/rbac.authorization.k8s.io/v1/namespaces/{namespace}/rolebindings", True),
    "ClusterRole": ("/apis/rbac.authorization.k8s.io/v1/clusterroles", False),
    "ClusterRoleBinding": ("/apis/rbac.authorization.k8s.io/v1/clusterrolebindings", False),
    "ValidatingAdmissionPolicy": ("/apis/admissionregistration.k8s.io/v1/validatingadmissionpolicies", False),
    "ValidatingAdmissionPolicyBinding": ("/apis/admissionregistration.k8s.io/v1/validatingadmissionpolicybindings", False),
}
CONTROL = "stego-allocation-accounts-ci"
PEER = "stego-allocation-accounts-peer-ci"
PREFIX = "stego-accounts-ci-"
ANNOTATION = "stego.dev/service-account-gateway"


def path(obj):
    collection, _ = KINDS[obj["kind"]]
    return collection.format(namespace=obj["metadata"].get("namespace", "")) + "/" + obj["metadata"]["name"]


def account_name(control, owner):
    marker = hashlib.sha256((control + ".widget-queue").encode()).hexdigest()[:32]
    data = ["stego-allocation-service-account-v1", control, "widget-queue", "example.test/owner", owner, "gateway"]
    digest = hashlib.sha256(json.dumps(data, separators=(",", ":")).encode()).digest()[:16]
    return "sa-" + marker + "-" + base64.b32encode(digest).decode().lower().rstrip("=")


def namespace(name, control=None, owner=None):
    labels = {"pod-security.kubernetes.io/enforce": "restricted"}
    obj = {"apiVersion": "v1", "kind": "Namespace", "metadata": {"name": name, "labels": labels}}
    if control:
        labels.update({
            "stego.dev/allocator": hashlib.sha256((control + ".widget-queue").encode()).hexdigest()[:32],
            "stego.dev/allocation-profile": "tenant", "example.test/owner": owner,
            "app.kubernetes.io/managed-by": "widget",
        })
        obj["metadata"]["annotations"] = {ANNOTATION: account_name(control, owner)}
    return obj


def service_account(ns):
    return {
        "apiVersion": "v1", "kind": "ServiceAccount",
        "metadata": {"namespace": ns["metadata"]["name"],
                     "name": ns["metadata"]["annotations"][ANNOTATION],
                     "labels": {k: v for k, v in ns["metadata"]["labels"].items() if not k.startswith("pod-security.")}},
        "automountServiceAccountToken": False,
    }


def actor(control):
    return "system:serviceaccount:" + control + ":widget-queue"


class Check:
    def __init__(self, args):
        self.args = args
        self.created = []
        self.probes = []
        self.deadline = time.monotonic() + 600
        self.result = {"complete": False, "pods_created": 0, "probes": self.probes}
        args.evidence.mkdir(parents=True, exist_ok=False)

    def save(self):
        (self.args.evidence / "created.json").write_text(json.dumps(self.created, indent=2) + "\n")
        (self.args.evidence / "result.json").write_text(json.dumps(self.result, indent=2) + "\n")

    def run(self, args, obj=None, user=None):
        if time.monotonic() >= self.deadline:
            raise RuntimeError("The account check time limit expired")
        command = [self.args.oc, "--context=" + self.args.context, "--request-timeout=10s"]
        if user:
            command += ["--as=" + user]
        return subprocess.run(command + args, input=json.dumps(obj) if obj is not None else None,
                              capture_output=True, text=True, timeout=15)

    def get(self, obj):
        result = self.run(["get", "--raw=" + path(obj)])
        if result.returncode:
            if "(NotFound)" in result.stderr:
                return None
            raise RuntimeError("A resource read failed: " + path(obj))
        return json.loads(result.stdout)

    def create(self, obj, user=None):
        if self.get(obj) is not None:
            raise RuntimeError("A test resource already exists: " + path(obj))
        result = self.run(["create", "-f", "-", "-o", "json"], obj, user)
        if result.returncode:
            raise RuntimeError("Resource creation failed: " + path(obj) + ": " + result.stderr[:2048])
        stored = json.loads(result.stdout)
        self.created.append({"apiVersion": stored["apiVersion"], "kind": stored["kind"],
                             "metadata": {k: stored["metadata"][k] for k in ["name", "namespace", "uid"] if k in stored["metadata"]}})
        self.save()
        return stored

    def probe(self, name, command, obj=None, user=None, allowed=False, policy=None):
        result = self.run(command, obj, user)
        # These probes use only fixed public test objects. Retain a bounded
        # response before classification so a failed gate remains inspectable.
        self.result.setdefault("probe_observations", []).append({
            "name": name, "expected_allowed": allowed, "policy": policy,
            "returncode": result.returncode, "stderr": result.stderr[:4096],
            "stderr_truncated": len(result.stderr) > 4096,
        })
        self.save()
        if (result.returncode == 0) != allowed:
            raise RuntimeError("Unexpected probe result: " + name + ": " + result.stderr[:2048])
        if not allowed:
            policies = [policy] if isinstance(policy, str) else policy
            if policies:
                # Kubernetes can report a policy denial as Invalid. When
                # equivalent guards overlap, either named guard can reject.
                if not any("ValidatingAdmissionPolicy '" + item + "' with binding '" + item + "' denied request:" in result.stderr for item in policies):
                    raise RuntimeError("The expected admission policy did not reject: " + name)
            elif "forbidden" not in result.stderr.lower():
                raise RuntimeError("Probe did not fail through access control: " + name)
        self.probes.append({"name": name, "allowed": allowed, "policy": policy})
        self.save()
        print(json.dumps(self.probes[-1]), flush=True)
        return result

    def remove(self, obj):
        stored = self.get(obj)
        if stored is None:
            return
        if stored["metadata"]["uid"] != obj["metadata"]["uid"]:
            raise RuntimeError("A test resource identity changed: " + path(obj))
        options = {"apiVersion": "v1", "kind": "DeleteOptions", "preconditions": {"uid": obj["metadata"]["uid"]}}
        result = self.run(["delete", "--raw=" + path(obj), "-f", "-"], options)
        if result.returncode:
            raise RuntimeError("Test resource deletion failed: " + path(obj))
        end = min(self.deadline, time.monotonic() + 60)
        while self.get(obj) is not None:
            if time.monotonic() >= end:
                raise RuntimeError("Test resource deletion did not finish: " + path(obj))
            time.sleep(1)

    def install(self):
        manifests = []
        hashes = {}
        for filename, control in [(self.args.manifest, CONTROL), (self.args.peer_manifest, PEER)]:
            raw = filename.read_bytes()
            hashes[control] = hashlib.sha256(raw).hexdigest()
            document = json.loads(raw)
            assert document["kind"] == "List"
            items = [item for item in document["items"] if item["kind"] not in ["Deployment", "NetworkPolicy"]]
            assert len([item for item in items if item["kind"] == "ValidatingAdmissionPolicy"]) == 6
            assert len([item for item in items if item["kind"] == "ValidatingAdmissionPolicyBinding"]) == 6
            for item in items:
                assert item["kind"] in ["ValidatingAdmissionPolicy", "ValidatingAdmissionPolicyBinding", "ClusterRole", "ClusterRoleBinding", "ServiceAccount"]
                if item["kind"] == "ServiceAccount":
                    assert item["metadata"]["namespace"] == control and item["metadata"]["name"] == "widget-queue"
                else:
                    assert item["metadata"]["name"].startswith(control + ".widget-queue")
                if item["kind"] == "ValidatingAdmissionPolicy":
                    assert item["spec"]["failurePolicy"] == "Fail"
            manifests.extend(items)
        # Do not alter any existing installation, even if its source matches.
        for obj in [namespace(CONTROL), namespace(PEER)] + manifests:
            if self.get(obj) is not None:
                raise RuntimeError("The dedicated account fixture is not empty: " + path(obj))
        self.result["manifest_sha256"] = hashes
        self.create(namespace(CONTROL))
        self.create(namespace(PEER))
        for item in manifests:
            self.create(item)
        end = time.monotonic() + 30
        policies = [item for item in manifests if item["kind"] == "ValidatingAdmissionPolicy"]
        while True:
            ready = True
            for policy in policies:
                stored = self.get(policy)
                status = stored.get("status", {})
                if status.get("typeChecking", {}).get("expressionWarnings"):
                    raise RuntimeError("Generated CEL has expression warnings: " + policy["metadata"]["name"])
                ready = ready and status.get("observedGeneration") == stored["metadata"]["generation"] and "typeChecking" in status
            if ready:
                break
            if time.monotonic() >= end:
                raise RuntimeError("Generated policy type checks did not finish")
            time.sleep(1)
        self.result["policies_type_checked"] = len(policies)

    def exercise(self):
        dry = ["create", "--dry-run=server", "-f", "-", "-o", "json"]
        name = PREFIX + uuid.uuid4().hex[:8]
        owner1, owner2 = "owner-1", "owner-2"
        first = namespace(name, CONTROL, owner1)
        primary = actor(CONTROL)
        peer = actor(PEER)
        outsider = "system:serviceaccount:" + CONTROL + ":ordinary"
        self.create({"apiVersion": "v1", "kind": "ServiceAccount", "metadata": {"name": "ordinary", "namespace": CONTROL}, "automountServiceAccountToken": False})
        rule = {"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "ClusterRole", "metadata": {"name": CONTROL + ".ordinary"},
                "rules": [{"apiGroups": [""], "resources": ["namespaces"], "verbs": ["create"]}]}
        self.create(rule)
        self.create({"apiVersion": rule["apiVersion"], "kind": "ClusterRoleBinding", "metadata": {"name": rule["metadata"]["name"]},
                     "roleRef": {"apiGroup": "rbac.authorization.k8s.io", "kind": "ClusterRole", "name": rule["metadata"]["name"]},
                     "subjects": [{"kind": "ServiceAccount", "namespace": CONTROL, "name": "ordinary"}]})
        reservation_policies = [control + ".widget-queue.namespace-reservations" for control in [CONTROL, PEER]]
        policy = reservation_policies
        self.probe("ordinary explicit reserved namespace", dry, namespace(name), outsider, policy=policy)
        generated = namespace(name)
        del generated["metadata"]["name"]
        generated["metadata"]["generateName"] = PREFIX
        self.probe("ordinary reserved generateName", dry, generated, outsider, policy=policy)
        generated["metadata"]["generateName"] = "stego-unrelated-account-check-"
        self.probe("unrelated generateName remains available", dry, generated, outsider, allowed=True)
        self.probe("declared primary namespace", dry, first, primary, allowed=True)
        stored_first = self.create(first, primary)
        quota = {"apiVersion": "v1", "kind": "ResourceQuota", "metadata": {"namespace": name, "name": "stego-allocation", "labels": first["metadata"]["labels"]},
                 "spec": {"hard": {"pods": "1", "limits.cpu": "1", "limits.memory": "256Mi", "limits.ephemeral-storage": "128Mi", "requests.storage": "1Gi"}}}
        self.create(quota, primary)
        old_account = service_account(first)
        self.probe("non-allocator account creation rejected", dry, old_account,
                   policy=CONTROL + ".widget-queue.service-accounts")
        stored_account = self.create(old_account, primary)
        old_identity = "system:serviceaccount:" + name + ":" + stored_account["metadata"]["name"]
        policy = [CONTROL + ".widget-queue.service-accounts", CONTROL + ".widget-queue.allocation"]
        forged = copy.deepcopy(old_account)
        forged["metadata"]["name"] = account_name(CONTROL, owner2)
        self.probe("foreign owner account name", dry, forged, primary, policy=policy)
        # A patch avoids an AlreadyExists response for an otherwise valid name.
        self.probe("automatic token mount rejected", ["patch", "serviceaccount", old_account["metadata"]["name"], "-n", name,
                   "--type=merge", "-p", json.dumps({"automountServiceAccountToken": True}), "--dry-run=server"], user=primary, policy=policy)
        self.probe("owner annotation replacement rejected", ["patch", "namespace", name, "--type=merge", "--dry-run=server", "-p",
                   json.dumps({"metadata": {"annotations": {ANNOTATION: account_name(CONTROL, owner2)}}})], user=primary,
                   policy=CONTROL + ".widget-queue.ownership")
        grant_name = "retained-grant"
        self.create({"apiVersion": "v1", "kind": "ConfigMap", "metadata": {"namespace": CONTROL, "name": grant_name}, "data": {"proof": "account-identity"}})
        self.create({"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "Role", "metadata": {"namespace": CONTROL, "name": grant_name},
                     "rules": [{"apiGroups": [""], "resources": ["configmaps"], "resourceNames": [grant_name], "verbs": ["get"]}]})
        binding = self.create({"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "RoleBinding", "metadata": {"namespace": CONTROL, "name": grant_name},
                               "roleRef": {"apiGroup": "rbac.authorization.k8s.io", "kind": "Role", "name": grant_name},
                               "subjects": [{"kind": "ServiceAccount", "namespace": name, "name": old_account["metadata"]["name"]}]})
        read = ["get", "configmap", grant_name, "-n", CONTROL, "-o", "json"]
        self.probe("original owner can use retained grant", read, user=old_identity, allowed=True)
        self.remove(stored_first)
        self.probe("unmarked namespace reuse rejected", dry, namespace(name), outsider, policy=reservation_policies)
        recovered = self.create(first, primary)
        self.create(quota, primary)
        recovered_account = self.create(old_account, primary)
        assert recovered["metadata"]["uid"] != stored_first["metadata"]["uid"]
        assert recovered_account["metadata"]["uid"] != stored_account["metadata"]["uid"]
        self.probe("same owner recovers its retained grant", read, user=old_identity, allowed=True)
        self.remove(recovered)
        second = namespace(name, CONTROL, owner2)
        stored_second = self.create(second, primary)
        assert stored_second["metadata"]["uid"] != stored_first["metadata"]["uid"]
        quota["metadata"]["labels"] = second["metadata"]["labels"]
        self.create(quota, primary)
        new_account = service_account(second)
        self.create(new_account, primary)
        self.probe("previous owner account recreation rejected", dry, old_account, primary, policy=policy)
        self.probe("replacement owner cannot use retained grant", read,
                   user="system:serviceaccount:" + name + ":" + new_account["metadata"]["name"])
        retained = self.get(binding)
        assert retained["metadata"]["uid"] == binding["metadata"]["uid"] and retained["subjects"] == binding["subjects"]
        self.remove(stored_second)
        third = namespace(name, PEER, owner1)
        stored_third = self.create(third, peer)
        quota["metadata"]["labels"] = third["metadata"]["labels"]
        self.create(quota, peer)
        peer_account = service_account(third)
        self.create(peer_account, peer)
        foreign = service_account(third)
        foreign["metadata"]["name"] = old_account["metadata"]["name"]
        self.probe("previous installation account name rejected", dry, foreign, peer, policy=[CONTROL + ".widget-queue.account-issuers", PEER + ".widget-queue.service-accounts", PEER + ".widget-queue.allocation"])
        del foreign["metadata"]["name"]
        foreign["metadata"]["generateName"] = old_account["metadata"]["name"] + "-"
        self.probe("previous installation generateName rejected", dry, foreign, peer, policy=[CONTROL + ".widget-queue.account-issuers", PEER + ".widget-queue.service-accounts", PEER + ".widget-queue.allocation"])
        self.probe("peer installation cannot use retained grant", read,
                   user="system:serviceaccount:" + name + ":" + peer_account["metadata"]["name"])
        retained = self.get(binding)
        assert retained["metadata"]["uid"] == binding["metadata"]["uid"] and retained["subjects"] == binding["subjects"]
        # An operator can allocate the name to another installation without
        # either tested installation's owner-account rules. This isolates the
        # issuer guard: an overlapping owner guard cannot supply the denial.
        self.remove(stored_third)
        foreign_namespace = namespace(name, "stego-allocation-accounts-foreign-ci", owner1)
        del foreign_namespace["metadata"]["annotations"]
        self.create(foreign_namespace)
        literal_account = {"apiVersion": "v1", "kind": "ServiceAccount",
                           "metadata": {"namespace": name, "name": "foreign-worker"},
                           "automountServiceAccountToken": False}
        self.probe("foreign literal account remains available", dry, literal_account, allowed=True)
        issuer_policies = [control + ".widget-queue.account-issuers" for control in [CONTROL, PEER]]
        self.probe("foreign profile cannot recreate previous issuer", dry, old_account, policy=issuer_policies)
        foreign_generated = copy.deepcopy(old_account)
        del foreign_generated["metadata"]["name"]
        foreign_generated["metadata"]["generateName"] = old_account["metadata"]["name"] + "-"
        self.probe("foreign profile cannot generate previous issuer", dry, foreign_generated, policy=issuer_policies)
        foreign_account = copy.deepcopy(literal_account)
        foreign_account["metadata"]["name"] = account_name("stego-allocation-accounts-foreign-ci", owner1)
        self.probe("foreign profile can use its own issuer", dry, foreign_account, allowed=True)
        self.result.update(retained_grant_unchanged=True, same_owner_recovery_allowed=True,
                           owner_reuse_denied=True, installation_reuse_denied=True,
                           issuer_guard_checked_without_owner_guards=True)

    def cleanup(self):
        self.deadline = time.monotonic() + 180
        errors = []
        # Remove allocation namespaces first, while their guards are installed.
        allocated = [obj for obj in self.created if obj["kind"] == "Namespace" and obj["metadata"]["name"].startswith(PREFIX)]
        # Reuse retains the previous UIDs in the journal. Only the last UID is live.
        live = {path(obj): obj for obj in self.created}
        for obj in allocated:
            if live[path(obj)] is not obj:
                continue
            try:
                self.remove(obj)
            except Exception as error:
                errors.append(str(error))
        for obj in reversed(list(live.values())):
            if obj["metadata"].get("namespace", "").startswith(PREFIX) or obj in allocated:
                continue
            try:
                self.remove(obj)
            except Exception as error:
                errors.append(str(error))
        for obj in live.values():
            try:
                if self.get(obj) is not None:
                    errors.append("Resource remains: " + path(obj))
            except Exception as error:
                errors.append(str(error))
        self.result["cleanup_errors"] = errors
        self.result["cleanup_complete"] = not errors
        self.result["complete"] = not errors and self.result.get("checks_passed", False)
        self.save()
        if errors:
            raise RuntimeError("Account fixture cleanup is incomplete; inspect the retained journal")


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
