// Copyright (c) 2026 Red Hat Inc.
// SPDX-License-Identifier: Apache-2.0

package assisted

import (
	"context"
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	assistedv1 "github.com/openshift/assisted-service/api/v1beta1"
	conditionsv1 "github.com/openshift/custom-resource-status/conditions/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/osac-project/osac/tests/integration/caas/internal/simulator/kube"
)

// discoveryPullSecret supplies a valid native authentication input without live credentials.
func discoveryPullSecret(namespace string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "pull-secret"},
		Type:       corev1.SecretTypeDockerConfigJson,
		Data: map[string][]byte{
			corev1.DockerConfigJsonKey: []byte(`{"auths":{"example.invalid":{"auth":"dGVzdDp0ZXN0"}}}`),
		},
	}
}

var _ = Describe("InfraEnv discovery publication", func() {
	var (
		infra  *assistedv1.InfraEnv
		secret *corev1.Secret
	)

	BeforeEach(func() {
		infra = &assistedv1.InfraEnv{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "agents", Name: "discovery", UID: "original",
				Finalizers: []string{"foreign.example/cleanup"}, Annotations: map[string]string{"retained": "yes"},
			},
			Spec: assistedv1.InfraEnvSpec{PullSecretRef: &corev1.LocalObjectReference{Name: "pull-secret"}},
		}
		infra.Status.BootArtifacts.InitrdURL = "https://example.invalid/initrd"
		secret = discoveryPullSecret(infra.Namespace)
	})

	DescribeTable(
		"publishes artifacts only after validating native inputs",
		func(ctx SpecContext, variant string) {
			switch variant {
			case "missing reference":
				infra.Spec.PullSecretRef = nil
			case "missing secret":
				secret.Name = "other"
			case "invalid secret":
				secret.Data[corev1.DockerConfigJsonKey] = []byte(`invalid`)
			case "empty auths":
				secret.Data[corev1.DockerConfigJsonKey] = []byte(`{"auths":{}}`)
			}

			c := fake.NewClientBuilder().
				WithScheme(agentScheme()).
				WithObjects(infra, secret).
				WithStatusSubresource(infra).
				Build()
			r := &InfraEnvReconciler{
				Client: c, APIReader: c, Scope: kube.Scope{AgentNamespaces: []string{"agents"}},
				DiscoveryURL: func(context.Context, *assistedv1.InfraEnv) (string, error) {
					return "http://ignition.release.svc/ignition.json", nil
				},
			}
			request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(infra)}

			_, err := r.Reconcile(ctx, request)
			if variant == "valid" {
				Expect(err).NotTo(HaveOccurred())
			} else {
				Expect(err).To(HaveOccurred())
			}

			current := &assistedv1.InfraEnv{}
			Expect(c.Get(ctx, request.NamespacedName, current)).To(Succeed())
			Expect(current.UID).To(Equal(infra.UID))
			Expect(current.Spec).To(Equal(infra.Spec))
			Expect(current.Annotations).To(Equal(infra.Annotations))
			Expect(current.Finalizers).To(ConsistOf("foreign.example/cleanup", infraEnvFinalizer))
			Expect(current.Status.BootArtifacts.InitrdURL).To(Equal(infra.Status.BootArtifacts.InitrdURL))

			if variant == "valid" {
				Expect(current.Status.BootArtifacts.DiscoveryIgnitionURL).To(Equal("http://ignition.release.svc/ignition.json"))
				Expect(conditionsv1.IsStatusConditionTrue(current.Status.Conditions, assistedv1.ImageCreatedCondition)).To(BeTrue())
			} else {
				Expect(current.Status.BootArtifacts.DiscoveryIgnitionURL).To(BeEmpty())
				Expect(
					conditionsv1.IsStatusConditionFalse(current.Status.Conditions, assistedv1.ImageCreatedCondition),
				).To(BeTrue())
			}

			By("settling without rewriting status or metadata")

			before := current.DeepCopy()

			_, err = r.Reconcile(ctx, request)
			if variant == "valid" {
				Expect(err).NotTo(HaveOccurred())
			} else {
				Expect(err).To(HaveOccurred())
			}

			Expect(c.Get(ctx, request.NamespacedName, current)).To(Succeed())
			Expect(current).To(Equal(before))
		},
		Entry("valid discovery inputs", "valid"),
		Entry("missing pull-secret reference", "missing reference"),
		Entry(
			"missing credentials",
			"missing secret",
		),
		Entry("malformed credentials", "invalid secret"),
		Entry("empty authentication map", "empty auths"),
	)

	It("keeps retrying a credential read failure until the API recovers", func(ctx SpecContext) {
		failure := errors.New("credential read unavailable")
		refuse := true
		c := fake.NewClientBuilder().WithScheme(agentScheme()).WithObjects(infra, secret).
			WithStatusSubresource(infra).WithInterceptorFuncs(interceptor.Funcs{
			Get: func(
				ctx context.Context, c client.WithWatch, key client.ObjectKey,
				object client.Object, opts ...client.GetOption,
			) error {
				if _, credential := object.(*corev1.Secret); credential && refuse {
					return failure
				}

				return c.Get(ctx, key, object, opts...)
			},
		}).Build()

		r := &InfraEnvReconciler{
			Client: c, APIReader: c, Scope: kube.Scope{AgentNamespaces: []string{"agents"}},
			DiscoveryURL: func(context.Context, *assistedv1.InfraEnv) (string, error) {
				return "http://ignition.release.svc/ignition.json", nil
			},
		}
		request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(infra)}
		for range 2 {
			_, err := r.Reconcile(ctx, request)
			Expect(err).To(MatchError(failure))
		}

		Expect(c.Get(ctx, request.NamespacedName, infra)).To(Succeed())
		Expect(conditionsv1.IsStatusConditionFalse(infra.Status.Conditions, assistedv1.ImageCreatedCondition)).To(BeTrue())

		refuse = false
		_, err := r.Reconcile(ctx, request)
		Expect(err).NotTo(HaveOccurred())
		Expect(c.Get(ctx, request.NamespacedName, infra)).To(Succeed())
		Expect(infra.Status.BootArtifacts.DiscoveryIgnitionURL).NotTo(BeEmpty())
		Expect(conditionsv1.IsStatusConditionTrue(infra.Status.Conditions, assistedv1.ImageCreatedCondition)).To(BeTrue())
	})

	It("cleans its own finalizer and preserves foreign cleanup", func(ctx SpecContext) {
		infra.Finalizers = append(infra.Finalizers, infraEnvFinalizer)
		c := fake.NewClientBuilder().WithScheme(agentScheme()).WithObjects(infra).Build()
		Expect(c.Delete(ctx, infra)).To(Succeed())
		r := &InfraEnvReconciler{Client: c, APIReader: c, Scope: kube.Scope{AgentNamespaces: []string{"agents"}}}
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(infra)})
		Expect(err).NotTo(HaveOccurred())
		Expect(c.Get(ctx, client.ObjectKeyFromObject(infra), infra)).To(Succeed())
		Expect(infra.Finalizers).To(ConsistOf("foreign.example/cleanup"))
	})

	It("ignores environments outside served namespaces", func(ctx SpecContext) {
		c := fake.NewClientBuilder().WithScheme(agentScheme()).WithObjects(infra).Build()
		r := &InfraEnvReconciler{Client: c, APIReader: c, Scope: kube.Scope{AgentNamespaces: []string{"foreign"}}}
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(infra)})
		Expect(err).NotTo(HaveOccurred())

		current := &assistedv1.InfraEnv{}
		Expect(c.Get(ctx, client.ObjectKeyFromObject(infra), current)).To(Succeed())
		Expect(current.Finalizers).To(Equal(infra.Finalizers))
		Expect(current.Status).To(Equal(infra.Status))
	})

	DescribeTable("preserves API failures", func(ctx SpecContext, operation string) {
		failure := errors.New("API unavailable")
		c := fake.NewClientBuilder().WithScheme(agentScheme()).WithObjects(infra, secret).WithStatusSubresource(infra).
			WithInterceptorFuncs(interceptor.Funcs{
				Get: func(
					ctx context.Context,
					c client.WithWatch,
					key client.ObjectKey,
					object client.Object,
					opts ...client.GetOption,
				) error {
					if operation == "read" {
						return failure
					}

					return c.Get(ctx, key, object, opts...)
				},
				SubResourcePatch: func(
					context.Context,
					client.Client,
					string,
					client.Object,
					client.Patch,
					...client.SubResourcePatchOption,
				) error {
					return failure
				},
			}).Build()
		r := &InfraEnvReconciler{
			Client: c, APIReader: c, Scope: kube.Scope{AgentNamespaces: []string{"agents"}},
			DiscoveryURL: func(context.Context, *assistedv1.InfraEnv) (string, error) {
				return "http://ignition.release.svc/ignition.json", nil
			},
		}
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(infra)})
		Expect(errors.Is(err, failure)).To(BeTrue())
	}, Entry("reading the environment", "read"), Entry("publishing status", "status"))
})
