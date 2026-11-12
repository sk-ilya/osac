// Copyright (c) 2026 Red Hat Inc.
// SPDX-License-Identifier: Apache-2.0

package capi

import (
	"context"
	"errors"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	agentv1 "github.com/openshift/cluster-api-provider-agent/api/v1beta1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	capiv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/osac-project/osac/tests/integration/caas/internal/simulator/kube"
)

var _ = Describe("MachineSet workers", func() {
	DescribeTable(
		"checks existing Machines before reading templates or creating clones",
		func(ctx SpecContext, original bool) {
			set := &capiv1.MachineSet{ObjectMeta: metav1.ObjectMeta{Namespace: "cp", Name: "workers", UID: "original-set"}}
			machine := &capiv1.Machine{ObjectMeta: metav1.ObjectMeta{
				Namespace: "cp", Name: "workers-0", UID: "original-machine",
				OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(set, capiv1.GroupVersion.WithKind("MachineSet"))},
			}, Spec: capiv1.MachineSpec{ProviderID: new("agent://installed")}}
			if !original {
				machine.OwnerReferences[0].UID = "another-set"
			}

			c := fake.NewClientBuilder().WithScheme(capiScheme()).WithObjects(machine).Build()
			Expect(c.Get(ctx, client.ObjectKeyFromObject(machine), machine)).To(Succeed())
			before := machine.DeepCopy()
			r := &MachineSetReconciler{Client: c, APIReader: c}

			err := r.createMachine(ctx, set, machine.Name)
			if original {
				Expect(err).NotTo(HaveOccurred())
			} else {
				Expect(err).To(MatchError(ContainSubstring("another MachineSet incarnation")))
			}

			Expect(c.Get(ctx, client.ObjectKeyFromObject(machine), machine)).To(Succeed())
			Expect(machine).To(Equal(before))

			var clones agentv1.AgentMachineList

			Expect(c.List(ctx, &clones)).To(Succeed())
			Expect(clones.Items).To(BeEmpty())
		},
		Entry("original Machine", true),
		Entry("foreign Machine", false),
	)

	DescribeTable("finds children only for the current controller", func(ctx SpecContext, variant string) {
		parent := &capiv1.MachineSet{ObjectMeta: metav1.ObjectMeta{
			Namespace: "control-plane", Name: "workers", UID: "current-parent",
		}}
		current := &capiv1.Machine{ObjectMeta: metav1.ObjectMeta{
			Namespace: parent.Namespace,
			Name:      "current",
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(parent, capiv1.GroupVersion.WithKind("MachineSet")),
			},
		}}
		old := current.DeepCopy()
		old.Name, old.OwnerReferences[0].UID = "old", "old-parent"
		foreign := current.DeepCopy()
		foreign.Name, foreign.OwnerReferences[0].Controller = "foreign", new(false)

		switch variant {
		case "version":
			current.OwnerReferences[0].APIVersion = "cluster.x-k8s.io/v1beta2"
		case "uid":
			parent.UID, current.OwnerReferences[0].UID = "", ""
		}

		c := fake.NewClientBuilder().WithScheme(capiScheme()).WithObjects(parent, current, old, foreign).Build()
		found, err := children(ctx, c, parent)
		Expect(err).NotTo(HaveOccurred())

		if variant == "current" {
			Expect(found).To(HaveLen(1))
			Expect(found[0].Name).To(Equal(current.Name))
		} else {
			Expect(found).To(BeEmpty())
		}
	},
		Entry("matching controller", "current"),
		Entry("another controller API version", "version"),
		Entry("missing parent UID", "uid"),
	)

	It("reuses a pending clone after interrupted Machine creation without duplicating workers", func(ctx SpecContext) {
		By("interrupting the Machine create after its infrastructure clone is persisted")

		cluster := &capiv1.Cluster{
			ObjectMeta: metav1.ObjectMeta{Namespace: "cp", Name: "cluster", UID: "cluster"},
		}
		set := &capiv1.MachineSet{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "cp",
				Name:      "workers",
				UID:       "original-set",
				Labels:    map[string]string{kube.ManagedByLabel: kube.ManagerName},
			},
			Spec: capiv1.MachineSetSpec{
				ClusterName: "cluster", Replicas: new(int32(2)),
				Template: capiv1.MachineTemplateSpec{Spec: capiv1.MachineSpec{
					ClusterName: "cluster",
					InfrastructureRef: corev1.ObjectReference{
						APIVersion: agentv1.GroupVersion.String(), Kind: "AgentMachineTemplate", Namespace: "cp", Name: "workers",
					},
					Bootstrap: capiv1.Bootstrap{DataSecretName: new("bootstrap")},
				}},
			},
		}
		template := &agentv1.AgentMachineTemplate{ObjectMeta: metav1.ObjectMeta{Namespace: "cp", Name: "workers"}}
		failCreate := true
		failure := errors.New("interrupted Machine create")
		created := 0
		c := fake.NewClientBuilder().WithScheme(capiScheme()).WithObjects(set, template, cluster).
			WithStatusSubresource(set).
			WithInterceptorFuncs(interceptor.Funcs{
				Create: func(ctx context.Context, c client.WithWatch, object client.Object, opts ...client.CreateOption) error {
					if _, machine := object.(*capiv1.Machine); machine && failCreate {
						return failure
					}

					// Give each persisted incarnation its own identity, including same-name replacements.
					created++
					object.SetUID(types.UID(fmt.Sprintf("created-%d", created)))

					return c.Create(ctx, object, opts...)
				},
			}).Build()
		r := &MachineSetReconciler{Client: c, APIReader: c}
		request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(set)}
		_, err := r.Reconcile(ctx, request)
		Expect(errors.Is(err, failure)).To(BeTrue(), "original failure lost: %v", err)
		Expect(c.Get(ctx, request.NamespacedName, set)).To(Succeed())
		Expect(kube.MatchesOwner(set, cluster, capiv1.GroupVersion.WithKind("Cluster"))).To(BeTrue())
		Expect(metav1.GetControllerOf(set)).To(BeNil(), "standalone MachineSets use ordinary Cluster ownership")

		pending := &agentv1.AgentMachine{}
		Expect(c.Get(ctx, client.ObjectKey{Namespace: "cp", Name: "workers-0"}, pending)).To(Succeed())
		Expect(pending.UID).NotTo(BeEmpty())
		Expect(metav1.GetControllerOf(pending)).To(BeNil())
		Expect(pending.OwnerReferences).NotTo(BeEmpty())
		Expect(pending.OwnerReferences[0].UID).To(Equal(set.UID))

		By("refusing to reuse the pending clone for a replacement MachineSet")

		missing := &capiv1.Machine{}
		Expect(apierrors.IsNotFound(c.Get(ctx, client.ObjectKeyFromObject(pending), missing))).To(BeTrue())
		beforeClone := pending.DeepCopy()
		replacement := set.DeepCopy()
		replacement.UID = "replacement-set"
		restarted := &MachineSetReconciler{Client: c, APIReader: c}
		err = restarted.createMachine(ctx, replacement, pending.Name)
		Expect(err).To(MatchError(ContainSubstring("AgentMachine workers-0 belongs to another MachineSet incarnation")))

		current := &agentv1.AgentMachine{}
		Expect(c.Get(ctx, client.ObjectKeyFromObject(pending), current)).To(Succeed())
		Expect(current.UID).To(Equal(beforeClone.UID))
		Expect(current.OwnerReferences).To(Equal(beforeClone.OwnerReferences))

		By("recovering from native state through a fresh reconciler")

		failCreate = false
		_, err = restarted.Reconcile(ctx, request)
		Expect(err).NotTo(HaveOccurred())

		var machines capiv1.MachineList

		Expect(c.List(ctx, &machines, client.InNamespace("cp"))).To(Succeed())
		Expect(machines.Items).To(HaveLen(2))

		for i := range machines.Items {
			Expect(machines.Items[i].Spec.InfrastructureRef.Kind).To(Equal("AgentMachine"))
			Expect(machines.Items[i].Spec.InfrastructureRef.Name).To(Equal(machines.Items[i].Name))
		}

		var clones agentv1.AgentMachineList

		Expect(c.List(ctx, &clones, client.InNamespace("cp"))).To(Succeed())
		Expect(clones.Items).To(HaveLen(2))

		recovered := &agentv1.AgentMachine{}
		Expect(c.Get(ctx, client.ObjectKeyFromObject(pending), recovered)).To(Succeed())
		Expect(recovered.UID).To(Equal(pending.UID), "recovery must reuse the original pending clone")

		By("leaving existing Machine inputs and provider observations unchanged")

		worker := &machines.Items[0]
		worker.Spec.ProviderID = new("agent://installed")
		worker.Annotations = map[string]string{"foreign.example/retained": "yes"}
		Expect(c.Update(ctx, worker)).To(Succeed())
		before := worker.DeepCopy()
		Expect(restarted.createMachine(ctx, set, worker.Name)).To(Succeed())
		Expect(c.Get(ctx, client.ObjectKeyFromObject(worker), worker)).To(Succeed())
		Expect(worker).To(Equal(before))
	})
})
