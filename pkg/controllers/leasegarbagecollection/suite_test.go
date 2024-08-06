/*
Copyright The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package leasegarbagecollection_test

import (
	"context"
	"testing"
	"time"

	"sigs.k8s.io/karpenter/pkg/test/v1alpha1"

	"github.com/samber/lo"
	coordinationsv1 "k8s.io/api/coordination/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"sigs.k8s.io/karpenter/pkg/apis"
	"sigs.k8s.io/karpenter/pkg/controllers/leasegarbagecollection"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/test"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	. "sigs.k8s.io/karpenter/pkg/utils/testing"

	. "sigs.k8s.io/karpenter/pkg/test/expectations"
)

var ctx context.Context
var env *test.Environment
var garbageCollectionController *leasegarbagecollection.Controller

func TestAPIs(t *testing.T) {
	ctx = TestContextWithLogger(t)
	RegisterFailHandler(Fail)
	RunSpecs(t, "LeaseGarbageCollection")
}

var _ = BeforeSuite(func() {
	env = test.NewEnvironment(test.WithCRDs(apis.CRDs...), test.WithCRDs(v1alpha1.CRDs...))
	ctx = options.ToContext(ctx, test.Options())

	garbageCollectionController = leasegarbagecollection.NewController(env.Client)
})

var _ = AfterSuite(func() {
	Expect(env.Stop()).To(Succeed(), "Failed to stop environment")
})

var _ = AfterEach(func() {
	ExpectCleanedUp(ctx, env.Client)
})

var _ = Describe("GarbageCollection", func() {
	var goodLease *coordinationsv1.Lease
	var badLease *coordinationsv1.Lease
	BeforeEach(func() {
		node := test.Node(test.NodeOptions{})
		ExpectApplied(ctx, env.Client, node)
		node = ExpectExists(ctx, env.Client, node)
		goodLease = &coordinationsv1.Lease{
			ObjectMeta: v1.ObjectMeta{
				CreationTimestamp: v1.Time{Time: time.Now().Add(-time.Hour * 2)},
				Name:              "new-lease",
				Namespace:         "kube-node-lease",
				Labels:            map[string]string{test.DiscoveryLabel: "unspecified"},
				OwnerReferences: []v1.OwnerReference{
					{
						APIVersion: "v1",
						Name:       "owner-node",
						Kind:       "Node",
						UID:        node.UID,
					},
				},
			},
		}
		badLease = &coordinationsv1.Lease{
			ObjectMeta: v1.ObjectMeta{
				CreationTimestamp: v1.Time{Time: time.Now().Add(-time.Hour * 2)},
				Name:              "new-lease",
				Namespace:         "kube-node-lease",
				Labels:            map[string]string{test.DiscoveryLabel: "unspecified"},
			},
		}
	})
	AfterEach(func() {
		// Reset the metrics collectors
		leasegarbagecollection.NodeLeasesDeletedTotal.Reset()
	})
	Context("Metrics", func() {
		It("should fire the leaseDeletedCounter metric when deleting leases", func() {
			ExpectApplied(ctx, env.Client, badLease)
			ExpectObjectReconciled(ctx, env.Client, garbageCollectionController, badLease)
			ExpectNotFound(ctx, env.Client, badLease)

			m, ok := FindMetricWithLabelValues("karpenter_nodes_leases_deleted_total", map[string]string{})
			Expect(ok).To(BeTrue())
			Expect(lo.FromPtr(m.GetCounter().Value)).To(BeNumerically("==", 1.0))

		})
	})
	It("should not delete node lease that contains an OwnerReference", func() {
		ExpectApplied(ctx, env.Client, goodLease)
		ExpectObjectReconciled(ctx, env.Client, garbageCollectionController, goodLease)
		ExpectExists(ctx, env.Client, goodLease)
	})
	It("should delete node lease that does not contain an OwnerReference", func() {
		ExpectApplied(ctx, env.Client, badLease)
		ExpectObjectReconciled(ctx, env.Client, garbageCollectionController, badLease)
		ExpectNotFound(ctx, env.Client, badLease)
	})
	It("should not delete node lease that does not contain OwnerReference in a outside of kube-node-lease namespace", func() {
		badLease.Namespace = "kube-system"
		ExpectApplied(ctx, env.Client, badLease)
		ExpectObjectReconciled(ctx, env.Client, garbageCollectionController, badLease)
		ExpectExists(ctx, env.Client, badLease)
	})
})
