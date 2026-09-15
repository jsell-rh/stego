#!/usr/bin/env python3
"""Check generated allocation policies in an existing, dedicated test namespace.

First install the manifest from TestAllocationManifests. This check creates no
Pod. It removes its own namespace and binding fixtures, including on failure.
The caller must remove the installed policies and roles after all checks.
"""

from pathlib import Path
import argparse
import copy
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
parser.add_argument("--network-isolation", action="store_true")
parser.add_argument("--network-peers", action="store_true")
args = parser.parse_args()
if args.network_peers and not args.network_isolation:
    parser.error("--network-peers requires --network-isolation")

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


def check_network_policy():
    policy = {
        "apiVersion": "networking.k8s.io/v1",
        "kind": "NetworkPolicy",
        "metadata": {
            "name": "stego-allocation", "namespace": owned,
            "labels": {k: v for k, v in labels.items() if not k.startswith("pod-security.")},
        },
        "spec": {"podSelector": {}, "policyTypes": ["Ingress", "Egress"]},
    }
    if args.network_peers:
        def rule(side, namespace, pod, port, protocol):
            return {
                side: [{
                    "namespaceSelector": {"matchLabels": {"kubernetes.io/metadata.name": namespace}},
                    "podSelector": {"matchLabels": {"app": pod}},
                }],
                "ports": [{"port": port, "protocol": protocol}],
            }
        policy["spec"]["ingress"] = [rule("from", control, "controller", 8080, "TCP")]
        policy["spec"]["egress"] = [
            rule("to", owned, "database", 5432, "TCP"),
            rule("to", "cluster-dns", "dns", 53, "UDP"),
        ]
    check("declared network policy", ["create", "--dry-run=server", "-f", "-"], policy, good=True)
    invalid = []
    for direction in ("ingress", "egress"):
        changed = copy.deepcopy(policy)
        changed["spec"][direction] = [{}]
        invalid.append((direction + " allow rule", changed))
    for field, value in (
        ("podSelector", {"matchLabels": {"app": "selected"}}),
        ("podSelector", {"matchExpressions": [{"key": "app", "operator": "Exists"}]}),
        ("policyTypes", ["Ingress"]),
        ("policyTypes", ["Egress"]),
        ("policyTypes", ["Ingress", "Ingress"]),
    ):
        changed = copy.deepcopy(policy)
        changed["spec"][field] = value
        invalid.append(("changed network " + field + " " + json.dumps(value), changed))
    for field, value in (("name", "other"), ("namespace", foreign)):
        changed = copy.deepcopy(policy)
        changed["metadata"][field] = value
        invalid.append(("foreign network " + field, changed))
    changed = copy.deepcopy(policy)
    changed["metadata"]["labels"]["example.test/owner"] = "other"
    invalid.append(("foreign network owner", changed))
    if args.network_peers:
        for direction, side, count in (("ingress", "from", 1), ("egress", "to", 2)):
            for index in range(count):
                for mutation in ("namespace", "pod", "no namespace", "no pod", "OR selectors", "port", "protocol", "all ports", "port range", "extra peer"):
                    changed = copy.deepcopy(policy)
                    selected = changed["spec"][direction][index]
                    peer = selected[side][0]
                    if mutation == "namespace":
                        peer["namespaceSelector"]["matchLabels"]["kubernetes.io/metadata.name"] = foreign
                    elif mutation == "pod":
                        peer["podSelector"]["matchLabels"]["app"] = "unrelated"
                    elif mutation == "no namespace":
                        del peer["namespaceSelector"]
                    elif mutation == "no pod":
                        del peer["podSelector"]
                    elif mutation == "OR selectors":
                        selected[side] = [{"namespaceSelector": peer["namespaceSelector"]}, {"podSelector": peer["podSelector"]}]
                    elif mutation == "port":
                        selected["ports"][0]["port"] += 1
                    elif mutation == "protocol":
                        selected["ports"][0]["protocol"] = "UDP" if selected["ports"][0]["protocol"] == "TCP" else "TCP"
                    elif mutation == "all ports":
                        del selected["ports"]
                    elif mutation == "port range":
                        selected["ports"][0]["endPort"] = 65535
                    elif mutation == "extra peer":
                        selected[side].append({"ipBlock": {"cidr": "0.0.0.0/0"}})
                    invalid.append((f"{direction} peer {index} {mutation}", changed))
    for label, changed in invalid:
        check(label, ["create", "--dry-run=server", "-f", "-"], changed)
    check("another actor creates reserved policy", ["create", "--dry-run=server", "-f", "-"], policy, as_user=None)
    additional = copy.deepcopy(policy)
    additional["metadata"]["name"] = "additional-allow"
    additional["metadata"]["labels"] = {}
    additional["spec"]["ingress"] = [{}]
    check("another actor adds an unlabelled allow policy", ["create", "--dry-run=server", "-f", "-"], additional, as_user=None)
    check("create declared network policy", ["create", "-f", "-"], policy, good=True)
    check("allocator reads declared network policy", ["get", "networkpolicy", "stego-allocation", "-n", owned, "-o", "name"], good=True)
    check("allocator lists the complete policy set", ["get", "networkpolicies", "-n", owned], good=True)
    change = json.dumps({"spec": {"ingress": [{}]}})
    check("another actor changes declared policy", ["patch", "networkpolicy", "stego-allocation", "-n", owned, "--type=merge", "-p", change, "--dry-run=server"], as_user=None)
    for identity in (actor, None):
        check("reserved policy deletion " + (identity or "operator"), ["delete", "networkpolicy", "stego-allocation", "-n", owned, "--dry-run=server"], as_user=identity)


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
    if args.network_isolation:
        check_network_policy()
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
        if args.network_isolation:
            extra = {"apiVersion": "networking.k8s.io/v1", "kind": "NetworkPolicy", "metadata": {"name": "extra", "namespace": owned}, "spec": {"podSelector": {}, "policyTypes": ["Ingress"], "ingress": [{}]}}
            check("regenerated policy blocks an extra allow policy", ["create", "--dry-run=server", "-f", "-"], extra, as_user=None)
            check("declared policy survives regeneration", ["get", "networkpolicy", "stego-allocation", "-n", owned, "-o", "name"], good=True)
            check("regenerated policy blocks another actor", ["patch", "networkpolicy", "stego-allocation", "-n", owned, "--type=merge", "-p", json.dumps({"spec": {"egress": [{}]}}), "--dry-run=server"], as_user=None)
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
