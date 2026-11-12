// Copyright (c) 2026 Red Hat Inc.
// SPDX-License-Identifier: Apache-2.0

package cpo

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	hypershiftv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/osac-project/osac/tests/integration/caas/internal/simulator/kube"
)

var _ = Describe("Control-plane responses", func() {
	It("publishes a parseable placeholder kubeconfig without guest credentials", func() {
		config, err := clientcmd.Load([]byte(placeholderGuestKubeconfig))
		Expect(err).NotTo(HaveOccurred())
		Expect(clientcmd.Validate(*config)).To(Succeed())
		current := config.Contexts[config.CurrentContext]
		Expect(current).NotTo(BeNil())
		Expect(config.Clusters[current.Cluster].Server).To(Equal("https://guest.caas-it.invalid:6443"))
		data, err := json.Marshal(config.AuthInfos[current.AuthInfo])
		Expect(err).NotTo(HaveOccurred())
		Expect(string(data)).To(Equal("{}"))
	})

	It(
		"recovers availability on the original HCP while retaining worker inputs and guest observations",
		func(ctx SpecContext) {
			By("preparing a held control plane with an independent guest condition")

			scheme := runtime.NewScheme()
			Expect(corev1.AddToScheme(scheme)).To(Succeed())
			Expect(hypershiftv1.AddToScheme(scheme)).To(Succeed())

			controlPlane := &hypershiftv1.HostedControlPlane{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "control-plane", Name: "cluster", UID: "original-control-plane",
					Labels: map[string]string{kube.ManagedByLabel: kube.ManagerName},
				},
				Spec: hypershiftv1.HostedControlPlaneSpec{
					Services: []hypershiftv1.ServicePublishingStrategyMapping{
						{
							Service:                   hypershiftv1.APIServer,
							ServicePublishingStrategy: hypershiftv1.ServicePublishingStrategy{Type: hypershiftv1.LoadBalancer},
						},
					},
					Platform: hypershiftv1.PlatformSpec{
						Type:  hypershiftv1.NonePlatform,
						Agent: &hypershiftv1.AgentPlatformSpec{AgentNamespace: "hub"},
					},
				},
			}
			meta.SetStatusCondition(&controlPlane.Status.Conditions, metav1.Condition{
				Type: "ClusterVersionFailing", Status: metav1.ConditionFalse, Reason: "GuestObserved",
				Message: "Simulated provider observation", ObservedGeneration: 1,
			})
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(controlPlane).
				WithStatusSubresource(controlPlane, &corev1.Service{}).Build()
			available := false

			var readError error

			r := &HostedControlPlaneReconciler{
				Client: c, APIReader: c, Scope: kube.Scope{AgentNamespaces: []string{"hub"}},
				Availability: func(_ context.Context, observed *hypershiftv1.HostedControlPlane) (bool, bool, error) {
					Expect(observed.UID).To(Equal(controlPlane.UID))

					return available, true, readError
				},
			}
			request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(controlPlane)}

			By("creating the API Service while leaving its load-balancer status to its owner")

			result, err := r.Reconcile(ctx, request)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(time.Second))

			service := &corev1.Service{}
			Expect(
				c.Get(ctx, client.ObjectKey{Namespace: controlPlane.Namespace, Name: "kube-apiserver"}, service),
			).To(Succeed())
			Expect(service.Status.LoadBalancer.Ingress).To(BeEmpty())

			By("observing the load balancer and publishing worker prerequisites before availability")

			nextService := service.DeepCopy()
			nextService.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{IP: "192.0.2.10"}}
			Expect(c.Status().Patch(ctx, nextService,
				client.MergeFromWithOptions(service, client.MergeFromWithOptimisticLock{}))).To(Succeed())
			_, err = r.Reconcile(ctx, request)
			Expect(err).NotTo(HaveOccurred())

			held := &hypershiftv1.HostedControlPlane{}
			Expect(c.Get(ctx, request.NamespacedName, held)).To(Succeed())
			Expect(held.Status.Ready).To(BeFalse())
			Expect(held.Status.Initialized).To(BeTrue())
			Expect(held.Status.ControlPlaneEndpoint.Host).NotTo(BeEmpty())
			Expect(held.Status.KubeConfig).NotTo(BeNil())
			Expect(held.Status.KubeConfig.Name).NotTo(BeEmpty())

			access := &corev1.Secret{}
			Expect(
				c.Get(ctx, client.ObjectKey{Namespace: held.Namespace, Name: held.Status.KubeConfig.Name}, access),
			).To(Succeed())
			access.Data["foreign"] = []byte("retained")
			Expect(c.Update(ctx, access)).To(Succeed())

			By("releasing availability without replacing the HCP or losing the guest condition")

			available = true
			result, err = r.Reconcile(ctx, request)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(time.Second))

			ready := &hypershiftv1.HostedControlPlane{}
			Expect(c.Get(ctx, request.NamespacedName, ready)).To(Succeed())
			Expect(ready.UID).To(Equal(controlPlane.UID))
			Expect(c.Get(ctx, client.ObjectKeyFromObject(access), access)).To(Succeed())
			Expect(access.Data["foreign"]).To(Equal([]byte("retained")))
			Expect(meta.IsStatusConditionTrue(ready.Status.Conditions, "Available")).To(BeTrue())
			Expect(meta.IsStatusConditionFalse(ready.Status.Conditions, "ClusterVersionFailing")).To(BeTrue())

			By("applying a later availability hold to the ready control plane")

			available = false
			result, err = r.Reconcile(ctx, request)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(time.Second))

			held = &hypershiftv1.HostedControlPlane{}
			Expect(c.Get(ctx, request.NamespacedName, held)).To(Succeed())
			Expect(held.UID).To(Equal(controlPlane.UID))
			Expect(held.Status.Initialized).To(BeTrue())
			Expect(meta.IsStatusConditionFalse(held.Status.Conditions, "Available")).To(BeTrue())
			Expect(meta.IsStatusConditionFalse(held.Status.Conditions, "ClusterVersionFailing")).To(BeTrue())

			By("surfacing failures to read the live availability response")

			readError = errors.New("settings unavailable")
			_, err = r.Reconcile(ctx, request)
			Expect(errors.Is(err, readError)).To(BeTrue(), "original failure lost: %v", err)
		},
	)

	It("keeps an older control plane's Service and Secret unchanged", func(ctx SpecContext) {
		scheme := runtime.NewScheme()
		Expect(corev1.AddToScheme(scheme)).To(Succeed())
		Expect(hypershiftv1.AddToScheme(scheme)).To(Succeed())

		current := &hypershiftv1.HostedControlPlane{
			ObjectMeta: metav1.ObjectMeta{Namespace: "cp", Name: "cluster", UID: "new"},
		}
		previous := current.DeepCopy()
		previous.UID = "old"

		service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "cp", Name: "kube-apiserver"}}
		secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "cp", Name: "cluster-admin-kubeconfig"}}

		Expect(controllerutil.SetControllerReference(previous, service, scheme)).To(Succeed())
		Expect(controllerutil.SetControllerReference(previous, secret, scheme)).To(Succeed())
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(service, secret).Build()
		r := &HostedControlPlaneReconciler{Client: c, APIReader: c}

		_, err := r.ensureAPIService(ctx, current)
		Expect(err).To(MatchError(ContainSubstring("another control plane")))
		_, err = r.ensureKubeconfig(ctx, current)
		Expect(err).To(MatchError(ContainSubstring("another control plane")))

		afterService := &corev1.Service{}
		afterSecret := &corev1.Secret{}

		Expect(c.Get(ctx, client.ObjectKeyFromObject(service), afterService)).To(Succeed())
		Expect(c.Get(ctx, client.ObjectKeyFromObject(secret), afterSecret)).To(Succeed())
		Expect(afterService).To(Equal(service))
		Expect(afterSecret).To(Equal(secret))
	})

	DescribeTable(
		"converges native API publication while retaining Service allocations",
		func(ctx SpecContext, kind hypershiftv1.PublishingStrategyType) {
			scheme := runtime.NewScheme()
			Expect(corev1.AddToScheme(scheme)).To(Succeed())
			Expect(hypershiftv1.AddToScheme(scheme)).To(Succeed())

			cp := &hypershiftv1.HostedControlPlane{
				ObjectMeta: metav1.ObjectMeta{Namespace: "cp", Name: "cluster", UID: "original"},
			}

			strategy := hypershiftv1.ServicePublishingStrategy{Type: kind}
			if kind == hypershiftv1.NodePort {
				strategy.NodePort = &hypershiftv1.NodePortPublishingStrategy{Address: "node.example.invalid", Port: 32000}
			} else {
				strategy.LoadBalancer = &hypershiftv1.LoadBalancerPublishingStrategy{Hostname: "configured.example.invalid"}
			}

			cp.Spec.Services = []hypershiftv1.ServicePublishingStrategyMapping{
				{Service: hypershiftv1.APIServer, ServicePublishingStrategy: strategy},
			}
			service := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Namespace:  "cp",
					Name:       "kube-apiserver",
					UID:        "original-service",
					Finalizers: []string{"foreign.example/cleanup"},
				},
				Spec: corev1.ServiceSpec{
					ClusterIP:  "10.0.0.10",
					ClusterIPs: []string{"10.0.0.10"},
					Ports:      []corev1.ServicePort{{Name: "api", NodePort: 31443, Port: 443, Protocol: corev1.ProtocolUDP}},
				},
				Status: corev1.ServiceStatus{
					LoadBalancer: corev1.LoadBalancerStatus{
						Ingress: []corev1.LoadBalancerIngress{{Hostname: "observed.example.invalid", IP: "192.0.2.10"}},
					},
				},
			}
			Expect(controllerutil.SetControllerReference(cp, service, scheme)).To(Succeed())
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(service).WithStatusSubresource(service).Build()
			r := &HostedControlPlaneReconciler{Client: c, APIReader: c}
			observed, err := r.ensureAPIService(ctx, cp)
			Expect(err).NotTo(HaveOccurred())
			Expect(observed.Spec.ClusterIP).To(Equal("10.0.0.10"))
			Expect(observed.Spec.ClusterIPs).To(ConsistOf("10.0.0.10"))
			Expect(observed.Spec.Ports[0].NodePort).To(Equal(int32(31443)))
			Expect(observed.Spec.Ports[0].Port).To(Equal(int32(6443)))
			Expect(observed.Spec.Ports[0].Protocol).To(Equal(corev1.ProtocolTCP))
			Expect(observed.Spec.Ports[0].TargetPort.StrVal).To(Equal("client"))
			Expect(observed.Finalizers).To(ConsistOf("foreign.example/cleanup"))

			host, port := apiEndpoint(cp, observed)
			if kind == hypershiftv1.NodePort {
				Expect(observed.Spec.Type).To(Equal(corev1.ServiceTypeNodePort))
				Expect(host).To(Equal("node.example.invalid"))
				Expect(port).To(Equal(int32(31443)))
			} else {
				Expect(observed.Spec.Type).To(Equal(corev1.ServiceTypeLoadBalancer))
				Expect(host).To(Equal("configured.example.invalid"))
				Expect(port).To(Equal(int32(6443)))

				strategy := &cp.Spec.Services[0].ServicePublishingStrategy
				strategy.LoadBalancer.Hostname = ""
				host, _ = apiEndpoint(cp, observed)
				Expect(host).To(Equal("observed.example.invalid"))

				observed.Status.LoadBalancer.Ingress[0].Hostname = ""
				host, _ = apiEndpoint(cp, observed)
				Expect(host).To(Equal("192.0.2.10"))
			}
		},
		Entry("load balancer", hypershiftv1.LoadBalancer),
		Entry("node port", hypershiftv1.NodePort),
	)

	It("releases only its None-platform cleanup barrier after a restart", func(ctx SpecContext) {
		scheme := runtime.NewScheme()
		Expect(corev1.AddToScheme(scheme)).To(Succeed())
		Expect(hypershiftv1.AddToScheme(scheme)).To(Succeed())

		cp := &hypershiftv1.HostedControlPlane{
			ObjectMeta: metav1.ObjectMeta{
				Namespace:  "cp",
				Name:       "cluster",
				UID:        "original",
				Labels:     map[string]string{kube.ManagedByLabel: kube.ManagerName},
				Finalizers: []string{"foreign.example/cleanup"},
			},
			Spec: hypershiftv1.HostedControlPlaneSpec{
				Platform: hypershiftv1.PlatformSpec{
					Type:  hypershiftv1.NonePlatform,
					Agent: &hypershiftv1.AgentPlatformSpec{AgentNamespace: "hub"},
				},
			},
		}
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cp).Build()
		reads := 0
		r := &HostedControlPlaneReconciler{
			Client:    c,
			APIReader: c,
			Scope:     kube.Scope{AgentNamespaces: []string{"hub"}},
			Availability: func(context.Context, *hypershiftv1.HostedControlPlane) (bool, bool, error) {
				reads++
				return false, false, nil
			},
		}
		request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cp)}
		_, err := r.Reconcile(ctx, request)
		Expect(err).NotTo(HaveOccurred())
		Expect(c.Get(ctx, request.NamespacedName, cp)).To(Succeed())
		Expect(cp.Finalizers).To(ContainElement(controlPlaneFinalizer))
		Expect(c.Delete(ctx, cp)).To(Succeed())
		restarted := &HostedControlPlaneReconciler{Client: c, APIReader: c, Scope: r.Scope, Availability: r.Availability}
		_, err = restarted.Reconcile(ctx, request)
		Expect(err).NotTo(HaveOccurred())
		Expect(reads).To(Equal(1), "cleanup must be independent of current case controls")
		Expect(c.Get(ctx, request.NamespacedName, cp)).To(Succeed())
		Expect(cp.Finalizers).To(ConsistOf("foreign.example/cleanup"))
	})
})
