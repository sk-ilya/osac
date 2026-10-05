from copy import deepcopy
import importlib.util
from pathlib import Path
import unittest

SPEC = importlib.util.spec_from_file_location(
    "publishing", Path(__file__).resolve().parents[2] / "plugins/filter/publishing.py"
)
p = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(p)


def profile():
    return {"tenant": "test-tenant", "api_address": "192.0.2.10", "api_port": 30443,
            "base_domain": "lab.example.test", "worker_namespace": "workers",
            "worker_name": "worker-1", "worker_mac": "02:00:00:00:00:01",
            "ingress_service": "worker-ingress",
            "evidence": {"nodeport_connectivity": True, "guest_ingress": True, "dns": True}}


def order():
    return {"metadata": {"name": "order-test", "uid": "order-uid", "annotations": {p.TENANT: "test-tenant"}}}


def definition():
    return {"metadata": {"name": "order-test", "annotations": {"existing": "keep"}},
            "spec": {"platform": {"type": "Agent"}, "dns": {"baseDomain": "lab.example.test"},
                     "services": [{"service": "APIServer", "servicePublishingStrategy": {"type": "LoadBalancer"}},
                                  {"service": "OAuthServer", "servicePublishingStrategy": {"type": "Route"}}]}}


class PublishingTests(unittest.TestCase):
    def test_profile_copies_valid_configuration(self):
        original = profile()
        self.assertEqual(p.validate_profile(original, order(), "create"), original)
        self.assertIsNot(p.validate_profile(original, order(), "create"), original)

    def test_profile_rejects_wrong_tenant(self):
        cfg = profile()
        cfg["tenant"] = "other"
        with self.assertRaises(ValueError):
            p.validate_profile(cfg, order(), "create")

    def test_creation_requires_every_evidence_gate(self):
        for gate in profile()["evidence"]:
            cfg = profile()
            cfg["evidence"][gate] = False
            with self.assertRaises(ValueError):
                p.validate_profile(cfg, order(), "create")

    def test_deletion_does_not_require_connectivity(self):
        cfg = profile()
        cfg["evidence"] = dict.fromkeys(cfg["evidence"], False)
        self.assertEqual(p.validate_profile(cfg, order(), "delete"), cfg)

    def test_string_true_is_not_evidence(self):
        cfg = profile()
        cfg["evidence"]["dns"] = "true"
        with self.assertRaises(ValueError):
            p.validate_profile(cfg, order(), "create")

    def test_rejects_unknown_profile_fields(self):
        cfg = profile()
        cfg["token"] = "placeholder"
        with self.assertRaises(ValueError):
            p.validate_profile(cfg, order(), "create")

    def test_rejects_invalid_addresses_ports_domains_and_macs(self):
        for key, value in [("api_address", "::1"), ("api_port", 6443), ("api_port", True),
                           ("base_domain", "bad..domain"), ("worker_mac", "invalid")]:
            cfg = profile()
            cfg[key] = value
            with self.assertRaises(ValueError):
                p.validate_profile(cfg, order(), "create")

    def test_nodeport_changes_only_api_and_ownership(self):
        original = definition()
        snapshot = deepcopy(original)
        result = p.nodeport_definition(original, profile(), order())
        self.assertEqual(original, snapshot)
        self.assertEqual(result["spec"]["services"][1], original["spec"]["services"][1])
        self.assertEqual(result["metadata"]["annotations"]["existing"], "keep")
        self.assertEqual(result["metadata"]["annotations"][p.TENANT], "test-tenant")
        self.assertEqual(result["metadata"]["annotations"][p.OWNER], "order-uid")
        self.assertEqual(result["spec"]["services"][0]["servicePublishingStrategy"],
                         {"type": "NodePort", "nodePort": {"address": "192.0.2.10", "port": 30443}})
        self.assertEqual(p.nodeport_definition(result, profile(), order()), result)

    def test_rejects_non_agent_platform(self):
        original = definition()
        original["spec"]["platform"]["type"] = "KubeVirt"
        with self.assertRaises(ValueError):
            p.nodeport_definition(original, profile(), order())

    def test_rejects_wrong_hosted_domain(self):
        cfg = profile()
        cfg["base_domain"] = "other.example.test"
        with self.assertRaises(ValueError):
            p.nodeport_definition(definition(), cfg, order())

    def test_rejects_missing_or_duplicate_apiserver(self):
        for services in [[], definition()["spec"]["services"] * 2]:
            original = definition()
            original["spec"]["services"] = services
            with self.assertRaises(ValueError):
                p.nodeport_definition(original, profile(), order())

    def test_loadbalancer_does_not_satisfy_nodeport_readiness(self):
        service = {"spec": {"type": "LoadBalancer", "ports": [{"nodePort": 30443}]}}
        self.assertFalse(p.api_service_ready(service, profile()))
        service["spec"]["type"] = "NodePort"
        self.assertTrue(p.api_service_ready(service, profile()))
        service["spec"]["ports"][0]["nodePort"] = 30444
        self.assertFalse(p.api_service_ready(service, profile()))

    def test_conflicting_port_is_rejected(self):
        service = {"metadata": {"namespace": "foreign", "name": "other"},
                   "spec": {"type": "NodePort", "ports": [{"nodePort": 30443}]}}
        with self.assertRaises(ValueError):
            p.check_port([service], profile(), "control-plane")
        service["metadata"] = {"namespace": "control-plane", "name": "kube-apiserver"}
        self.assertTrue(p.check_port([service], profile(), "control-plane"))
        self.assertTrue(p.check_port([], profile(), "control-plane"))

    def test_route_is_passthrough_and_owned(self):
        route = p.route_definition(profile(), order())
        self.assertEqual(route["spec"]["host"], "wildcard.apps.order-test.lab.example.test")
        self.assertEqual(route["spec"]["tls"]["termination"], "passthrough")
        self.assertEqual(route["spec"]["wildcardPolicy"], "Subdomain")
        self.assertTrue(p.check_owned([route], order()))
        self.assertTrue(p.check_owned([], order()))

    def test_foreign_route_is_rejected(self):
        for key in [p.OWNER, p.TENANT]:
            route = p.route_definition(profile(), order())
            route["metadata"]["annotations"][key] = "foreign"
            with self.assertRaises(ValueError):
                p.check_owned([route], order())

    def test_preserves_owner_reference_annotation(self):
        co = order()
        co["metadata"]["annotations"]["osac.openshift.io/owner-reference"] = "reference-placeholder"
        for result in [p.nodeport_definition(definition(), profile(), co), p.route_definition(profile(), co)]:
            self.assertEqual(result["metadata"]["annotations"]["osac.openshift.io/owner-reference"], "reference-placeholder")


if __name__ == "__main__":
    unittest.main()
