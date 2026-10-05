# Provider Agent CaaS publishing

This collection adds a fail-closed NodePort profile to the CI cluster template.
It does not provision VMs, replace OSAC networking managers, or implement tenant
network isolation. Other cluster templates and VMaaS entry points are unchanged.

The provider project loads lab.caas.profile after template/namespace resolution.
The role reads caas-lab-profile in POD_NAMESPACE, checks tenant scope, and wires
existing template hooks. Creation requires explicit connectivity/DNS evidence
flags. Deletion retains the inventory VM, disks, Service and global DNS suffix.
Keep the profile ConfigMap until all profile-managed orders have been deleted.

The publishing role changes the APIServer definition before creation, validates
the resulting NodePort Service, and publishes a wildcard passthrough Route to a
pre-existing, owned VM ingress Service. Route ownership uses the ClusterOrder UID;
tenant annotations are copied from the order. Existing foreign Routes are rejected.
Kubeconfig handling and cluster-operator waits delegate to existing OSAC roles.

Unit tests: python3 -m unittest discover -s tests/unit -v
Real AAP/HyperShift, guest traffic and Route admission require deployed validation.
