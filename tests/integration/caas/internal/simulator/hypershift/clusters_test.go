// Copyright (c) 2026 Red Hat Inc.
// SPDX-License-Identifier: Apache-2.0

package hypershift

import (
	"context"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	installv1 "github.com/openshift/assisted-service/api/hiveextension/v1beta1"
	agentv1 "github.com/openshift/cluster-api-provider-agent/api/v1beta1"
	hivev1 "github.com/openshift/hive/apis/hive/v1"
	hypershiftv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	capiv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/osac-project/osac/tests/integration/caas/internal/simulator/kube"
)

// hypershiftScheme registers provider inputs and status records used by these tests.
func hypershiftScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		corev1.AddToScheme, hypershiftv1.AddToScheme, capiv1.AddToScheme,
		agentv1.AddToScheme, hivev1.AddToScheme, installv1.AddToScheme,
	} {
		Expect(add(scheme)).To(Succeed())
	}

	return scheme
}

// agentHostedCluster supplies a served cluster with a stable identity.
func agentHostedCluster() *hypershiftv1.HostedCluster {
	return &hypershiftv1.HostedCluster{
		ObjectMeta: metav1.ObjectMeta{Namespace: "hub", Name: "cluster", UID: "original-cluster"},
		Spec: hypershiftv1.HostedClusterSpec{
			PullSecret: corev1.LocalObjectReference{Name: "source-pull-secret"},
			Services: []hypershiftv1.ServicePublishingStrategyMapping{
				{
					Service:                   hypershiftv1.Ignition,
					ServicePublishingStrategy: hypershiftv1.ServicePublishingStrategy{Type: hypershiftv1.Route},
				},
			},
			Platform: hypershiftv1.PlatformSpec{
				Type:  hypershiftv1.AgentPlatform,
				Agent: &hypershiftv1.AgentPlatformSpec{AgentNamespace: "hub"},
			},
		},
	}
}

