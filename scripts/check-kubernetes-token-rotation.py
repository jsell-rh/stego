#!/usr/bin/env python3
"""Check a real projected-token rotation in a bounded, dedicated cluster Job.

Use an explicit saved context. The check creates no cluster RBAC rule. It uses
SelfSubjectReview to confirm which credential the API server authenticated.
Evidence contains counts and results, not tokens or credential identifiers.
An observation error leaves the Job in place for inspection. Do not start a
replacement until the existing Job state is known.
"""

from pathlib import Path
import argparse
import json
import subprocess
import tarfile
import tempfile
import time
import uuid

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument("--context", required=True)
parser.add_argument("--source", type=Path, required=True)
parser.add_argument("--evidence", type=Path)
args = parser.parse_args()
root = args.source.resolve()
directory = args.evidence or Path(tempfile.mkdtemp(prefix="stego-token-rotation-"))
directory.mkdir(mode=0o700, parents=True, exist_ok=False) if args.evidence else None
namespace = "stego-rotation-" + uuid.uuid4().hex[:8]
(directory / "namespace").write_text(namespace)


def oc(*command, **kwargs):
    return subprocess.check_output(
        [
            "oc",
            "--context=" + args.context,
            "--request-timeout=30s",
            "-n",
            namespace,
            *command,
        ],
        **kwargs,
    )


paths = (
    subprocess.check_output(["git", "ls-files", "-z"], cwd=root).decode().split("\0")
)
with tarfile.open(directory / "source.tar", "w") as archive:
    for name in paths:
        if name and (root / name).is_file():
            archive.add(root / name, arcname=name, recursive=False)

run = """mkdir -p /work/tmp /work/compiler
while [ ! -f /work/start ]; do sleep 1; done
cd /work/compiler
go test -v -count=1 -timeout=16m ./internal/generator/kubernetesclient > /work/test.log 2>&1
result=$?
echo "$result" > /work/result
while [ ! -f /work/collected ]; do sleep 1; done
exit "$result"
"""
environment = {
    "GOMAXPROCS": "1",
    "GOMEMLIMIT": "2GiB",
    "GOFLAGS": "-p=1",
    "GOTOOLCHAIN": "local",
    "GOWORK": "off",
    "GOCACHE": "/work/cache",
    "GOMODCACHE": "/work/modules",
    "GOPATH": "/work/go",
    "TMPDIR": "/work/tmp",
    "STEGO_KUBERNETES_LIVE_ROTATION": "1",
    "STEGO_KUBERNETES_ROTATION_ARTIFACTS": "/work/artifacts",
    "STEGO_TEST_NAMESPACE": namespace,
}
container = {
    "name": "test",
    "image": "docker.io/library/golang@sha256:2d54f6c8c6ea532a321e0b4c69553b2ed3637608d4f4357dbed37939fe2620cc",
    "command": ["sh", "-c", run],
    "env": [{"name": k, "value": v} for k, v in environment.items()]
    + [
        {
            "name": "STEGO_TEST_POD_UID",
            "valueFrom": {"fieldRef": {"fieldPath": "metadata.uid"}},
        }
    ],
    "resources": {
        "requests": {"cpu": "100m", "memory": "256Mi", "ephemeral-storage": "1Gi"},
        "limits": {"cpu": "1", "memory": "3Gi", "ephemeral-storage": "6Gi"},
    },
    "securityContext": {
        "runAsNonRoot": True,
        "readOnlyRootFilesystem": True,
        "allowPrivilegeEscalation": False,
        "capabilities": {"drop": ["ALL"]},
    },
    "volumeMounts": [
        {"name": "work", "mountPath": "/work"},
        {
            "name": "kubernetes-api",
            "mountPath": "/var/run/stego-kubernetes",
            "readOnly": True,
        },
    ],
}
items = [
    {"apiVersion": "v1", "kind": "Namespace", "metadata": {"name": namespace}},
    {
        "apiVersion": "v1",
        "kind": "ResourceQuota",
        "metadata": {"name": "check", "namespace": namespace},
        "spec": {
            "hard": {
                "pods": "1",
                "limits.cpu": "1",
                "limits.memory": "3Gi",
                "limits.ephemeral-storage": "6Gi",
            }
        },
    },
    {
        "apiVersion": "v1",
        "kind": "ServiceAccount",
        "metadata": {"name": "rotation", "namespace": namespace},
        "automountServiceAccountToken": False,
    },
    {
        "apiVersion": "batch/v1",
        "kind": "Job",
        "metadata": {"name": "check", "namespace": namespace},
        "spec": {
            "backoffLimit": 0,
            "activeDeadlineSeconds": 1200,
            "template": {
                "spec": {
                    "serviceAccountName": "rotation",
                    "restartPolicy": "Never",
                    "automountServiceAccountToken": False,
                    "securityContext": {
                        "runAsNonRoot": True,
                        "seccompProfile": {"type": "RuntimeDefault"},
                    },
                    "containers": [container],
                    "volumes": [
                        {"name": "work", "emptyDir": {"sizeLimit": "6Gi"}},
                        {
                            "name": "kubernetes-api",
                            "projected": {
                                "defaultMode": 288,
                                "sources": [
                                    {
                                        "serviceAccountToken": {
                                            "path": "token",
                                            "expirationSeconds": 600,
                                        }
                                    },
                                    {
                                        "configMap": {
                                            "name": "kube-root-ca.crt",
                                            "items": [
                                                {"key": "ca.crt", "path": "ca.crt"}
                                            ],
                                        }
                                    },
                                ],
                            },
                        },
                    ],
                }
            },
        },
    },
]
(directory / "job.json").write_text(
    json.dumps({"apiVersion": "v1", "kind": "List", "items": items}, indent=2) + "\n"
)
print(f"Evidence: {directory}; namespace: {namespace}", flush=True)
print(oc("apply", "-f", str(directory / "job.json")).decode(), flush=True)
for _ in range(60):
    pods = json.loads(oc("get", "pods", "-l", "job-name=check", "-o", "json"))["items"]
    if pods:
        break
    time.sleep(1)
