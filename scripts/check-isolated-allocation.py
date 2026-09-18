#!/usr/bin/env python3
"""Check isolated-runtime admission with server dry-run requests only.

Use the exact qualified manifests. Hold the shared live-test lease. This check
installs no runtime handler and stores no Pod. Keep the source copy unchanged
until the check and UID-based cleanup finish.
"""
import argparse
import copy
import importlib.util
import json
from pathlib import Path
import uuid

spec = importlib.util.spec_from_file_location("pods", Path(__file__).with_name("check-allocation-pods.py"))
pods = importlib.util.module_from_spec(spec)
spec.loader.exec_module(pods)
common = pods.common
common.CONTROL = "stego-isolated-control-ci"
common.PEER = "stego-isolated-peer-ci"
common.PREFIX = "stego-isolated-ci-"
pods.UNRELATED = "stego-isolated-unrelated-ci"
HELPER = "registry.invalid/stego/helper@sha256:" + "b" * 64


def isolated_pod(namespace, account):
    obj = pods.pod(namespace, account)
    obj["spec"]["securityContext"].pop("runAsNonRoot")
    workspace = copy.deepcopy(obj["spec"]["containers"][0])
    workspace["name"] = "workspace-copy"
    # Keep a root workspace helper and an ordinary emptyDir. There is no mutation.
    workspace["securityContext"] = {"runAsUser": 0}
    helper = copy.deepcopy(workspace)
    helper.update(name="network-helper", image=HELPER)
    helper["securityContext"] = {"runAsUser": 0, "allowPrivilegeEscalation": False,
                                 "capabilities": {"drop": ["ALL"], "add": ["NET_ADMIN", "NET_RAW"]}}
    obj["spec"]["initContainers"] = [workspace, helper]
    obj["spec"]["volumes"] = [{"name": "socket-state", "emptyDir": {}}]
    return obj


