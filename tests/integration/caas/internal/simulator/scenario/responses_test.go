// Copyright (c) 2026 Red Hat Inc.
// SPDX-License-Identifier: Apache-2.0

package scenario

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	assistedv1 "github.com/openshift/assisted-service/api/v1beta1"
	hypershiftv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	kubefake "k8s.io/client-go/kubernetes/fake"
	capiv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/osac-project/osac/tests/integration/caas/internal/simulator/assisted"
	"github.com/osac-project/osac/tests/integration/caas/internal/simulator/kube"
)

var _ = Describe("Native component response inputs", func() {
	Context("with two independently configured cases", func() {
		var (
			a, b         *corev1.ConfigMap
			cluster      *capiv1.Cluster
			hc           *hypershiftv1.HostedCluster
			infra, other *assistedv1.InfraEnv
			cp           *hypershiftv1.HostedControlPlane
			c            client.Client
			responses    Responses
		)

		BeforeEach(func() {
			a = &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name: "case-a", Namespace: "settings",
					Labels: map[string]string{
						settingsLabel:         "true",
						infraEnvUIDLabel:      "infra-a",
						hostedClusterUIDLabel: "hc-a",
						clusterUIDLabel:       "cluster",
					},
				},
				Data: map[string]string{
					configMapKey:    string(Normal),
					discoveryURLKey: "http://ignition.release.svc/p1/ignition.json",
				},
			}
			b = a.DeepCopy()
			b.Name = "case-b"
			b.Labels[infraEnvUIDLabel] = "infra-b"
			b.Labels[hostedClusterUIDLabel] = "hc-b"
			b.Labels[clusterUIDLabel] = "cluster-b"
			b.Data[discoveryURLKey] = "http://ignition.release.svc/p2/ignition.json"
			foreign := a.DeepCopy()
			foreign.Namespace = "other"
			hc = &hypershiftv1.HostedCluster{
				ObjectMeta: metav1.ObjectMeta{Namespace: "hub", Name: "cluster", UID: "hc-a"},
				Spec:       hypershiftv1.HostedClusterSpec{InfraID: "cluster-infrastructure"},
			}
			cluster = &capiv1.Cluster{ObjectMeta: metav1.ObjectMeta{
				Namespace: "hub-cluster", Name: "cluster-infrastructure", UID: "cluster",
				Annotations: map[string]string{kube.HostedClusterAnnotation: "hub/cluster"},
			}}

			cp = &hypershiftv1.HostedControlPlane{ObjectMeta: metav1.ObjectMeta{
				Namespace: "hub-cluster",
				Name:      "control-plane",
				OwnerReferences: []metav1.OwnerReference{
					*metav1.NewControllerRef(cluster, capiv1.GroupVersion.WithKind("Cluster")),
				},
			}}
			cluster.Spec.ControlPlaneRef = &corev1.ObjectReference{
				APIVersion: hypershiftv1.GroupVersion.String(), Kind: "HostedControlPlane",
				Namespace: cp.Namespace, Name: cp.Name,
			}

			infra = &assistedv1.InfraEnv{ObjectMeta: metav1.ObjectMeta{Namespace: "hub", Name: "discovery", UID: "infra-a"}}
			other = infra.DeepCopy()
			other.SetUID("infra-b")

			scheme := runtime.NewScheme()
			Expect(corev1.AddToScheme(scheme)).To(Succeed())
			Expect(capiv1.AddToScheme(scheme)).To(Succeed())
			Expect(hypershiftv1.AddToScheme(scheme)).To(Succeed())

			c = fake.NewClientBuilder().WithScheme(scheme).WithObjects(a, b, foreign, hc, cluster).Build()
			responses = Responses{Reader: c, Namespace: "settings"}
		})

		DescribeTable(
			"rejects a mismatched control-plane reference",
			func(ctx SpecContext, change func(*corev1.ObjectReference)) {
				Expect(c.Get(ctx, client.ObjectKeyFromObject(cluster), cluster)).To(Succeed())
				change(cluster.Spec.ControlPlaneRef)
				Expect(c.Update(ctx, cluster)).To(Succeed())

				_, _, err := responses.Availability(ctx, cp)
				Expect(err).To(MatchError(ContainSubstring("control plane differs from its original Cluster")))
			},
			Entry("API version", func(ref *corev1.ObjectReference) { ref.APIVersion = "other.example/v1" }),
			Entry("kind", func(ref *corev1.ObjectReference) { ref.Kind = "OtherControlPlane" }),
			Entry("namespace", func(ref *corev1.ObjectReference) { ref.Namespace = "other" }),
			Entry("UID", func(ref *corev1.ObjectReference) { ref.UID = "other-control-plane" }),
		)

		DescribeTable("accepts an optional matching control-plane UID", func(ctx SpecContext, uid types.UID) {
			cp.UID = "original-control-plane"

			Expect(c.Get(ctx, client.ObjectKeyFromObject(cluster), cluster)).To(Succeed())
			cluster.Spec.ControlPlaneRef.UID = uid
			Expect(c.Update(ctx, cluster)).To(Succeed())

			available, found, err := responses.Availability(ctx, cp)
			Expect(err).NotTo(HaveOccurred())
			Expect(found).To(BeTrue())
			Expect(available).To(BeTrue())
		},
			Entry("unset UID", types.UID("")),
			Entry("original UID", types.UID("original-control-plane")),
		)

		DescribeTable(
			"observes live controls without coupling components or cases",
			func(ctx SpecContext, profile Profile, installation assisted.InstallationResponse, available, holdDetach bool) {
				By("reading the original settings before changing the selected case")

				initial, err := responses.Installation(ctx, infra)
				Expect(err).NotTo(HaveOccurred())
				Expect(initial).To(Equal(assisted.InstallationComplete))

				endpoint, err := responses.DiscoveryURL(ctx, infra)
				Expect(err).NotTo(HaveOccurred())
				Expect(endpoint).To(Equal(a.Data[discoveryURLKey]))

				By("changing the profile and invalidating only the discovery URL")
				Expect(c.Get(ctx, client.ObjectKeyFromObject(a), a)).To(Succeed())
				a.Data[configMapKey] = string(profile)
				a.Data[discoveryURLKey] = "invalid-discovery-url"
				Expect(c.Update(ctx, a)).To(Succeed())

				By("routing each component to its own response despite the discovery error")

				got, err := responses.Installation(ctx, infra)
				Expect(err).NotTo(HaveOccurred())
				Expect(got).To(Equal(installation))

				actualAvailability, found, err := responses.Availability(ctx, cp)
				Expect(err).NotTo(HaveOccurred())
				Expect(found).To(BeTrue())
				Expect(actualAvailability).To(Equal(available))

				actualHold, err := responses.HoldDetach(ctx, cluster)
				Expect(err).NotTo(HaveOccurred())
				Expect(actualHold).To(Equal(holdDetach))

				_, err = responses.DiscoveryURL(ctx, infra)
				Expect(err).To(HaveOccurred())

				By("leaving the other case's discovery and installation unchanged")

				endpoint, err = responses.DiscoveryURL(ctx, other)
				Expect(err).NotTo(HaveOccurred())
				Expect(endpoint).To(Equal(b.Data[discoveryURLKey]))

				otherInstallation, err := responses.Installation(ctx, other)
				Expect(err).NotTo(HaveOccurred())
				Expect(otherInstallation).To(Equal(assisted.InstallationComplete))
			},
			Entry("normal progress", Normal, assisted.InstallationComplete, true, false),
			Entry("installation held", InstallationStalled, assisted.InstallationHeld, true, false),
			Entry("installation failed", InstallationFailed, assisted.InstallationFailed, true, false),
			Entry("detach held", DetachStalled, assisted.InstallationComplete, true, true),
			Entry("control-plane availability held", ControlPlaneStalled, assisted.InstallationComplete, false, false),
		)

		It("uses the HostedCluster name when infraID is omitted", func(ctx SpecContext) {
			Expect(c.Get(ctx, client.ObjectKeyFromObject(hc), hc)).To(Succeed())
			hc.Spec.InfraID = ""
			Expect(c.Update(ctx, hc)).To(Succeed())
			cluster.SetName(hc.GetName())
			Expect(c.Get(ctx, client.ObjectKeyFromObject(a), a)).To(Succeed())
			a.Data[configMapKey] = string(DetachStalled)
			Expect(c.Update(ctx, a)).To(Succeed())
			hold, err := responses.HoldDetach(ctx, cluster)
			Expect(err).NotTo(HaveOccurred())
			Expect(hold).To(BeTrue())
		})

		It("waits when the linked HostedCluster is absent", func(ctx SpecContext) {
			Expect(c.Delete(ctx, hc)).To(Succeed())
			_, found, err := responses.Availability(ctx, cp)
			Expect(err).NotTo(HaveOccurred())
			Expect(found).To(BeFalse())

			hold, err := responses.HoldDetach(ctx, cluster)
			Expect(err).NotTo(HaveOccurred())
			Expect(hold).To(BeFalse())
		})

		It("waits when the Cluster has no case binding", func(ctx SpecContext) {
			Expect(c.Get(ctx, client.ObjectKeyFromObject(a), a)).To(Succeed())
			delete(a.Labels, clusterUIDLabel)
			Expect(c.Update(ctx, a)).To(Succeed())

			_, found, err := responses.Availability(ctx, cp)
			Expect(err).NotTo(HaveOccurred())
			Expect(found).To(BeFalse())

			hold, err := responses.HoldDetach(ctx, cluster)
			Expect(err).NotTo(HaveOccurred())
			Expect(hold).To(BeFalse())
		})

		It("ignores replacement HostedCluster settings for a surviving Cluster", func(ctx SpecContext) {
			Expect(c.Delete(ctx, hc)).To(Succeed())
			hc.UID = "hc-b"
			hc.ResourceVersion = ""
			Expect(c.Create(ctx, hc)).To(Succeed())
			Expect(c.Get(ctx, client.ObjectKeyFromObject(b), b)).To(Succeed())
			b.Data[configMapKey] = string(DetachStalled)
			Expect(c.Update(ctx, b)).To(Succeed())

			_, found, err := responses.Availability(ctx, cp)
			Expect(err).NotTo(HaveOccurred())
			Expect(found).To(BeFalse())

			hold, err := responses.HoldDetach(ctx, cluster)
			Expect(err).NotTo(HaveOccurred())
			Expect(hold).To(BeFalse())
		})

		It("rejects duplicate case bindings for a Cluster UID", func(ctx SpecContext) {
			Expect(c.Get(ctx, client.ObjectKeyFromObject(b), b)).To(Succeed())
			b.Labels[clusterUIDLabel] = string(cluster.UID)
			Expect(c.Update(ctx, b)).To(Succeed())

			_, _, err := responses.Availability(ctx, cp)
			Expect(err).To(MatchError(ContainSubstring("multiple settings match native root")))
			_, err = responses.HoldDetach(ctx, cluster)
			Expect(err).To(MatchError(ContainSubstring("multiple settings match native root")))
		})

		It("selects settings by the live HostedCluster UID", func(ctx SpecContext) {
			Expect(c.Get(ctx, client.ObjectKeyFromObject(a), a)).To(Succeed())
			a.Labels[hostedClusterUIDLabel] = "unrelated-root"
			Expect(c.Update(ctx, a)).To(Succeed())
			_, found, err := responses.Availability(ctx, cp)
			Expect(err).NotTo(HaveOccurred())
			Expect(found).To(BeFalse())

			hold, err := responses.HoldDetach(ctx, cluster)
			Expect(err).NotTo(HaveOccurred())
			Expect(hold).To(BeFalse())
		})

		DescribeTable("rejects invalid native HostedCluster links", func(ctx SpecContext, link string) {
			cluster.SetAnnotations(map[string]string{kube.HostedClusterAnnotation: link})
			_, err := responses.HoldDetach(ctx, cluster)
			Expect(err).To(HaveOccurred())
		},
			Entry("missing link", ""),
			Entry("missing namespace", "/cluster"),
			Entry("missing name", "hub/"),
		)

		It("rejects a Cluster outside the linked control-plane namespace", func(ctx SpecContext) {
			cluster.SetNamespace("other-cluster")
			_, err := responses.HoldDetach(ctx, cluster)
			Expect(err).To(HaveOccurred())
		})

		It("rejects a Cluster with another infrastructure name", func(ctx SpecContext) {
			cluster.SetName("other-infrastructure")
			_, err := responses.HoldDetach(ctx, cluster)
			Expect(err).To(HaveOccurred())
		})

		It("rejects a replacement Cluster as the source of an old control plane's settings", func(ctx SpecContext) {
			By("confirming that the original Cluster supplies availability")

			_, found, err := responses.Availability(ctx, cp)
			Expect(err).NotTo(HaveOccurred())
			Expect(found).To(BeTrue())

			By("replacing the Cluster while retaining its name")
			Expect(c.Get(ctx, client.ObjectKeyFromObject(cluster), cluster)).To(Succeed())
			Expect(c.Delete(ctx, cluster)).To(Succeed())
			cluster.SetUID("replacement-cluster")
			cluster.SetResourceVersion("")
			Expect(c.Create(ctx, cluster)).To(Succeed())
			_, _, err = responses.Availability(ctx, cp)
			Expect(err).To(HaveOccurred())
		})
	})

	It("prevents a second case from binding a surviving Cluster", func(ctx SpecContext) {
		first := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
			Namespace: "settings", Name: "case-a", UID: "case-a",
			Labels: map[string]string{settingsLabel: "true", clusterUIDLabel: "cluster"},
		}}
		second := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
			Namespace: "settings", Name: "case-b", UID: "case-b",
			Labels: map[string]string{settingsLabel: "true"},
		}}
		api := kubefake.NewClientset(first, second)
		other := &Scenario{Kube: api, Namespace: "settings", Name: "case-b", uid: second.UID}

		Expect(other.BindCluster(ctx, types.UID("cluster"))).To(MatchError(ContainSubstring("bound to another case")))
		stored, err := api.CoreV1().ConfigMaps("settings").Get(ctx, second.Name, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(stored.Labels).NotTo(HaveKey(clusterUIDLabel))

		Expect(other.BindCluster(ctx, types.UID("cluster-b"))).To(Succeed())
		err = other.BindCluster(ctx, types.UID("replacement-cluster"))
		Expect(err).To(MatchError(ContainSubstring("incarnation changed")))
	})

	Context("without a valid unique binding", func() {
		var (
			infra     *assistedv1.InfraEnv
			c         client.Client
			responses Responses
		)

		BeforeEach(func() {
			infra = &assistedv1.InfraEnv{ObjectMeta: metav1.ObjectMeta{Namespace: "hub", Name: "discovery", UID: "infra"}}

			c = fake.NewClientBuilder().Build()
			responses = Responses{Reader: c, Namespace: "settings"}
		})

		It("waits without supplying discovery or installation progress when settings are missing", func(ctx SpecContext) {
			installation, err := responses.Installation(ctx, infra)
			Expect(err).NotTo(HaveOccurred())
			Expect(installation).To(BeZero())

			endpoint, err := responses.DiscoveryURL(ctx, infra)
			Expect(err).NotTo(HaveOccurred())
			Expect(endpoint).To(BeEmpty())
		})

		It("rejects an empty native root UID", func(ctx SpecContext) {
			infra.SetUID("")
			_, err := responses.Installation(ctx, infra)
			Expect(err).To(HaveOccurred())
		})

		It("rejects invalid settings without assuming normal progress", func(ctx SpecContext) {
			settings := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name: "case", Namespace: "settings",
					Labels: map[string]string{settingsLabel: "true", infraEnvUIDLabel: "infra"},
				},
			}
			Expect(c.Create(ctx, settings)).To(Succeed())
			_, err := responses.Installation(ctx, infra)
			Expect(err).To(HaveOccurred())
		})

		It("rejects ambiguous bindings even when either binding is valid on its own", func(ctx SpecContext) {
			settings := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name: "case", Namespace: "settings",
					Labels: map[string]string{settingsLabel: "true", infraEnvUIDLabel: "infra"},
				},
				Data: map[string]string{configMapKey: string(Normal)},
			}
			Expect(c.Create(ctx, settings)).To(Succeed())
			installation, err := responses.Installation(ctx, infra)
			Expect(err).NotTo(HaveOccurred())
			Expect(installation).To(Equal(assisted.InstallationComplete))

			duplicate := settings.DeepCopy()
			duplicate.Name = "duplicate"
			duplicate.ResourceVersion = ""
			Expect(c.Create(ctx, duplicate)).To(Succeed())
			_, err = responses.Installation(ctx, infra)
			Expect(err).To(MatchError(ContainSubstring("multiple settings match native root")))
		})
	})
})