var _ = Describe("HostedCluster observations", func() {
	It("waits for independent control-plane outputs and follows their published values", func(ctx SpecContext) {
		By("creating inputs without producing CPO status or access credentials")

		cluster := agentHostedCluster()
		pullSecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: cluster.Namespace, Name: cluster.Spec.PullSecret.Name},
			Data: map[string][]byte{
				corev1.DockerConfigJsonKey: []byte(`{"auths":{"example.invalid":{"auth":"dGVzdDp0ZXN0"}}}`),
			},
		}
		c := fake.NewClientBuilder().WithScheme(hypershiftScheme()).WithObjects(cluster, pullSecret).
			WithStatusSubresource(cluster, &hypershiftv1.HostedControlPlane{}).
			WithInterceptorFuncs(interceptor.Funcs{Create: func(
				ctx context.Context,
				c client.WithWatch,
				object client.Object,
				opts ...client.CreateOption,
			) error {
				object.SetUID(types.UID(fmt.Sprintf("%T-%s", object, object.GetName())))
				return c.Create(ctx, object, opts...)
			}}).
			Build()
		r := &HostedClusterReconciler{Client: c, APIReader: c, Scope: kube.Scope{AgentNamespaces: []string{"hub"}}}
		request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cluster)}
		result, err := r.Reconcile(ctx, request)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).NotTo(BeZero())

		cp := &hypershiftv1.HostedControlPlane{}
		Expect(c.Get(ctx, client.ObjectKey{Namespace: "hub-cluster", Name: "cluster"}, cp)).To(Succeed())
		Expect(cp.Status).To(Equal(hypershiftv1.HostedControlPlaneStatus{}))
		Expect(cp.Annotations).To(Equal(map[string]string{kube.HostedClusterAnnotation: "hub/cluster"}))
		Expect(cp.Spec.Platform.Type).To(Equal(hypershiftv1.NonePlatform))
		Expect(cp.OwnerReferences).To(BeEmpty())

		namespace := &corev1.Namespace{}
		Expect(c.Get(ctx, client.ObjectKey{Name: cp.Namespace}, namespace)).To(Succeed())
		Expect(namespace.Annotations).To(Equal(map[string]string{kube.HostedClusterAnnotation: "hub/cluster"}))

		native := &capiv1.Cluster{}
		Expect(c.Get(ctx, client.ObjectKey{Namespace: cp.Namespace, Name: "cluster"}, native)).To(Succeed())
		Expect(native.Annotations).To(Equal(map[string]string{kube.HostedClusterAnnotation: "hub/cluster"}))
		By("observing independent CAPI adoption of the original provider")
		Expect(controllerutil.SetControllerReference(native, cp, c.Scheme())).To(Succeed())
		Expect(c.Update(ctx, cp)).To(Succeed())

		for _, object := range []client.Object{
			&corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: cp.Namespace, Name: "kube-apiserver"}},
			&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: cp.Namespace, Name: "cluster-admin-kubeconfig"}},
		} {
			err := c.Get(ctx, client.ObjectKeyFromObject(object), object)
			Expect(apierrors.IsNotFound(err)).To(BeTrue(), "HostedCluster must leave %T creation to CPO", object)
		}

		By("supplying an independent access Secret with a declared source key")

		source := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: cp.Namespace, Name: "published-access"},
			Data:       map[string][]byte{"published": []byte("published")},
		}
		Expect(controllerutil.SetControllerReference(cp, source, c.Scheme())).To(Succeed())
		Expect(c.Create(ctx, source)).To(Succeed())

		for _, step := range []struct {
			description string
			available   metav1.ConditionStatus
		}{
			{"observing worker prerequisites while control-plane availability is held", metav1.ConditionFalse},
			{"observing availability on the same HostedCluster after CPO releases it", metav1.ConditionTrue},
		} {
			By(step.description)

			Expect(c.Get(ctx, client.ObjectKeyFromObject(cp), cp)).To(Succeed())

			cp.Status.KubeConfig = &hypershiftv1.KubeconfigSecretRef{Name: source.Name, Key: "published"}
			cp.Status.ControlPlaneEndpoint = hypershiftv1.APIEndpoint{Host: "192.0.2.99", Port: 6443}
			for _, kind := range []string{"Available", "KubeAPIServerAvailable"} {
				meta.SetStatusCondition(&cp.Status.Conditions, metav1.Condition{
					Type: kind, Status: step.available, Reason: "CPOObserved", ObservedGeneration: 1,
				})
			}

			meta.SetStatusCondition(&cp.Status.Conditions, metav1.Condition{
				Type: "ClusterVersionFailing", Status: metav1.ConditionFalse, Reason: "GuestObserved", ObservedGeneration: 1,
			})
			Expect(c.Status().Update(ctx, cp)).To(Succeed())
			_, err = r.Reconcile(ctx, request)
			Expect(err).NotTo(HaveOccurred())

			observed := &hypershiftv1.HostedCluster{}
			Expect(c.Get(ctx, request.NamespacedName, observed)).To(Succeed())
			Expect(observed.UID).To(Equal(cluster.UID))
			Expect(meta.IsStatusConditionPresentAndEqual(observed.Status.Conditions, "Available", step.available)).To(BeTrue())
			Expect(meta.IsStatusConditionTrue(observed.Status.Conditions, "ClusterVersionSucceeding")).To(BeTrue())
			Expect(observed.Status.ControlPlaneEndpoint.Host).To(Equal("192.0.2.99"))

			copied := &corev1.Secret{}
			Expect(
				c.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: "cluster-admin-kubeconfig"}, copied),
			).To(Succeed())
			Expect(copied.Data["kubeconfig"]).To(Equal([]byte("published")))

			infra := &agentv1.AgentCluster{}
			Expect(c.Get(ctx, client.ObjectKey{Namespace: cp.Namespace, Name: cp.Name}, infra)).To(Succeed())
		}

		By("waiting for published access when the source Secret disappears")

		Expect(c.Delete(ctx, source)).To(Succeed())
		result, err = r.Reconcile(ctx, request)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).NotTo(BeZero())

		observed := &hypershiftv1.HostedCluster{}
		Expect(c.Get(ctx, request.NamespacedName, observed)).To(Succeed())
		Expect(observed.Status.KubeConfig).To(BeNil())
		Expect(
			meta.IsStatusConditionTrue(observed.Status.Conditions, "Available"),
		).To(BeTrue(), "HC availability follows HCP availability independently of the access copy")

		By("mapping a source Secret change back to its HostedCluster")

		requests := r.hostedClusterForChild(ctx, source)
		Expect(requests).To(HaveLen(1))
		Expect(requests[0].NamespacedName).To(Equal(request.NamespacedName))
	})

	It("waits for a terminating namespace without directly removing provider records", func(ctx SpecContext) {
		cluster := agentHostedCluster()
		cluster.Finalizers, cluster.DeletionTimestamp = []string{hostedClusterFinalizer}, new(metav1.Now())
		namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
			Name: "hub-cluster", UID: "control-plane-namespace", Finalizers: []string{"foreign.example/cleanup"},
			Labels:      map[string]string{kube.ManagedByLabel: kube.ManagerName},
			Annotations: map[string]string{kube.HostedClusterAnnotation: "hub/cluster"},
		}}
		deployment := &hivev1.ClusterDeployment{ObjectMeta: metav1.ObjectMeta{Namespace: namespace.Name, Name: "cluster"}}
		installation := &installv1.AgentClusterInstall{
			ObjectMeta: metav1.ObjectMeta{Namespace: namespace.Name, Name: "cluster"},
		}
		c := fake.NewClientBuilder().
			WithScheme(hypershiftScheme()).
			WithObjects(cluster, namespace, deployment, installation).
			Build()
		r := &HostedClusterReconciler{Client: c, APIReader: c, Scope: kube.Scope{AgentNamespaces: []string{"hub"}}}
		result, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cluster)})
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).NotTo(BeZero())

		for _, object := range []client.Object{cluster, deployment, installation} {
			Expect(
				c.Get(ctx, client.ObjectKeyFromObject(object), object),
			).To(Succeed(), "HostedCluster must wait for owning controllers")
		}
	})
})
