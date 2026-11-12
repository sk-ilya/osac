// Copyright (c) 2026 Red Hat Inc.
// SPDX-License-Identifier: Apache-2.0

package hypershift

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	hypershiftv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	capiv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

var _ = Describe("HyperShift input watches", func() {
	It("maps an unowned pull Secret only to referencing HostedClusters in its namespace", func(ctx SpecContext) {
		hc := agentHostedCluster()
		other := hc.DeepCopy()
		other.Name, other.UID = "other", "other-cluster"
		other.Spec.PullSecret.Name = "different-secret"
		foreign := hc.DeepCopy()
		foreign.Namespace, foreign.UID = "other-hub", "foreign-cluster"
		secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: hc.Namespace, Name: hc.Spec.PullSecret.Name}}
		c := fake.NewClientBuilder().WithScheme(hypershiftScheme()).WithObjects(hc, other, foreign).Build()
		r := &HostedClusterReconciler{Client: c, APIReader: c}
		Expect(
			r.hostedClustersForSecret(ctx, secret),
		).To(ConsistOf(ctrl.Request{NamespacedName: client.ObjectKeyFromObject(hc)}))
	})

	DescribeTable("maps the original shared CA to its HostedCluster's pools", func(ctx SpecContext, variant string) {
		pool := &hypershiftv1.NodePool{ObjectMeta: metav1.ObjectMeta{Namespace: "hub", Name: "workers", UID: "pool"}}
		cluster := &capiv1.Cluster{ObjectMeta: metav1.ObjectMeta{Namespace: "cp", Name: "cluster", UID: "cluster"}}
		objects := ignitionInputs(pool, cluster)
		ca := objects[2].(*corev1.Secret)
		other := pool.DeepCopy()
		other.Name, other.UID, other.Spec.ClusterName = "other", "other-pool", "different-cluster"
		foreign := pool.DeepCopy()
		foreign.Namespace, foreign.UID = "other-hub", "foreign-pool"
		c := fake.NewClientBuilder().
			WithScheme(hypershiftScheme()).
			WithObjects(append(objects, pool, other, foreign)...).
			Build()

		switch variant {
		case "UID":
			ca.OwnerReferences[0].UID = "previous-control-plane"
		case "name":
			ca.OwnerReferences[0].Name = "another-control-plane"
		case "version":
			ca.OwnerReferences[0].APIVersion = "example.com/v1"
		case "unrelated":
			ca.Name = "another-secret"
		}

		r := &NodePoolReconciler{Client: c, APIReader: c}

		requests := r.poolsForSecret(ctx, ca)
		if variant == "current" {
			Expect(requests).To(ConsistOf(ctrl.Request{NamespacedName: client.ObjectKeyFromObject(pool)}))
		} else {
			Expect(requests).To(BeEmpty())
		}
	}, Entry("current CA", "current"), Entry("old owner UID", "UID"), Entry("other owner name", "name"),
		Entry("other owner version", "version"), Entry("unrelated Secret", "unrelated"))

	It("maps a deleted NodePool from the event without a live API object", func(ctx SpecContext) {
		pool := &hypershiftv1.NodePool{
			ObjectMeta: metav1.ObjectMeta{Namespace: "hub", Name: "workers"},
			Spec:       hypershiftv1.NodePoolSpec{ClusterName: "cluster"},
		}
		Expect(hostedClusterForPool(ctx, pool)).To(ConsistOf(ctrl.Request{
			NamespacedName: client.ObjectKey{Namespace: "hub", Name: "cluster"},
		}))
	})
})
