#!/usr/bin/env python3
"""Check generated allocation policies in an existing, dedicated test namespace.

First install the manifest from TestAllocationManifests. This check creates no
Pod. It removes its own namespace and binding fixtures, including on failure.
The caller must remove the installed policies and roles after all checks.
"""

from pathlib import Path
import argparse
import json
import subprocess
import hashlib
import uuid
import time

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument("--context", required=True)
parser.add_argument("--namespace", required=True)
parser.add_argument("--evidence", type=Path, required=True)
parser.add_argument("--next-manifest", type=Path)
args = parser.parse_args()

ctx = args.context
control = args.namespace
base = control + ".widget-queue"
actor = "system:serviceaccount:" + control + ":widget-queue"
marker = hashlib.sha256(base.encode()).hexdigest()[:32]
owned = "tenant-" + uuid.uuid4().hex[:8]
foreign = "tenant-" + uuid.uuid4().hex[:8]
names = [owned, foreign]
created = []
bindings = []
evidence = []
d = args.evidence
d.mkdir(parents=True, exist_ok=True)
(d / "test-names.json").write_text(json.dumps(names))


def run(args, obj=None, as_user=None, good=True, check=True):
    cmd = ["oc", "--context=" + ctx]
    if as_user:
        cmd += ["--as=" + as_user]
    p = subprocess.run(
        cmd + args,
        input=json.dumps(obj) if obj else None,
        text=True,
        capture_output=True,
        timeout=75,
    )
    if check and (p.returncode == 0) != good:
        raise RuntimeError(" ".join(args) + "\n" + p.stdout + p.stderr)
    return p


labels = {
    "stego.dev/allocator": marker,
    "stego.dev/allocation-profile": "tenant",
    "example.test/owner": "owner-1",
    "app.kubernetes.io/managed-by": "widget",
    "pod-security.kubernetes.io/enforce": "restricted",
}


def ns(name, owned=True):
    return {
        "apiVersion": "v1",
        "kind": "Namespace",
        "metadata": {
            "name": name,
            "labels": labels.copy()
            if owned
            else {"stego.test/allocation-check": marker},
        },
    }


def check(label, args, obj=None, as_user=actor, good=False):
    r = run(args, obj, as_user, good)
    if not good and not (
        "forbidden" in r.stderr.lower() or "invalid" in r.stderr.lower()
    ):
        raise RuntimeError(
            "request did not fail through admission or authorization: " + r.stderr
        )
    evidence.append({"check": label, "allowed": good})
    print(("ALLOW " if good else "DENY ") + label, flush=True)
    return r


