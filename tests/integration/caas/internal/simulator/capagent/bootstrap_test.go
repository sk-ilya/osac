// Copyright (c) 2026 Red Hat Inc.
// SPDX-License-Identifier: Apache-2.0

package capagent

import (
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	capiv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

var _ = Describe("Agent bootstrap authorization", func() {
	It("preserves an unauthenticated source without manufacturing credentials", func(ctx SpecContext) {
		source := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: "cp", Name: "bootstrap"},
			Data: map[string][]byte{
				"value": []byte(`{"ignition":{"config":{"merge":[{"source":"https://ignition.invalid/worker"}]}}}`),
			},
		}
		machine := &capiv1.Machine{
			ObjectMeta: metav1.ObjectMeta{Namespace: "cp", Name: "worker"},
			Spec:       capiv1.MachineSpec{Bootstrap: capiv1.Bootstrap{DataSecretName: &source.Name}},
		}
		c := fake.NewClientBuilder().WithScheme(capagentScheme()).WithObjects(source).Build()
		r := &AgentMachineReconciler{Client: c, APIReader: c}
		pool, token, headers, err := r.bootstrap(ctx, machine)
		Expect(err).NotTo(HaveOccurred())
		Expect(pool).To(Equal("worker"))
		Expect(token).To(BeNil())
		Expect(headers).To(BeEmpty())

		var outputs corev1.SecretList

		Expect(c.List(ctx, &outputs, client.InNamespace("cp"))).To(Succeed())
		Expect(outputs.Items).To(HaveLen(1), "unauthenticated bootstrap must not create an empty token Secret")
	})

	It("shares an unowned token and refreshes it after the bootstrap input is replaced", func(ctx SpecContext) {
		By("preparing one authenticated bootstrap input for two workers")

		source := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: "cp", Name: "workers-bootstrap", UID: "source-input"},
			Data: map[string][]byte{"value": []byte(
				`{"ignition":{"config":{"merge":[{"source":"https://ignition.invalid/worker",` +
					`"httpHeaders":[{"name":"Authorization","value":"Bearer test-only"}]}]}}}`,
			)},
		}
		c := fake.NewClientBuilder().WithScheme(capagentScheme()).WithObjects(source).Build()
		r := &AgentMachineReconciler{Client: c, APIReader: c}

		for _, name := range []string{"worker-0", "worker-1"} {
			machine := &capiv1.Machine{
				ObjectMeta: metav1.ObjectMeta{Namespace: "cp", Name: name, UID: types.UID("uid-" + name)},
				Spec:       capiv1.MachineSpec{Bootstrap: capiv1.Bootstrap{DataSecretName: &source.Name}},
			}
			_, ref, _, err := r.bootstrap(ctx, machine)
			Expect(err).NotTo(HaveOccurred())
			Expect(ref).NotTo(BeNil())
			Expect(ref.Namespace).To(Equal(source.Namespace))
			Expect(ref.Name).To(Equal("agent-" + source.Name))
		}

		By("keeping the shared output independent of the source Secret lifecycle")

		token := &corev1.Secret{}
		Expect(c.Get(ctx, client.ObjectKey{Namespace: "cp", Name: "agent-workers-bootstrap"}, token)).To(Succeed())
		Expect(token.OwnerReferences).To(BeEmpty())
		Expect(token.Data).To(HaveKeyWithValue("ignition-token", []byte("test-only")))

		By("refreshing the shared credentials after the source is replaced")

		Expect(c.Delete(ctx, source)).To(Succeed())
		source.ResourceVersion, source.UID = "", "replacement-input"
		source.Data["value"] = []byte(strings.ReplaceAll(string(source.Data["value"]), "test-only", "replacement-token"))
		Expect(c.Create(ctx, source)).To(Succeed())
		replacement := &capiv1.Machine{
			ObjectMeta: metav1.ObjectMeta{Namespace: "cp", Name: "replacement"},
			Spec:       capiv1.MachineSpec{Bootstrap: capiv1.Bootstrap{DataSecretName: &source.Name}},
		}
		_, ref, _, err := r.bootstrap(ctx, replacement)
		Expect(err).NotTo(HaveOccurred())
		Expect(ref.Name).To(Equal(token.Name))
		Expect(c.Get(ctx, client.ObjectKeyFromObject(token), token)).To(Succeed())
		Expect(token.OwnerReferences).To(BeEmpty())
		Expect(token.Data).To(HaveKeyWithValue("ignition-token", []byte("replacement-token")))
	})

	It("refreshes an existing token while preserving its other data and metadata", func(ctx SpecContext) {
		source := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "cp", Name: "bootstrap", UID: "source"}}
		owner := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "cp", Name: "other", UID: "other"}}
		token := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "cp",
				Name:      "agent-bootstrap",
				OwnerReferences: []metav1.OwnerReference{
					*metav1.NewControllerRef(owner, corev1.SchemeGroupVersion.WithKind("Secret")),
				},
				Annotations: map[string]string{"retained": "yes"},
			},
			Data: map[string][]byte{"ignition-token": []byte("old"), "retained": []byte("value")},
		}
		c := fake.NewClientBuilder().WithScheme(capagentScheme()).WithObjects(token).Build()
		r := &AgentMachineReconciler{Client: c, APIReader: c}
		_, err := r.ensureToken(ctx, source, "new")
		Expect(err).NotTo(HaveOccurred())
		Expect(c.Get(ctx, client.ObjectKeyFromObject(token), token)).To(Succeed())
		Expect(
			token.OwnerReferences,
		).To(ConsistOf(*metav1.NewControllerRef(owner, corev1.SchemeGroupVersion.WithKind("Secret"))))
		Expect(token.Annotations).To(HaveKeyWithValue("retained", "yes"))
		Expect(token.Data).To(Equal(map[string][]byte{"ignition-token": []byte("new"), "retained": []byte("value")}))
		before := token.DeepCopy()
		_, err = r.ensureToken(ctx, source, "new")
		Expect(err).NotTo(HaveOccurred())
		Expect(c.Get(ctx, client.ObjectKeyFromObject(token), token)).To(Succeed())
		Expect(token).To(Equal(before))
	})
})
