#!/usr/bin/env python3
"""Test retained control-account grants after control namespace replacement.

Use the related and peer manifests from the account CI artifact. Hold the shared
live-test lease. This test creates no Pods and uses only public fixture data.
A successful account reuse is a failed security check, not a passing test.
"""

import argparse
import base64
import hashlib
import importlib.util
import json
from pathlib import Path
import ssl
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid

spec = importlib.util.spec_from_file_location("accounts", Path(__file__).with_name("check-allocation-account-identity.py"))
common = importlib.util.module_from_spec(spec)
spec.loader.exec_module(common)


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, request, response, code, message, headers, target):
        raise RuntimeError("The fixture API redirected a credential request")


def transport(cluster):
    server = cluster.get("server", "")
    url = urllib.parse.urlsplit(server)
    if url.scheme != "https" or not url.hostname or url.username or url.password or url.query or url.fragment or url.path not in ["", "/"]:
        raise RuntimeError("The fixture API address is invalid")
    if cluster.get("insecure-skip-tls-verify") or cluster.get("proxy-url") or cluster.get("tls-server-name"):
        raise RuntimeError("The fixture requires direct verified API TLS")
    ca = base64.b64decode(cluster["certificate-authority-data"], validate=True).decode()
    context = ssl.create_default_context(cadata=ca)
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), urllib.request.HTTPSHandler(context=context), NoRedirect())
    return server.rstrip("/"), opener


def request_status(server, opener, path, token, timeout=10):
    # The token stays in memory. It is not a process argument or an artifact.
    if not path.startswith("/api/v1/namespaces/") or any(c in token for c in "\r\n") or not token:
        raise RuntimeError("The fixture credential request is invalid")
    request = urllib.request.Request(server + path, headers={"Authorization": "Bearer " + token})
    try:
        with opener.open(request, timeout=timeout) as response:
            response.read(65537)
            return response.status
    except urllib.error.HTTPError as error:
        if error.code in [401, 403]:
            return error.code
        raise RuntimeError("The fixture returned an unexpected HTTP status") from None
    except Exception:
        raise RuntimeError("The fixture credential request failed") from None


def wait_for_token_rejection(fetch, record, now=time.monotonic, pause=time.sleep):
    started = now()
    deadline = started + 30
    while True:
        remaining = deadline - now()
        if remaining <= 0:
            break
        code = fetch(min(10, remaining))
        record(code, round(now() - started, 3))
        if code == 401:
            return
        if code != 200:
            raise RuntimeError("Old token rejection returned an unexpected HTTP status: " + str(code))
        pause(min(1, max(0, deadline - now())))
    raise RuntimeError("Old token still permits access after the rejection wait limit")