try:
    for policy in [base + ".allocation", base + ".ownership", base + ".resources"]:
        doc = json.loads(
            run(["get", "validatingadmissionpolicy", policy, "-o", "json"]).stdout
        )
        if doc.get("status", {}).get("observedGeneration") != doc["metadata"][
            "generation"
        ] or doc.get("status", {}).get("typeChecking", {}).get("expressionWarnings"):
            raise RuntimeError("policy was not type checked")
    check(
        "wrong namespace pattern",
        ["create", "--dry-run=server", "-f", "-"],
        ns("foreign-" + uuid.uuid4().hex[:8]),
    )
    check(
        "namespace owner spoof by another actor",
        ["create", "--dry-run=server", "-f", "-"],
        ns(owned),
        as_user=None,
    )
    check("owned namespace", ["create", "-f", "-"], ns(owned), good=True)
    created.append(owned)
    run(["create", "-f", "-"], ns(foreign, False))
    created.append(foreign)
    # Bound the foreign test namespace before tests. It receives no workloads.
    for name in names:
        quota = {
            "apiVersion": "v1",
            "kind": "ResourceQuota",
            "metadata": {
                "name": "stego-allocation",
                "namespace": name,
                "labels": {
                    k: v for k, v in labels.items() if not k.startswith("pod-security.")
                },
            },
            "spec": {
                "hard": {
                    "pods": "1",
                    "limits.cpu": "1",
                    "limits.memory": "256Mi",
                    "limits.ephemeral-storage": "128Mi",
                    "requests.storage": "1Gi",
                }
            },
        }
        if name == owned:
            check("declared quota", ["create", "-f", "-"], quota, good=True)
        else:
            run(["create", "-f", "-"], quota)
    check(
        "foreign namespace delete", ["delete", "namespace", foreign, "--dry-run=server"]
    )
    patch = json.dumps({"metadata": {"labels": {"stego.dev/allocator": None}}})
    check(
        "owner label removal",
        ["patch", "namespace", owned, "--type=merge", "-p", patch, "--dry-run=server"],
        as_user=None,
    )
    patch = json.dumps(
        {"metadata": {"labels": {"pod-security.kubernetes.io/enforce": "privileged"}}}
    )
    check(
        "Pod security downgrade",
        ["patch", "namespace", owned, "--type=merge", "-p", patch, "--dry-run=server"],
        as_user=None,
    )

    def rb(namespace, role, sa, sa_ns, index="0", cluster=False):
        name = (
            base + "." + namespace + "." + index
            if cluster
            else "stego-" + marker + "-" + index
        )
        meta = {
            "name": name,
            "labels": {
                k: v for k, v in labels.items() if not k.startswith("pod-security.")
            },
        }
        if not cluster:
            meta["namespace"] = namespace
        return {
            "apiVersion": "rbac.authorization.k8s.io/v1",
            "kind": "ClusterRoleBinding" if cluster else "RoleBinding",
            "metadata": meta,
            "roleRef": {
                "apiGroup": "rbac.authorization.k8s.io",
                "kind": "ClusterRole",
                "name": role,
            },
            "subjects": [{"kind": "ServiceAccount", "name": sa, "namespace": sa_ns}],
        }

    check(
        "foreign namespace binding",
        ["create", "--dry-run=server", "-f", "-"],
        rb(foreign, base + ".data", "widget-data", control),
    )
    check(
        "wrong role",
        ["create", "--dry-run=server", "-f", "-"],
        rb(owned, "cluster-admin", "widget-data", control),
    )
    check(
        "wrong subject",
        ["create", "--dry-run=server", "-f", "-"],
        rb(owned, base + ".data", "widget-data", "default"),
    )
    check(
        "cluster binding before proof",
        ["create", "--dry-run=server", "-f", "-"],
        rb(owned, base + ".review", "gateway", owned, "1", True),
    )
    check(
        "declared data binding",
        ["create", "-f", "-"],
        rb(owned, base + ".data", "widget-data", control),
        good=True,
    )
    check(
        "namespace proof",
        ["create", "-f", "-"],
        rb(owned, base + ".proof", "widget-queue", control, "proof"),
        good=True,
    )
    cluster = rb(owned, base + ".review", "gateway", owned, "1", True)
    check("declared cluster binding", ["create", "-f", "-"], cluster, good=True)
    bindings.append(cluster["metadata"]["name"])
    check(
        "global data binding",
        ["create", "--dry-run=server", "-f", "-"],
        rb(owned, base + ".data", "gateway", owned, "2", True),
    )
    check(
        "foreign cluster binding",
        ["create", "--dry-run=server", "-f", "-"],
        rb(foreign, base + ".review", "gateway", foreign, "1", True),
    )
    quota["metadata"]["namespace"] = owned
    quota["spec"]["hard"]["limits.cpu"] = "2"
    check("quota increase", ["replace", "--dry-run=server", "-f", "-"], quota)
    for name in names:
        run(
            ["create", "-f", "-"],
            {
                "apiVersion": "v1",
                "kind": "Secret",
                "metadata": {"name": "probe", "namespace": name},
                "stringData": {"test": "not-a-credential"},
            },
        )
    worker = "system:serviceaccount:" + control + ":widget-data"
    check(
        "worker owned Secret read",
        ["get", "secret", "probe", "-n", owned, "-o", "name"],
        as_user=worker,
        good=True,
    )
    check(
        "worker foreign Secret read",
        ["get", "secret", "probe", "-n", foreign, "-o", "name"],
        as_user=worker,
    )
    check(
        "allocator Secret read", ["get", "secret", "probe", "-n", owned, "-o", "name"]
    )
    check(
        "worker Namespace create",
        ["create", "--dry-run=server", "-f", "-"],
        ns("tenant-" + uuid.uuid4().hex[:8]),
        as_user=worker,
    )
    check(
        "quota deletion by another actor",
        [
            "delete",
            "resourcequota",
            "stego-allocation",
            "-n",
            owned,
            "--dry-run=server",
        ],
        as_user=None,
    )
    check(
        "reserved binding deletion by another actor",
        [
            "delete",
            "rolebinding",
            "stego-" + marker + "-0",
            "-n",
            owned,
            "--dry-run=server",
        ],
        as_user=None,
    )
    check(
        "quota collection deletion while active",
        [
            "delete",
            "--raw",
            "/api/v1/namespaces/" + owned + "/resourcequotas?dryRun=All",
        ],
        as_user=None,
    )
    record = {
        "apiVersion": "v1",
        "kind": "ConfigMap",
        "metadata": {
            "name": "key-identity",
            "namespace": owned,
            "labels": {
                k: v for k, v in labels.items() if not k.startswith("pod-security.")
            },
        },
        "immutable": True,
        "data": {"linked_id": "gateway-1", "fingerprint": "gateway-1:public-hash"},
    }
    run(["create", "-f", "-"], record)
    record["metadata"]["namespace"] = foreign
    run(["create", "-f", "-"], record)
    check(
        "allocator public identity read",
        ["get", "configmap", "key-identity", "-n", owned, "-o", "name"],
        good=True,
    )
    check(
        "allocator foreign identity read",
        ["get", "configmap", "key-identity", "-n", foreign, "-o", "name"],
    )
    record["metadata"]["namespace"] = owned
    check(
        "allocator identity record create",
        ["create", "--dry-run=server", "-f", "-"],
        record,
    )
    seal = json.dumps(
        {
            "metadata": {
                "labels": {"example.test/linked-id": "gateway-1"},
                "annotations": {"example.test/fingerprint": "gateway-1:public-hash"},
            }
        }
    )
    check(
        "allocator namespace identity seal",
        ["patch", "namespace", owned, "--type=merge", "-p", seal],
        good=True,
    )
    change = json.dumps(
        {"metadata": {"annotations": {"example.test/fingerprint": "replacement"}}}
    )
    check(
        "sealed fingerprint change",
        ["patch", "namespace", owned, "--type=merge", "-p", change, "--dry-run=server"],
    )
    change = json.dumps({"metadata": {"labels": {"example.test/linked-id": None}}})
    check(
        "sealed label removal",
        ["patch", "namespace", owned, "--type=merge", "-p", change, "--dry-run=server"],
        as_user=None,
    )
    if args.next_manifest:
        next_doc = json.loads(args.next_manifest.read_text())
        policies = [
            item
            for item in next_doc["items"]
            if item["kind"]
            in {"ValidatingAdmissionPolicy", "ValidatingAdmissionPolicyBinding"}
        ]
        expected = {
            base + "." + suffix for suffix in ("allocation", "ownership", "resources")
        }
        if (
            len(policies) != 6
            or {item["metadata"]["name"] for item in policies} != expected
        ):
            raise RuntimeError("next fixture has unexpected policy names")
        run(
            ["apply", "-f", "-"],
            {"apiVersion": "v1", "kind": "List", "items": policies},
        )
        for policy in expected:
            for attempt in range(20):
                doc = json.loads(
                    run(
                        ["get", "validatingadmissionpolicy", policy, "-o", "json"]
                    ).stdout
                )
                if (
                    doc.get("status", {}).get("observedGeneration")
                    == doc["metadata"]["generation"]
                ):
                    break
                time.sleep(1)
            else:
                raise RuntimeError("next policy was not observed")
            if doc.get("status", {}).get("typeChecking", {}).get("expressionWarnings"):
                raise RuntimeError("next policy has type warnings")
        check(
            "retired subject binding delete",
            ["delete", "rolebinding", "stego-" + marker + "-0", "-n", owned],
            good=True,
        )
        check(
            "retired subject binding create",
            ["create", "--dry-run=server", "-f", "-"],
            rb(owned, base + ".data", "widget-data", control),
        )
        check(
            "replacement subject binding",
            ["create", "-f", "-"],
            rb(owned, base + ".data", "replacement", control),
            good=True,
        )
        check(
            "retired worker Secret read",
            ["get", "secret", "probe", "-n", owned, "-o", "name"],
            as_user=worker,
        )
        check(
            "replacement worker Secret read",
            ["get", "secret", "probe", "-n", owned, "-o", "name"],
            as_user="system:serviceaccount:" + control + ":replacement",
            good=True,
        )
        check(
            "retired cluster binding delete",
            ["delete", "clusterrolebinding", bindings[0], "--dry-run=server"],
            good=True,
        )
    # Prove orphan binding cleanup when its namespace authorization proof is gone.
    run(["delete", "namespace", owned, "--wait=true", "--timeout=60s"])
    created.remove(owned)
    check(
        "orphan cluster binding delete",
        ["delete", "clusterrolebinding", bindings[0], "--wait=false"],
        good=True,
    )
    bindings.clear()
    (d / "admission-results.json").write_text(json.dumps(evidence, indent=2) + "\n")
finally:
    for name in bindings:
        run(
            ["delete", "clusterrolebinding", name, "--ignore-not-found=true"],
            check=False,
        )
    for name in created:
        run(
            ["delete", "namespace", name, "--wait=false", "--ignore-not-found=true"],
            check=False,
        )
