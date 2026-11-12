// Copyright (c) 2026 Red Hat Inc.
// SPDX-License-Identifier: Apache-2.0

package fixtures

import (
	"context"
	"errors"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	bmfv1 "github.com/osac-project/osac/bare-metal-fulfillment-operator/api/v1alpha1"
	osacv1 "github.com/osac-project/osac/osac-operator/api/v1alpha1"
	publicv1 "github.com/osac-project/osac/proto/gen/osac/public/v1"
	"google.golang.org/grpc"
	"k8s.io/apimachinery/pkg/runtime"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/osac-project/osac/tests/integration/caas/internal/harness"
)

// requestReuseConnection supplies the two tenant request lists used before reuse.
type requestReuseConnection struct {
	variant string
	methods []string
}

// Invoke checks read-only, bounded request selection and supplies the selected residue or error.
func (c *requestReuseConnection) Invoke(
	ctx context.Context, method string, request, response any, _ ...grpc.CallOption,
) error {
	c.methods = append(c.methods, method)
	deadline, bounded := ctx.Deadline()
	Expect(bounded).To(BeTrue())
	Expect(time.Until(deadline)).To(BeNumerically(">", 0))
	Expect(time.Until(deadline)).To(BeNumerically("<=", 20*time.Second))

	switch request := request.(type) {
	case *publicv1.ClustersListRequest:
		Expect(method).To(Equal(publicv1.Clusters_List_FullMethodName))
		Expect(request.GetFilter()).To(Equal(`this.metadata.tenant == "tenant"`))
		Expect(request.GetLimit()).To(Equal(int32(1)))

		if c.variant == "cluster error" {
			return errors.New("cluster list unavailable")
		}

		if c.variant == "cluster residue" {
			response.(*publicv1.ClustersListResponse).SetItems([]*publicv1.Cluster{
				publicv1.Cluster_builder{Id: "remaining-cluster"}.Build(),
			})
		}
	case *publicv1.BareMetalInstancesListRequest:
		Expect(method).To(Equal(publicv1.BareMetalInstances_List_FullMethodName))
		Expect(request.GetFilter()).To(Equal(`this.metadata.tenant == "tenant"`))
		Expect(request.GetLimit()).To(Equal(int32(1)))

		if c.variant == "allocation error" {
			return errors.New("allocation list unavailable")
		}

		if c.variant == "allocation residue" {
			response.(*publicv1.BareMetalInstancesListResponse).SetItems([]*publicv1.BareMetalInstance{
				publicv1.BareMetalInstance_builder{Id: "remaining-allocation"}.Build(),
			})
		}
	default:
		return fmt.Errorf("unexpected reuse RPC %s", method)
	}

	return nil
}

// NewStream rejects streaming RPCs because reuse observes bounded unary lists.
func (c *requestReuseConnection) NewStream(
	context.Context, *grpc.StreamDesc, string, ...grpc.CallOption,
) (grpc.ClientStream, error) {
	return nil, errors.New("unexpected streaming reuse RPC")
}

var _ = Describe("Application request reuse checks", func() {
	DescribeTable(
		"rejects remaining requests and failed observations without writes",
		func(ctx SpecContext, variant, failure string, calls int) {
			scheme := runtime.NewScheme()
			Expect(osacv1.AddToScheme(scheme)).To(Succeed())
			Expect(bmfv1.AddToScheme(scheme)).To(Succeed())
			environment := &harness.Environment{
				Namespace: "osac", Kube: kubefake.NewClientset(),
				Client: fake.NewClientBuilder().WithScheme(scheme).Build(),
			}
			connection := &requestReuseConnection{variant: variant}

			err := checkApplicationInputsGone(ctx, environment, connection, "tenant")
			if failure == "" {
				Expect(err).NotTo(HaveOccurred())
			} else {
				Expect(err).To(MatchError(ContainSubstring(failure)))
			}

			Expect(connection.methods).To(HaveLen(calls))
			Expect(connection.methods[0]).To(Equal(publicv1.Clusters_List_FullMethodName))

			if calls == 2 {
				Expect(connection.methods[1]).To(Equal(publicv1.BareMetalInstances_List_FullMethodName))
			}
		},
		Entry("cluster residue", "cluster residue", "fulfillment clusters remain", 1),
		Entry("allocation residue", "allocation residue", "fulfillment bare metal allocations remain", 2),
		Entry("cluster read error", "cluster error", "list Fulfillment clusters: cluster list unavailable", 1),
		Entry("allocation read error", "allocation error",
			"list Fulfillment bare metal allocations: allocation list unavailable", 2),
		Entry("empty requests", "empty", "", 2),
	)
})
