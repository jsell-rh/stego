#!/usr/bin/env python3
"""Check generated Pod restrictions with server dry-run requests.

Hold the shared live-test lease. Use the two manifests from the exact qualified
source. This check creates no Pod and installs no runtime handler. All created
resources have a UID journal and are removed while their guards remain active.
"""

import argparse
import copy
import importlib.util
import json
from pathlib import Path
import time
import uuid

spec = importlib.util.spec_from_file_location("accounts", Path(__file__).with_name("check-allocation-account-identity.py"))
common = importlib.util.module_from_spec(spec)
spec.loader.exec_module(common)
common.CONTROL = "stego-pods-control-ci"
common.PEER = "stego-pods-peer-ci"
common.PREFIX = "stego-pods-ci-"
common.KINDS["RuntimeClass"] = ("/apis/node.k8s.io/v1/runtimeclasses", False)
common.KINDS["Pod"] = ("/api/v1/namespaces/{namespace}/pods", True)
RUNTIME = "stego-pods-test-runtime"
OTHER_RUNTIME = "stego-pods-other-runtime"
UNRELATED = "stego-pods-unrelated-ci"


def pod(namespace, account):
    return {
        "apiVersion": "v1", "kind": "Pod",
        "metadata": {"namespace": namespace, "name": "admission-check"},
        "spec": {
            "runtimeClassName": RUNTIME, "serviceAccountName": account,
            "automountServiceAccountToken": False, "restartPolicy": "Never",
            "securityContext": {"runAsNonRoot": True, "seccompProfile": {"type": "RuntimeDefault"}},
            "containers": [{"name": "check", "image": "registry.invalid/stego/admission-only@sha256:" + "a" * 64,
                            "securityContext": {"allowPrivilegeEscalation": False, "capabilities": {"drop": ["ALL"]}},
                            "resources": {"requests": {"cpu": "10m", "memory": "16Mi", "ephemeral-storage": "16Mi"},
                                          "limits": {"cpu": "10m", "memory": "16Mi", "ephemeral-storage": "16Mi"}}}],
        },
    }


class Check(common.Check):
    policy_count = 8
    allocation_prefixes = (common.PREFIX, UNRELATED)

    def create(self, obj, user=None):
        if obj["kind"] == "Pod":
            raise RuntimeError("Pod persistence is forbidden in the admission check")
        return super().create(obj, user)

    def pod_probe(self, name, obj, user=None, allowed=False, policy=None):
        command = ["create", "--dry-run=server", "-f", "-", "-o", "json"]
        result = self.probe(name, command, obj, user, allowed, policy)
        if allowed:
            admitted = json.loads(result.stdout)
            if admitted["spec"].get("runtimeClassName") != obj["spec"].get("runtimeClassName"):
                raise RuntimeError("Admission changed the selected runtime")
            if admitted["spec"].get("serviceAccountName") != obj["spec"].get("serviceAccountName"):
                raise RuntimeError("Admission changed the selected account")
        # Dry-run must not leave a stored object, including on a denied request.
        if self.get(obj) is not None:
            raise RuntimeError("The dry-run left a stored Pod")

    def wait_account(self, namespace, name):
        obj = {"apiVersion": "v1", "kind": "ServiceAccount", "metadata": {"namespace": namespace, "name": name}}
        end = time.monotonic() + 20
        while self.get(obj) is None:
            if time.monotonic() >= end:
                raise RuntimeError("The fixture account was not ready")
            time.sleep(1)

    def grant_writer(self, namespace):
        name = "pod-writer"
        self.create({"apiVersion": "v1", "kind": "ServiceAccount", "metadata": {"namespace": common.CONTROL, "name": name}, "automountServiceAccountToken": False})
        self.create({"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "Role", "metadata": {"namespace": namespace, "name": name},
                     "rules": [{"apiGroups": [""], "resources": ["pods"], "verbs": ["create"]}]})
        self.create({"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "RoleBinding", "metadata": {"namespace": namespace, "name": name},
                     "roleRef": {"apiGroup": "rbac.authorization.k8s.io", "kind": "Role", "name": name},
                     "subjects": [{"kind": "ServiceAccount", "namespace": common.CONTROL, "name": name}]})
        return "system:serviceaccount:" + common.CONTROL + ":" + name

    def exercise(self):
        for name in [RUNTIME, OTHER_RUNTIME]:
            self.create({"apiVersion": "node.k8s.io/v1", "kind": "RuntimeClass", "metadata": {"name": name}, "handler": "stego-admission-test-no-runtime"})
        allocated = []
        for control, owner in [(common.CONTROL, "owner-1"), (common.PEER, "owner-2")]:
            name = common.PREFIX + uuid.uuid4().hex[:8]
            namespace = common.namespace(name, control, owner)
            self.create(namespace, common.actor(control))
            self.create(common.service_account(namespace), common.actor(control))
            self.wait_account(name, "default")
            allocated.append(namespace)
        first, peer = allocated
        namespace = first["metadata"]["name"]
        account = first["metadata"]["annotations"][common.ANNOTATION]
        policy = common.CONTROL + ".widget-queue.pods.tenant"
        writer = self.grant_writer(namespace)
        valid = pod(namespace, account)
        self.pod_probe("declared runtime and allocated account", valid, writer, allowed=True)
        self.pod_probe("peer installation retains its own policy", pod(peer["metadata"]["name"], peer["metadata"]["annotations"][common.ANNOTATION]), allowed=True)
        for field, value, label in [
            ("runtimeClassName", None, "missing runtime"),
            ("runtimeClassName", OTHER_RUNTIME, "different runtime"),
            ("serviceAccountName", "default", "default account"),
            ("serviceAccountName", None, "missing account"),
            ("automountServiceAccountToken", True, "automatic token mount"),
            ("automountServiceAccountToken", None, "implicit token setting"),
        ]:
            changed = copy.deepcopy(valid)
            if value is None:
                del changed["spec"][field]
            else:
                changed["spec"][field] = value
            self.pod_probe(label, changed, writer, policy=policy)
        changed = copy.deepcopy(valid)
        del changed["spec"]["runtimeClassName"]
        changed["metadata"]["labels"] = {"stego.dev/allocation-profile": "other", "stego.dev/allocator": "0" * 32}
        self.pod_probe("Pod labels cannot escape namespace policy", changed, writer, policy=policy)
        self.create(common.namespace(UNRELATED))
        self.wait_account(UNRELATED, "default")
        unrelated = pod(UNRELATED, "default")
        del unrelated["spec"]["runtimeClassName"]
        self.pod_probe("unrelated namespace retains its rules", unrelated, allowed=True)
        self.pod_probe("writer cannot access unrelated namespace", unrelated, writer)
        for verb, resource in [("create", "serviceaccounts"), ("patch", "validatingadmissionpolicies"), ("create", "runtimeclasses")]:
            result = self.run(["auth", "can-i", verb, resource, "-n", namespace], user=writer)
            if (result.returncode, result.stdout.strip()) != (1, "no"):
                raise RuntimeError("The Pod writer has an unexpected capability")
            self.result.setdefault("denied_capabilities", []).append({"verb": verb, "resource": resource})
            self.save()
        self.result.update(dry_run_only=True, runtime_handler_installed=False, namespace_security="restricted")


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
