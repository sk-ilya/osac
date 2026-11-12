// Copyright (c) 2026 Red Hat Inc.
// SPDX-License-Identifier: Apache-2.0

package hypershift

import (
	"crypto/tls"
	"encoding/base64"
	"encoding/json"

	ignitiontypes "github.com/coreos/ignition/v2/config/v3_2/types"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	hypershiftv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	capiv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/osac-project/osac/tests/integration/caas/internal/simulator/kube"
)

// ignitionInputs supplies independently published endpoint and certificate inputs.
// The symbolic certificate tests resource dependencies without making a TLS connection.
func ignitionInputs(pool *hypershiftv1.NodePool, cluster *capiv1.Cluster) []client.Object {
	hc := agentHostedCluster()
	hc.Status.IgnitionEndpoint = "ignition.example.invalid"
	pool.Spec.ClusterName = hc.Name

	if cluster.Annotations == nil {
		cluster.Annotations = map[string]string{}
	}

	cluster.Annotations[kube.HostedClusterAnnotation] = hc.Namespace + "/" + hc.Name
	cluster.Spec.ControlPlaneRef = &corev1.ObjectReference{
		APIVersion: hypershiftv1.GroupVersion.String(),
		Kind:       "HostedControlPlane",
		Name:       hc.Name,
	}
	cp := &hypershiftv1.HostedControlPlane{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: cluster.Namespace, Name: hc.Name, UID: "original-control-plane",
			Labels:      map[string]string{kube.ManagedByLabel: kube.ManagerName},
			Annotations: map[string]string{kube.HostedClusterAnnotation: hc.Namespace + "/" + hc.Name},
		},
	}
	ca := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: cluster.Namespace, Name: "ignition-ca-cert"},
		Data:       map[string][]byte{corev1.TLSCertKey: []byte("certificate")},
	}
	Expect(controllerutil.SetControllerReference(cp, ca, hypershiftScheme())).To(Succeed())

	return []client.Object{hc, cp, ca}
}

var _ = Describe("Ignition publication inputs", func() {
	It("keeps a valid signing pair across retries and recovery", func(ctx SpecContext) {
		cp := &hypershiftv1.HostedControlPlane{
			ObjectMeta: metav1.ObjectMeta{Namespace: "cp", Name: "cluster", UID: "original"},
		}
		c := fake.NewClientBuilder().WithScheme(hypershiftScheme()).WithObjects(cp).Build()
		r := &HostedClusterReconciler{Client: c, APIReader: c}
		Expect(r.ensureIgnitionCA(ctx, cp)).To(Succeed())

		ca := &corev1.Secret{}
		Expect(c.Get(ctx, client.ObjectKey{Namespace: cp.Namespace, Name: "ignition-ca-cert"}, ca)).To(Succeed())
		_, err := tls.X509KeyPair(ca.Data[corev1.TLSCertKey], ca.Data[corev1.TLSPrivateKeyKey])
		Expect(err).NotTo(HaveOccurred())

		before := ca.DeepCopy()
		restarted := &HostedClusterReconciler{Client: c, APIReader: c}
		Expect(restarted.ensureIgnitionCA(ctx, cp)).To(Succeed())
		Expect(c.Get(ctx, client.ObjectKeyFromObject(ca), ca)).To(Succeed())
		Expect(ca).To(Equal(before))

		cp.UID = "replacement"
		Expect(restarted.ensureIgnitionCA(ctx, cp)).NotTo(Succeed())
		Expect(c.Get(ctx, client.ObjectKeyFromObject(ca), ca)).To(Succeed())
		Expect(ca).To(Equal(before))
	})

	DescribeTable("requires each management-side bootstrap dependency", func(ctx SpecContext, missing string) {
		pool := &hypershiftv1.NodePool{ObjectMeta: metav1.ObjectMeta{Namespace: "hub", Name: "workers", UID: "pool"}}
		cluster := &capiv1.Cluster{ObjectMeta: metav1.ObjectMeta{Namespace: "cp", Name: "cluster", UID: "cluster"}}
		objects := ignitionInputs(pool, cluster)

		switch missing {
		case "endpoint":
			objects[0].(*hypershiftv1.HostedCluster).Status.IgnitionEndpoint = ""
		case "CA":
			objects = objects[:2]
		case "certificate":
			objects[2].(*corev1.Secret).Data = nil
		case "CA owner":
			objects[2].SetOwnerReferences(nil)
		}

		c := fake.NewClientBuilder().WithScheme(hypershiftScheme()).WithObjects(objects...).Build()
		r := &NodePoolReconciler{Client: c, APIReader: c}
		_, err := r.bootstrapData(ctx, pool, cluster)
		Expect(err).To(HaveOccurred())

		var secrets corev1.SecretList

		Expect(c.List(ctx, &secrets, client.MatchingLabels{kube.ManagedByLabel: kube.ManagerName})).To(Succeed())
		Expect(secrets.Items).To(BeEmpty(), "missing inputs must not produce a token or bootstrap output")
	},
		Entry("endpoint", "endpoint"),
		Entry("CA Secret", "CA"),
		Entry("CA contents", "certificate"),
		Entry("original CA owner", "CA owner"),
	)

	It("builds authenticated Ignition from the endpoint, certificate and stable native token", func(ctx SpecContext) {
		pool := &hypershiftv1.NodePool{ObjectMeta: metav1.ObjectMeta{Namespace: "hub", Name: "workers", UID: "pool"}}
		cluster := &capiv1.Cluster{ObjectMeta: metav1.ObjectMeta{Namespace: "cp", Name: "cluster", UID: "cluster"}}
		c := fake.NewClientBuilder().WithScheme(hypershiftScheme()).WithObjects(ignitionInputs(pool, cluster)...).Build()
		r := &NodePoolReconciler{Client: c, APIReader: c}
		data, err := r.bootstrapData(ctx, pool, cluster)
		Expect(err).NotTo(HaveOccurred())

		var config ignitiontypes.Config

		Expect(json.Unmarshal(data, &config)).To(Succeed())
		Expect(config.Ignition.Config.Merge).To(HaveLen(1))
		Expect(config.Ignition.Config.Merge[0].Source).To(Equal(new("https://ignition.example.invalid/ignition")))

		token := &corev1.Secret{}
		Expect(c.Get(ctx, client.ObjectKey{Namespace: "cp", Name: "workers-token"}, token)).To(Succeed())
		Expect(token.Data["token"]).NotTo(BeEmpty())
		Expect(
			config.Ignition.Config.Merge[0].HTTPHeaders[0].Value,
		).To(Equal(new("Bearer " + base64.StdEncoding.EncodeToString(token.Data["token"]))))
		Expect(
			config.Ignition.Security.TLS.CertificateAuthorities[0].Source,
		).To(Equal(new("data:text/plain;base64," + base64.StdEncoding.EncodeToString([]byte("certificate")))))

		restarted := &NodePoolReconciler{Client: c, APIReader: c}
		recovered, err := restarted.bootstrapData(ctx, pool, cluster)
		Expect(err).NotTo(HaveOccurred())
		Expect(recovered).To(Equal(data))
	})
})
