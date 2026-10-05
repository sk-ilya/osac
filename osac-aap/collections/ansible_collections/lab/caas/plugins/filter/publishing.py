"""Pure transformations and ownership checks for the provider publishing profile."""

from copy import deepcopy
from ipaddress import IPv4Address
import re

TENANT = "osac.openshift.io/tenant"
OWNER = "deployment.osac.local/cluster-order-uid"
WORKER = "deployment.osac.local/worker"


def validate_profile(profile, order, action):
    required = {"tenant", "api_address", "api_port", "base_domain", "worker_namespace",
                "worker_name", "worker_mac", "ingress_service", "evidence"}
    if not isinstance(profile, dict) or set(profile) != required:
        raise ValueError("Profile must contain exactly the documented fields")
    if action not in ("create", "delete"):
        raise ValueError("Unknown publishing action")
    metadata = order["metadata"]
    if metadata.get("annotations", {}).get(TENANT) != profile["tenant"]:
        raise ValueError("ClusterOrder tenant does not match the provider profile")
    if not metadata.get("uid") or not metadata.get("name"):
        raise ValueError("ClusterOrder identity is required")
    IPv4Address(profile["api_address"])
    port = profile["api_port"]
    if type(port) is not int or not 30000 <= port <= 32767:
        raise ValueError("API port must be in the standard NodePort range")
    for key in ("worker_namespace", "worker_name", "ingress_service"):
        if not isinstance(profile[key], str) or not re.fullmatch(r"[a-z0-9](?:[-a-z0-9]*[a-z0-9])?", profile[key]) or len(profile[key]) > 63:
            raise ValueError("Invalid resource name: " + key)
    domain = profile["base_domain"]
    if not isinstance(domain, str) or len(domain) > 253 or any(
        not re.fullmatch(r"[a-z0-9](?:[-a-z0-9]*[a-z0-9])?", label) or len(label) > 63
        for label in domain.split(".")
    ):
        raise ValueError("Invalid base domain")
    if not re.fullmatch(r"(?:[0-9a-f]{2}:){5}[0-9a-f]{2}", profile["worker_mac"]):
        raise ValueError("Invalid worker MAC")
    evidence = profile["evidence"]
    checks = {"nodeport_connectivity", "guest_ingress", "dns"}
    if not isinstance(evidence, dict) or set(evidence) != checks or any(type(v) is not bool for v in evidence.values()):
        raise ValueError("Evidence must contain the three boolean checks")
    if action == "create" and not all(evidence.values()):
        raise ValueError("NodePort, guest ingress and DNS evidence gates remain incomplete")
    return deepcopy(profile)


def nodeport_definition(definition, profile, order):
    result = deepcopy(definition)
    if result["spec"]["platform"]["type"] != "Agent":
        raise ValueError("Publishing profile requires the Agent platform")
    if result["spec"]["dns"]["baseDomain"] != profile["base_domain"]:
        raise ValueError("HostedCluster base domain does not match the provider profile")
    apis = [s for s in result["spec"]["services"] if s["service"] == "APIServer"]
    if len(apis) != 1:
        raise ValueError("Exactly one APIServer publishing entry is required")
    apis[0]["servicePublishingStrategy"] = {
        "type": "NodePort", "nodePort": {"address": profile["api_address"], "port": profile["api_port"]}
    }
    annotations = result["metadata"].setdefault("annotations", {})
    annotations[TENANT] = order["metadata"]["annotations"][TENANT]
    annotations[OWNER] = order["metadata"]["uid"]
    if "osac.openshift.io/owner-reference" in order["metadata"]["annotations"]:
        annotations["osac.openshift.io/owner-reference"] = order["metadata"]["annotations"]["osac.openshift.io/owner-reference"]
    return result


def api_service_ready(service, profile):
    return service.get("spec", {}).get("type") == "NodePort" and any(
        p.get("nodePort") == profile["api_port"] and p.get("protocol", "TCP") == "TCP"
        for p in service.get("spec", {}).get("ports", [])
    )


def check_port(services, profile, control_plane_namespace):
    for service in services:
        if any(p.get("nodePort") == profile["api_port"] for p in service.get("spec", {}).get("ports", [])):
            metadata = service["metadata"]
            if metadata["namespace"] != control_plane_namespace or metadata["name"] != "kube-apiserver" or not api_service_ready(service, profile):
                raise ValueError("Requested API NodePort is already allocated")
    return True


def check_owned(resources, order):
    for resource in resources:
        annotations = resource["metadata"].get("annotations", {})
        if annotations.get(OWNER) != order["metadata"]["uid"] or annotations.get(TENANT) != order["metadata"]["annotations"][TENANT]:
            raise ValueError("Existing publishing resource belongs to another order or tenant")
    return True


def route_definition(profile, order):
    annotations = {TENANT: order["metadata"]["annotations"][TENANT], OWNER: order["metadata"]["uid"]}
    if "osac.openshift.io/owner-reference" in order["metadata"]["annotations"]:
        annotations["osac.openshift.io/owner-reference"] = order["metadata"]["annotations"]["osac.openshift.io/owner-reference"]
    name = order["metadata"]["name"]
    return {
        "apiVersion": "route.openshift.io/v1", "kind": "Route",
        "metadata": {"name": name + "-apps", "namespace": profile["worker_namespace"], "annotations": annotations},
        "spec": {"host": "wildcard.apps." + name + "." + profile["base_domain"],
                 "to": {"kind": "Service", "name": profile["ingress_service"]},
                 "port": {"targetPort": "https"}, "wildcardPolicy": "Subdomain",
                 "tls": {"termination": "passthrough", "insecureEdgeTerminationPolicy": "Redirect"}},
    }


class FilterModule:
    def filters(self):
        return {
            "validate_profile": validate_profile,
            "nodeport_definition": nodeport_definition,
            "api_service_ready": api_service_ready,
            "check_port": check_port,
            "check_owned": check_owned,
            "route_definition": route_definition,
        }
