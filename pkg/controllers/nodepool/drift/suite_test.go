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

package drift_test

import (
	"context"
	"testing"

	"github.com/awslabs/operatorpkg/object"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"sigs.k8s.io/karpenter/pkg/apis"
	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
	"sigs.k8s.io/karpenter/pkg/controllers/nodepool/drift"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
	"sigs.k8s.io/karpenter/pkg/test/v1alpha1"
	. "sigs.k8s.io/karpenter/pkg/utils/testing"
)

var (
	controller    *drift.Controller
	ctx           context.Context
	env           *test.Environment
	cloudProvider *fake.CloudProvider
	nodePool      *v1.NodePool
	nodeClass     *v1alpha1.TestNodeClass
)

func TestAPIs(t *testing.T) {
	ctx = TestContextWithLogger(t)
	RegisterFailHandler(Fail)
	RunSpecs(t, "Drift")
}

var _ = BeforeSuite(func() {
	cloudProvider = fake.NewCloudProvider()
	env = test.NewEnvironment(test.WithCRDs(apis.CRDs...), test.WithCRDs(v1alpha1.CRDs...))
	controller = drift.NewController(env.Client, cloudProvider)
})

var _ = AfterSuite(func() {
	Expect(env.Stop()).To(Succeed(), "Failed to stop environment")
})

var _ = AfterEach(func() {
	ExpectCleanedUp(ctx, env.Client)
})

var _ = Describe("Drift", func() {
	BeforeEach(func() {
		nodePool = test.NodePool()
		nodeClass = test.NodeClass(v1alpha1.TestNodeClass{
			ObjectMeta: metav1.ObjectMeta{Name: nodePool.Spec.Template.Spec.NodeClassRef.Name},
		})
		nodePool.Spec.Template.Spec.NodeClassRef.Group = object.GVK(nodeClass).Group
		nodePool.Spec.Template.Spec.NodeClassRef.Kind = object.GVK(nodeClass).Kind
		_ = nodePool.StatusConditions().Clear(v1.ConditionTypeNodeClaimsDrifted)
	})

	It("should not set Drifted if there are no managed nodeclaims", func() {
		ExpectApplied(ctx, env.Client, nodePool, nodeClass)
		ExpectObjectReconciled(ctx, env.Client, controller, nodePool)
		nodePool = ExpectExists(ctx, env.Client, nodePool)
		Expect(nodePool.StatusConditions().Get(v1.ConditionTypeNodeClaimsDrifted).IsFalse()).To(BeTrue())
	})

	It("should not set Drifted if the drifted condition of nodeclaim is nil ", func() {
		nodeClaim := test.NodeClaim(v1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
				v1.NodePoolLabelKey: nodePool.Name,
			}},
		})
		ExpectApplied(ctx, env.Client, nodePool, nodeClass, nodeClaim)
		ExpectObjectReconciled(ctx, env.Client, controller, nodePool)
		nodePool = ExpectExists(ctx, env.Client, nodePool)
		Expect(nodeClaim.StatusConditions().Get(v1.ConditionTypeDrifted)).To(BeNil())

		Expect(nodePool.StatusConditions().Get(v1.ConditionTypeNodeClaimsDrifted).IsFalse()).To(BeTrue())
		Expect(nodePool.Status.DriftedNodeClaimCount).To(BeZero())
	})

	It("should set Drifted to false if all NodeClaims are not drifted", func() {
		nodeClaim := test.NodeClaim(v1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
				v1.NodePoolLabelKey: nodePool.Name,
			}},
		})
		nodeClaim.StatusConditions().SetFalse(v1.ConditionTypeDrifted, "NotDrifted", "NotDrifted")
		Expect(nodeClaim.StatusConditions().Get(v1.ConditionTypeDrifted).IsFalse()).To(BeTrue())
		ExpectApplied(ctx, env.Client, nodePool, nodeClass, nodeClaim)
		ExpectObjectReconciled(ctx, env.Client, controller, nodePool)
		nodePool = ExpectExists(ctx, env.Client, nodePool)

		Expect(nodePool.StatusConditions().Get(v1.ConditionTypeNodeClaimsDrifted).IsFalse()).To(BeTrue())
		Expect(nodePool.Status.DriftedNodeClaimCount).To(BeZero())
	})

	It("should set Drifted to true if there are drifted NodeClaims", func() {
		nodeClaim := test.NodeClaim(v1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
				v1.NodePoolLabelKey: nodePool.Name,
			}},
		})
		unknownNodeClaim := test.NodeClaim(v1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
				v1.NodePoolLabelKey: nodePool.Name,
			}},
		})
		unknownNodeClaim.StatusConditions().SetUnknown(v1.ConditionTypeDrifted)
		notDriftedNodeClaim := test.NodeClaim(v1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
				v1.NodePoolLabelKey: nodePool.Name,
			}},
		})
		notDriftedNodeClaim.StatusConditions().SetFalse(v1.ConditionTypeDrifted, "NotDrifted", "NotDrifted")
		driftedNodeClaim := test.NodeClaim(v1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
				v1.NodePoolLabelKey: nodePool.Name,
			}},
		})
		driftedNodeClaim.StatusConditions().SetTrue(v1.ConditionTypeDrifted)

		Expect(nodeClaim.StatusConditions().Get(v1.ConditionTypeDrifted)).To(BeNil())
		Expect(unknownNodeClaim.StatusConditions().Get(v1.ConditionTypeDrifted).IsUnknown()).To(BeTrue())
		Expect(notDriftedNodeClaim.StatusConditions().Get(v1.ConditionTypeDrifted).IsFalse()).To(BeTrue())
		Expect(driftedNodeClaim.StatusConditions().Get(v1.ConditionTypeDrifted).IsTrue()).To(BeTrue())
		ExpectApplied(ctx, env.Client, nodePool, nodeClass, nodeClaim, unknownNodeClaim, driftedNodeClaim, notDriftedNodeClaim)
		ExpectObjectReconciled(ctx, env.Client, controller, nodePool)
		nodePool = ExpectExists(ctx, env.Client, nodePool)

		Expect(nodePool.StatusConditions().Get(v1.ConditionTypeNodeClaimsDrifted).IsTrue()).To(BeTrue())
		Expect(nodePool.Status.DriftedNodeClaimCount).To(Equal(1))
	})
})