class Check(pods.Check):
    allocation_prefixes = (common.PREFIX, pods.UNRELATED)

    def grant_scc(self, namespace, control):
        labels = {k: v for k, v in namespace["metadata"]["labels"].items() if not k.startswith("pod-security.")}
        return self.create({"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "RoleBinding",
                            "metadata": {"namespace": namespace["metadata"]["name"],
                                         "name": "stego-" + labels["stego.dev/allocator"] + "-2", "labels": labels},
                            "roleRef": {"apiGroup": "rbac.authorization.k8s.io", "kind": "ClusterRole", "name": "system:openshift:scc:privileged"},
                            "subjects": [{"kind": "ServiceAccount", "namespace": namespace["metadata"]["name"],
                                          "name": namespace["metadata"]["annotations"][common.ANNOTATION]}]}, common.actor(control))

    def pod_probe(self, name, obj, user=None, allowed=False, policy=None):
        result = self.probe(name, ["create", "--dry-run=server", "-f", "-", "-o", "json"], obj, user, allowed, policy)
        if allowed:
            admitted = json.loads(result.stdout)
            for field in ["runtimeClassName", "serviceAccountName"]:
                if admitted["spec"].get(field) != obj["spec"].get(field):
                    raise RuntimeError("Admission changed " + field)
            if "initContainers" in obj["spec"]:
                init = {c["name"]: c for c in admitted["spec"]["initContainers"]}
                if init["workspace-copy"]["securityContext"]["runAsUser"] != 0:
                    raise RuntimeError("Admission changed the root workspace helper")
                volumes = {v["name"]: v for v in admitted["spec"]["volumes"]}
                if volumes["socket-state"]["emptyDir"].get("medium", "") != "":
                    raise RuntimeError("Admission changed the socket volume")
        if self.get(obj) is not None:
            raise RuntimeError("The dry-run left a stored Pod")

    def exercise(self):
        for name in [pods.RUNTIME, pods.OTHER_RUNTIME]:
            self.create({"apiVersion": "node.k8s.io/v1", "kind": "RuntimeClass", "metadata": {"name": name}, "handler": "stego-admission-test-no-runtime"})
        allocated = []
        for control, owner in [(common.CONTROL, "owner-1"), (common.PEER, "owner-2")]:
            name = common.PREFIX + uuid.uuid4().hex[:8]
            namespace = common.namespace(name, control, owner)
            namespace["metadata"]["labels"]["pod-security.kubernetes.io/enforce"] = "privileged"
            self.create(namespace, common.actor(control))
            self.create(common.service_account(namespace), common.actor(control))
            self.wait_account(name, "default")
            self.grant_scc(namespace, control)
            allocated.append(namespace)
        first, peer = allocated
        namespace = first["metadata"]["name"]
        account = first["metadata"]["annotations"][common.ANNOTATION]
        policy = common.CONTROL + ".widget-queue.pods.tenant"
        writer = self.grant_writer(namespace)
        valid = isolated_pod(namespace, account)
        self.pod_probe("root workspace and declared network helper", valid, writer, allowed=True)
        self.pod_probe("peer installation retains its own rules", isolated_pod(peer["metadata"]["name"], peer["metadata"]["annotations"][common.ANNOTATION]), allowed=True)
        for field, value, label in [
            ("runtimeClassName", None, "missing runtime"),
            ("runtimeClassName", pods.OTHER_RUNTIME, "different runtime"),
            ("serviceAccountName", "default", "default account"),
            ("automountServiceAccountToken", True, "automatic token mount"),
            ("hostNetwork", True, "host network"),
            ("hostPID", True, "host process namespace"),
            ("hostIPC", True, "host IPC namespace"),
            ("volumes", [{"name": "host", "hostPath": {"path": "/"}}], "host storage"),
            ("volumes", [{"name": "remote", "nfs": {"server": "127.0.0.1", "path": "/"}}], "inline remote storage"),
            ("securityContext", {"sysctls": [{"name": "net.ipv4.ip_unprivileged_port_start", "value": "0"}]}, "sysctl change"),
        ]:
            changed = copy.deepcopy(valid)
            if value is None:
                del changed["spec"][field]
            else:
                changed["spec"][field] = value
            # Use the operator for policy denials, so SCC cannot hide a missing rule.
            self.pod_probe(label, changed, policy=policy)
        for label, change in [
            ("privileged container", {"securityContext": {"privileged": True}}),
            ("host port", {"ports": [{"containerPort": 8080, "hostPort": 8080}]}),
            ("extended device", {"resources": {"limits": {"example.test/device": "1"}}}),
            ("unmasked proc", {"securityContext": {"procMount": "Unmasked"}}),
            ("privilege escalation", {"securityContext": {"allowPrivilegeEscalation": True}}),
        ]:
            changed = copy.deepcopy(valid)
            changed["spec"]["containers"][0].update(change)
            if label == "unmasked proc":
                changed["spec"]["hostUsers"] = False
            self.pod_probe(label, changed, policy=policy)
        for field, value, label in [
            ("name", "other-helper", "capabilities under another name"),
            ("image", "registry.invalid/stego/helper@sha256:" + "c" * 64, "capabilities in another image"),
            ("securityContext", {"runAsUser": 0, "allowPrivilegeEscalation": False, "capabilities": {"drop": ["ALL"], "add": ["SYS_ADMIN"]}}, "undeclared capability"),
            ("securityContext", {"runAsUser": 0, "allowPrivilegeEscalation": False, "capabilities": {"add": ["NET_ADMIN"]}}, "capabilities without drop all"),
        ]:
            changed = copy.deepcopy(valid)
            changed["spec"]["initContainers"][1][field] = value
            self.pod_probe(label, changed, policy=policy)
        changed = copy.deepcopy(valid)
        changed["metadata"]["annotations"] = {"io.katacontainers.config.hypervisor.shared_fs": "none"}
        self.pod_probe("runtime annotation override", changed, policy=policy)
        for label, annotations in [
            ("unconfined legacy seccomp", {"seccomp.security.alpha.kubernetes.io/pod": "unconfined"}),
            ("local legacy seccomp", {"seccomp.security.alpha.kubernetes.io/pod": "localhost/other"}),
            ("container legacy seccomp", {"container.seccomp.security.alpha.kubernetes.io/check": "unconfined"}),
            ("undeclared application annotation", {"example.test/other": "one"}),
        ]:
            changed["metadata"]["annotations"] = annotations
            self.pod_probe(label, changed, policy=policy)
        changed["metadata"]["annotations"] = {"seccomp.security.alpha.kubernetes.io/pod": "runtime/default"}
        self.pod_probe("matching platform seccomp annotation", changed, writer, allowed=True)
        changed["metadata"]["annotations"] = {"example.test/workload": "one"}
        self.pod_probe("declared application annotation", changed, writer, allowed=True)
        self.probe("namespace mode is immutable", ["patch", "namespace", namespace, "--type=merge", "--dry-run=server", "-p",
                   json.dumps({"metadata": {"labels": {"pod-security.kubernetes.io/enforce": "restricted"}}})],
                   user=common.actor(common.CONTROL), policy=common.CONTROL + ".widget-queue.ownership")
        self.create(common.namespace(pods.UNRELATED))
        self.wait_account(pods.UNRELATED, "default")
        unrelated = pods.pod(pods.UNRELATED, "default")
        del unrelated["spec"]["runtimeClassName"]
        self.pod_probe("unrelated namespace retains restricted rules", unrelated, allowed=True)
        self.pod_probe("writer cannot access unrelated namespace", unrelated, writer)
        for verb, resource in [("create", "serviceaccounts"), ("patch", "validatingadmissionpolicies"), ("create", "runtimeclasses")]:
            result = self.run(["auth", "can-i", verb, resource, "-n", namespace], user=writer)
            if (result.returncode, result.stdout.strip()) != (1, "no"):
                raise RuntimeError("The Pod writer has an unexpected capability")
            self.result.setdefault("denied_capabilities", []).append({"verb": verb, "resource": resource})
        self.result.update(dry_run_only=True, runtime_handler_installed=False, namespace_security="isolated-runtime")


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