else:
    raise RuntimeError("No Pod observed. Inspect the existing Job before another run.")
pod = pods[0]["metadata"]["name"]
oc("wait", "--for=condition=Ready", "pod/" + pod, "--timeout=120s")
with (directory / "source.tar").open("rb") as source:
    subprocess.run(
        [
            "oc",
            "--context=" + args.context,
            "-n",
            namespace,
            "exec",
            "-i",
            pod,
            "--",
            "tar",
            "xf",
            "-",
            "-C",
            "/work/compiler",
        ],
        stdin=source,
        check=True,
        timeout=120,
    )
oc("exec", pod, "--", "touch", "/work/start")
print("Generated-client rotation check started", flush=True)
while True:
    result = subprocess.run(
        [
            "oc",
            "--context=" + args.context,
            "--request-timeout=30s",
            "-n",
            namespace,
            "exec",
            pod,
            "--",
            "cat",
            "/work/result",
        ],
        capture_output=True,
        text=True,
        timeout=40,
    )
    if result.returncode == 0:
        break
    job = json.loads(oc("get", "job", "check", "-o", "json"))
    if any(
        c["type"] == "Failed" and c["status"] == "True"
        for c in job.get("status", {}).get("conditions", [])
    ):
        (directory / "job-final.json").write_text(json.dumps(job, indent=2) + "\n")
        raise RuntimeError(
            "The Job failed before evidence collection. Inspect its Pod."
        )
    time.sleep(10)
(directory / "test.log").write_bytes(oc("exec", pod, "--", "cat", "/work/test.log"))
runtime = subprocess.run(
    [
        "oc",
        "--context=" + args.context,
        "-n",
        namespace,
        "exec",
        pod,
        "--",
        "cat",
        "/work/artifacts/runtime.log",
    ],
    capture_output=True,
    timeout=40,
)
if runtime.returncode == 0:
    (directory / "runtime.log").write_bytes(runtime.stdout)
if result.stdout.strip() == "0":
    if runtime.returncode != 0:
        raise RuntimeError(
            "Successful command has no runtime evidence. Inspect the Job."
        )
    (directory / "rotation.json").write_bytes(
        oc("exec", pod, "--", "cat", "/work/artifacts/rotation.json")
    )
oc("exec", pod, "--", "touch", "/work/collected")
condition = "Complete" if result.stdout.strip() == "0" else "Failed"
oc("wait", "--for=condition=" + condition, "job/check", "--timeout=60s")
(directory / "job-final.json").write_bytes(oc("get", "job", "check", "-o", "json"))
log = directory / ("runtime.log" if runtime.returncode == 0 else "test.log")
print(log.read_text()[-5000:], flush=True)
print(oc("delete", "namespace", namespace, "--wait=false").decode(), flush=True)
raise SystemExit(0 if result.stdout.strip() == "0" else 1)
