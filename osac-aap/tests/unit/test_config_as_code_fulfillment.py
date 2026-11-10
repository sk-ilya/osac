"""Shared fulfillment Pod settings and monorepo inventory paths."""

from pathlib import Path

import pytest
import yaml
from ansible.parsing.dataloader import DataLoader
from ansible.template import Templar

ROOT = Path(__file__).resolve().parents[3]
CONFIG = ROOT / "osac-aap/collections/ansible_collections/osac/config_as_code"


@pytest.mark.parametrize("group", ["cluster-fulfillment", "bare-metal-fulfillment"])
@pytest.mark.parametrize("ca_name", [None, "", "customer-service-ca"])
@pytest.mark.parametrize("pull_secrets", [None, '[{"name":"registry-credentials"}]'])
def test_fulfillment_pods_keep_native_identity_and_explicit_trust(
    monkeypatch: pytest.MonkeyPatch,
    group: str,
    ca_name: str | None,
    pull_secrets: str | None,
) -> None:
    """Keep required API trust while making service trust and registry auth explicit."""
    for name, value in {
        "AAP_FULFILLMENT_SERVICE_CA_CONFIGMAP": ca_name,
        "AAP_EE_PULL_SECRETS": pull_secrets,
    }.items():
        monkeypatch.delenv(name, raising=False)
        if value is not None:
            monkeypatch.setenv(name, value)
    variables = yaml.safe_load((CONFIG / "playbooks/vars/config.yml").read_text())
    variables.update(aap_ee_image="example.invalid/osac:test", osac_sa_name="osac-sa")
    templar = Templar(loader=DataLoader(), variables=variables)
    template = CONFIG / f"roles/aap/templates/{group}-ig.j2"
    pod = yaml.safe_load(templar.template(template.read_text()))
    spec = pod["spec"]
    worker = next(c for c in spec["containers"] if c["name"] == "worker")
    assert spec["serviceAccountName"] == "osac-sa"
    assert "imagePullPolicy" not in worker
    assert spec.get("imagePullSecrets", []) == (
        [] if pull_secrets is None else yaml.safe_load(pull_secrets)
    )
    volume = next(v for v in spec["volumes"] if v["name"] == "kube-api-access")
    sources = volume["projected"]["sources"]
    assert len([s for s in sources if "serviceAccountToken" in s]) == 1
    assert len([s for s in sources if "downwardAPI" in s]) == 1
    configmaps = [s["configMap"] for s in sources if "configMap" in s]
    assert configmaps[0] == {
        "name": "kube-root-ca.crt",
        "items": [{"key": "ca.crt", "path": "ca.crt"}],
    }
    expected_ca = "openshift-service-ca.crt" if ca_name is None else ca_name
    assert configmaps[1:] == (
        [{"name": expected_ca, "items": [{"key": "service-ca.crt", "path": "service-ca.crt"}]}]
        if expected_ca else []
    )
    assert all(not c.get("optional", False) for c in configmaps)


def test_all_shared_inventory_paths_resolve_from_the_project_root() -> None:
    """Fail when a shared SCM inventory points outside the monorepo layout."""
    objects = yaml.safe_load((CONFIG / "roles/aap/vars/controller.yml").read_text())
    sources = objects["controller_inventory_sources"]
    assert len(sources) == len(objects["controller_inventories"])
    for source in sources:
        assert source["source_path"] == "osac-aap/inventory/localhost.yml"
        inventory = yaml.safe_load((ROOT / source["source_path"]).read_text())
        assert "localhost" in inventory["all"]["hosts"]