class Check(common.Check):
    def expect_status(self, name, code, expected):
        self.result.setdefault("http_observations", []).append({"name": name, "status": code, "expected": expected})
        self.save()
        if code != expected:
            raise RuntimeError("Fixture HTTP status differs for " + name + ": " + str(code))

    def token(self, namespace, name, user=None):
        result = self.run(["create", "token", name, "-n", namespace, "--duration=10m"], user=user)
        if result.returncode:
            raise RuntimeError("Fixture token issuance failed; private output is withheld")
        token = result.stdout.strip()
        if not token or len(token) > 32768:
            raise RuntimeError("Fixture token response is invalid")
        return token

    def role(self, kind, namespace, name, rules, user):
        metadata = {"name": name}
        if namespace:
            metadata["namespace"] = namespace
        role = self.create({"apiVersion": "rbac.authorization.k8s.io/v1", "kind": kind,
                            "metadata": metadata, "rules": rules})
        self.create({"apiVersion": role["apiVersion"], "kind": kind + "Binding", "metadata": metadata,
                     "roleRef": {"apiGroup": "rbac.authorization.k8s.io", "kind": kind, "name": name},
                     "subjects": [{"kind": "ServiceAccount", "name": user, "namespace": common.PEER}]})

    def exercise(self):
        configuration = self.run(["config", "view", "--minify", "--flatten", "-o", "json"])
        if configuration.returncode:
            raise RuntimeError("Fixture API configuration is unavailable")
        config = json.loads(configuration.stdout)
        assert len(config["clusters"]) == 1
        # config view hides CA bytes without --raw. Read only the CA field.
        ca = self.run(["config", "view", "--minify", "--flatten", "--raw", "-o", "jsonpath={.clusters[0].cluster.certificate-authority-data}"])
        if ca.returncode:
            raise RuntimeError("Fixture API trust is unavailable")
        cluster = config["clusters"][0]["cluster"]
        cluster["certificate-authority-data"] = ca.stdout.strip()
        server, opener = transport(cluster)
        primary = common.actor(common.CONTROL)
        outsider_name = "replacement-owner"
        outsider = "system:serviceaccount:" + common.PEER + ":" + outsider_name
        self.create({"apiVersion": "v1", "kind": "ServiceAccount", "metadata": {"namespace": common.PEER, "name": outsider_name}, "automountServiceAccountToken": False})
        self.role("ClusterRole", None, common.CONTROL + ".replacement-owner",
                  [{"apiGroups": [""], "resources": ["namespaces"], "verbs": ["create"]}], outsider_name)
        ns = common.namespace(common.PREFIX + uuid.uuid4().hex[:8], common.CONTROL, "owner-1")
        allocated = self.create(ns, primary)
        marker = hashlib.sha256((common.CONTROL + ".widget-queue").encode()).hexdigest()[:32]
        labels = {k: v for k, v in ns["metadata"]["labels"].items() if not k.startswith("pod-security.")}
        grant = self.create({"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "RoleBinding",
                             "metadata": {"name": "stego-" + marker + "-0", "namespace": ns["metadata"]["name"], "labels": labels},
                             "roleRef": {"apiGroup": "rbac.authorization.k8s.io", "kind": "ClusterRole", "name": common.CONTROL + ".widget-queue.data"},
                             "subjects": [{"kind": "ServiceAccount", "name": "widget-data", "namespace": common.CONTROL}]}, primary)
        self.create({"apiVersion": "v1", "kind": "Secret", "metadata": {"name": "public-probe", "namespace": ns["metadata"]["name"]}, "stringData": {"value": "public-test-data"}})
        self.create({"apiVersion": "v1", "kind": "ServiceAccount", "metadata": {"name": "widget-data", "namespace": common.CONTROL}, "automountServiceAccountToken": False})
        global_grant = self.get({"kind": "ClusterRoleBinding", "metadata": {"name": common.CONTROL + ".widget-queue"}})
        old_namespace = self.get(common.namespace(common.CONTROL))
        targets = {"widget-queue": "/api/v1/namespaces/" + ns["metadata"]["name"],
                   "widget-data": "/api/v1/namespaces/" + ns["metadata"]["name"] + "/secrets/public-probe"}
        old_accounts = {name: self.get({"kind": "ServiceAccount", "metadata": {"namespace": common.CONTROL, "name": name}}) for name in targets}
        old_tokens = {name: self.token(common.CONTROL, name) for name in targets}
        ordinary = self.token(common.PEER, outsider_name)
        for name, path in targets.items():
            self.expect_status(name + " original token", request_status(server, opener, path, old_tokens[name]), 200)
            self.expect_status(name + " ordinary owner", request_status(server, opener, path, ordinary), 403)
        self.remove(old_namespace)
        replacement = self.create(common.namespace(common.CONTROL), outsider)
        assert replacement["metadata"]["uid"] != old_namespace["metadata"]["uid"]
        # Model a new namespace owner. This grant contains no retained workload
        # or allocator permission. Account creation is tested as this identity.
        self.role("Role", common.CONTROL, "replacement-owner",
                  [{"apiGroups": [""], "resources": ["serviceaccounts", "serviceaccounts/token"], "verbs": ["create"]}], outsider_name)
        observations = []
        self.result["control_account_observations"] = observations
        self.result["control_namespace_old_uid"] = old_namespace["metadata"]["uid"]
        self.result["control_namespace_new_uid"] = replacement["metadata"]["uid"]
        self.save()
        expected_policies = [obj["metadata"]["name"] for obj in self.created if obj["kind"] == "ValidatingAdmissionPolicy"]
        for name, path in targets.items():
            def record_rejection(code, seconds):
                self.result.setdefault("old_token_rejection_observations", []).append({"account": name, "status": code, "seconds": seconds})
                self.save()
            wait_for_token_rejection(lambda timeout: request_status(server, opener, path, old_tokens[name], timeout), record_rejection)
            self.expect_status(name + " ordinary owner", request_status(server, opener, path, ordinary), 403)
            obj = {"apiVersion": "v1", "kind": "ServiceAccount", "metadata": {"name": name, "namespace": common.CONTROL}, "automountServiceAccountToken": False}
            result = self.run(["create", "--dry-run=server", "-f", "-", "-o", "json"], obj, outsider)
            if result.returncode:
                # A transport or evaluation error cannot establish this boundary.
                if not any("ValidatingAdmissionPolicy '" + policy + "' with binding '" + policy + "' denied request:" in result.stderr for policy in expected_policies):
                    raise RuntimeError("Control account creation failed without an admission denial")
                observations.append({"account": name, "creation_denied_by_admission": True})
                self.save()
                continue
            created = self.create(obj, outsider)
            assert created["metadata"]["uid"] != old_accounts[name]["metadata"]["uid"]
            token = self.token(common.CONTROL, name, outsider)
            assert token != old_tokens[name]
            code = request_status(server, opener, path, token)
            observations.append({"account": name, "creation_denied_by_admission": False,
                                 "old_account_uid": old_accounts[name]["metadata"]["uid"], "new_account_uid": created["metadata"]["uid"],
                                 "fresh_token_http_status": code, "retained_access_obtained": code == 200})
            self.save()
        for retained in [grant, global_grant]:
            current = self.get(retained)
            assert current["metadata"]["uid"] == retained["metadata"]["uid"] and current["subjects"] == retained["subjects"] and current["roleRef"] == retained["roleRef"]
        assert self.get(allocated)["metadata"]["uid"] == allocated["metadata"]["uid"]
        self.result.update(control_namespace_old_uid=old_namespace["metadata"]["uid"], control_namespace_new_uid=replacement["metadata"]["uid"],
                           control_account_observations=observations, retained_grants_unchanged=True,
                           old_tokens_rejected=True, ordinary_owner_tokens_denied=True,
                           scope="Admission and real short-lived token requests for fixed allocator and control-worker names after control namespace replacement. No Pods.")
        self.save()
        if any(v.get("retained_access_obtained") for v in observations):
            raise RuntimeError("A replacement namespace owner obtained retained control-account access")
        if not all(v.get("creation_denied_by_admission") for v in observations):
            raise RuntimeError("Control account creation was accepted; further identity checks are required")


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
        check.result["failure"] = str(error) or type(error).__name__
        raise
    finally:
        check.cleanup()
        check.save()


if __name__ == "__main__":
    main()
