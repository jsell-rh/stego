"""Check related-account fixtures and cleanup without a cluster connection."""
import copy
import importlib.util
from pathlib import Path
import unittest


def load(name, filename):
    spec = importlib.util.spec_from_file_location(name, Path(__file__).with_name(filename))
    result = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(result)
    return result


peers = load("peer_bindings", "check-allocation-peer-bindings.py")
identity_tests = load("identity_tests", "test_allocation_account_identity.py")


class Fixture(identity_tests.Fixture):
    allocation_prefixes = peers.Check.allocation_prefixes


class PeerRunnerTests(unittest.TestCase):
    def test_source_identity_matches_common_fixture(self):
        for owner in ["owner-1", "owner-2"]:
            self.assertEqual(peers.account_name(owner, "gateway"), peers.common.account_name(peers.common.CONTROL, owner))
        self.assertNotEqual(peers.account_name("owner-1", "gateway"), peers.account_name("owner-1", "runner"))

    def test_destination_has_both_account_identities(self):
        ns = peers.destination("12345678", "owner-1")
        self.assertEqual(ns["metadata"]["name"], peers.DESTINATION_PREFIX + "12345678")
        self.assertEqual(ns["metadata"]["labels"]["stego.dev/allocation-profile"], "jobs")
        self.assertEqual(ns["metadata"]["annotations"], {
            peers.common.ANNOTATION: peers.account_name("owner-1", "gateway"),
            "stego.dev/service-account-runner": peers.account_name("owner-1", "runner")})

    def test_both_allocations_are_removed_before_their_guards(self):
        source = identity_tests.namespace("source")
        destination = peers.destination("12345678", "owner-1")
        destination["metadata"]["uid"] = "destination"
        guard = {"kind": "ValidatingAdmissionPolicy", "apiVersion": "admissionregistration.k8s.io/v1", "metadata": {"name": "fixture", "uid": "guard"}}
        fixture = Fixture([source, destination, guard])
        fixture.result["checks_passed"] = True
        fixture.cleanup()
        removed = [args[1] for args, obj, user in fixture.commands if args[0] == "delete"]
        self.assertEqual(removed, ["--raw=" + peers.common.path(value) for value in [source, destination, guard]])
        self.assertTrue(fixture.result["complete"])

    def test_replacement_namespace_is_not_deleted(self):
        old = peers.destination("12345678", "owner-1")
        old["metadata"]["uid"] = "old"
        replacement = copy.deepcopy(old)
        replacement["metadata"]["uid"] = "replacement"
        fixture = Fixture([old])
        fixture.objects[peers.common.path(old)] = replacement
        with self.assertRaisesRegex(RuntimeError, "cleanup is incomplete"):
            fixture.cleanup()
        self.assertFalse(any(args[0] == "delete" for args, obj, user in fixture.commands))
        self.assertFalse(fixture.result["complete"])

    def test_secret_cleanup_keeps_uid_preconditions(self):
        value = {"apiVersion": "v1", "kind": "Secret", "metadata": {"name": "public-probe", "namespace": peers.common.CONTROL, "uid": "secret"}}
        fixture = Fixture([value])
        fixture.cleanup()
        self.assertEqual(fixture.objects, {})
        self.assertFalse(fixture.result["complete"])


if __name__ == "__main__":
    unittest.main()
