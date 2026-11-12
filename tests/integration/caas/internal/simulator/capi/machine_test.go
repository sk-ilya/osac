// Copyright (c) 2026 Red Hat Inc.
// SPDX-License-Identifier: Apache-2.0

package capi

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	agentv1 "github.com/openshift/cluster-api-provider-agent/api/v1beta1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	capiv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/util/conditions"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/osac-project/osac/tests/integration/caas/internal/simulator/kube"
)

var _ = DescribeTable("Machine infrastructure adoption", func(ctx SpecContext, variant string) {
	By("preparing a pending clone with its MachineSet and an independent owner")

	scheme := capiScheme()
	set := &capiv1.MachineSet{ObjectMeta: metav1.ObjectMeta{Namespace: "cp", Name: "workers", UID: "current-set"}}
	machine := &capiv1.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "cp", Name: "worker", UID: "current-machine",
		},
		Spec: capiv1.MachineSpec{InfrastructureRef: corev1.ObjectReference{
			APIVersion: agentv1.GroupVersion.String(), Kind: "AgentMachine", Namespace: "cp", Name: "worker",
		}},
	}
	Expect(controllerutil.SetControllerReference(set, machine, scheme)).To(Succeed())
	clone := &agentv1.AgentMachine{ObjectMeta: metav1.ObjectMeta{
		Namespace: "cp", Name: "worker",
		OwnerReferences: []metav1.OwnerReference{
			{APIVersion: capiv1.GroupVersion.String(), Kind: "MachineSet", Name: set.Name, UID: set.UID},
			{APIVersion: "v1", Kind: "Secret", Name: "foreign", UID: "preserve-me"},
		},
	}}

	switch variant {
	case "foreign", "previous-machine":
		owner := machine.DeepCopy()
		owner.UID = "old-machine"
		if variant == "foreign" {
			owner.Name, owner.UID = "other", "foreign"
		}

		Expect(controllerutil.SetControllerReference(owner, clone, scheme)).To(Succeed())
	case "previous-set":
		clone.OwnerReferences[0].UID = "old-set"
	case "template":
		machine.Spec.InfrastructureRef.Kind = "AgentMachineTemplate"
		machine.Spec.InfrastructureRef.Name = "template"
	}

	beforeOwners := append([]metav1.OwnerReference{}, clone.OwnerReferences...)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(machine, clone).WithStatusSubresource(machine).Build()
	r := &MachineReconciler{Client: c, APIReader: c}

	By("adopting only concrete infrastructure from the current MachineSet")

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(machine)})
	if variant == "current" {
		Expect(err).NotTo(HaveOccurred())
	} else {
		Expect(err).To(HaveOccurred())
	}

	Expect(c.Get(ctx, client.ObjectKeyFromObject(clone), clone)).To(Succeed())

	if variant == "current" {
		expected := append(beforeOwners, *metav1.NewControllerRef(machine, capiv1.GroupVersion.WithKind("Machine")))
		Expect(clone.OwnerReferences).To(ConsistOf(expected), "adoption must retain both original non-controller owners")
	} else {
		Expect(clone.OwnerReferences).To(Equal(beforeOwners))
	}

	By("preserving the Machine's infrastructure reference")

	current := &capiv1.Machine{}
	Expect(c.Get(ctx, client.ObjectKeyFromObject(machine), current)).To(Succeed())
	Expect(current.Spec).To(Equal(machine.Spec))
},
	Entry("adopts a pending clone and retains its non-controller owners", "current"),
	Entry("rejects another controller's clone", "foreign"),
	Entry("rejects a previous Machine's clone", "previous-machine"),
	Entry("rejects a previous MachineSet's clone", "previous-set"),
	Entry("rejects a template reference", "template"),
)

var _ = DescribeTable("Machine infrastructure controller ownership", func(variant string) {
	machine := &capiv1.Machine{ObjectMeta: metav1.ObjectMeta{Namespace: "cp", Name: "worker", UID: "current-machine"}}
	infra := &agentv1.AgentMachine{ObjectMeta: metav1.ObjectMeta{Namespace: "cp", Name: "worker"}}
	Expect(controllerutil.SetControllerReference(machine, infra, capiScheme())).To(Succeed())

	switch variant {
	case "namespace":
		infra.Namespace = "other"
	case "missing-uid":
		machine.UID = ""
		infra.OwnerReferences[0].UID = ""
	}

	err := validateInfrastructureOwner(machine, infra)
	if variant == "current" {
		Expect(err).NotTo(HaveOccurred())
	} else {
		Expect(err).To(HaveOccurred())
	}
},
	Entry("accepts the current Machine's clone", "current"),
	Entry("rejects a clone in another namespace", "namespace"),
	Entry("rejects a missing Machine UID", "missing-uid"),
)

var _ = Describe("Machine readiness observations", func() {
	It("separates current provider conditions from sticky infrastructure and bootstrap readiness", func(ctx SpecContext) {
		machine := &capiv1.Machine{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "cp",
				Name:      "worker",
				UID:       "original",
				Labels:    map[string]string{kube.ManagedByLabel: kube.ManagerName},
			},
			Spec: capiv1.MachineSpec{
				Bootstrap: capiv1.Bootstrap{DataSecretName: new("bootstrap")},
				InfrastructureRef: corev1.ObjectReference{
					APIVersion: agentv1.GroupVersion.String(),
					Kind:       "AgentMachine",
					Name:       "worker",
				},
			},
		}
		infra := &agentv1.AgentMachine{
			ObjectMeta: metav1.ObjectMeta{Namespace: "cp", Name: "worker", UID: "original-provider"},
			Spec:       agentv1.AgentMachineSpec{ProviderID: new("agent://host")},
		}
		Expect(controllerutil.SetControllerReference(machine, infra, capiScheme())).To(Succeed())

		c := fake.NewClientBuilder().
			WithScheme(capiScheme()).
			WithObjects(machine, infra).
			WithStatusSubresource(machine, infra).
			Build()
		r := &MachineReconciler{Client: c, APIReader: c}
		for i, ready := range []bool{false, true, false} {
			Expect(c.Get(ctx, client.ObjectKeyFromObject(infra), infra)).To(Succeed())

			infra.Status.Ready = ready
			if ready {
				conditions.MarkTrue(infra, capiv1.ReadyCondition)
			} else {
				conditions.MarkFalse(
					infra,
					capiv1.ReadyCondition,
					"ProviderWaiting",
					capiv1.ConditionSeverityInfo,
					"Waiting for provider observation",
				)
			}

			Expect(c.Status().Update(ctx, infra)).To(Succeed())
			_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(machine)})
			Expect(err).NotTo(HaveOccurred())
			Expect(c.Get(ctx, client.ObjectKeyFromObject(machine), machine)).To(Succeed())
			Expect(machine.Status.BootstrapReady).To(BeTrue())
			Expect(machine.Status.InfrastructureReady).To(Equal(i > 0))
			Expect(conditions.IsTrue(machine, capiv1.InfrastructureReadyCondition)).To(Equal(ready))
			Expect(conditions.IsTrue(machine, capiv1.ReadyCondition)).To(Equal(ready))

			if i > 0 {
				Expect(machine.Spec.ProviderID).To(Equal(new("agent://host")))
			}
		}
	})
})
