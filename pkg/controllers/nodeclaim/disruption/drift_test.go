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

package disruption_test

import (
	"time"

	"github.com/imdario/mergo"
	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"knative.dev/pkg/apis"
	"knative.dev/pkg/ptr"

	"sigs.k8s.io/karpenter/pkg/global"

	"sigs.k8s.io/karpenter/pkg/apis/v1beta1"
	"sigs.k8s.io/karpenter/pkg/controllers/nodeclaim/disruption"
	"sigs.k8s.io/karpenter/pkg/controllers/nodepool/hash"
	"sigs.k8s.io/karpenter/pkg/operator/controller"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"sigs.k8s.io/karpenter/pkg/test"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Drift", func() {
	var nodePool *v1beta1.NodePool
	var nodeClaim *v1beta1.NodeClaim
	var node *v1.Node
	BeforeEach(func() {
		nodePool = test.NodePool()
		nodeClaim, node = test.NodeClaimAndNode(v1beta1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1beta1.NodePoolLabelKey:   nodePool.Name,
					v1.LabelInstanceTypeStable: test.RandomName(),
				},
				Annotations: map[string]string{
					v1beta1.NodePoolHashAnnotationKey: nodePool.Hash(),
				},
			},
		})
		// NodeClaims are required to be launched before they can be evaluated for drift
		nodeClaim.StatusConditions().MarkTrue(v1beta1.Launched)
	})
	Context("Metrics", func() {
		It("should fire a karpenter_nodeclaims_drifted metric when drifted", func() {
			cp.Drifted = "CloudProviderDrifted"
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim)
			ExpectReconcileSucceeded(ctx, nodeClaimDisruptionController, client.ObjectKeyFromObject(nodeClaim))

			nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
			Expect(nodeClaim.StatusConditions().GetCondition(v1beta1.Drifted).IsTrue()).To(BeTrue())
			metric, found := FindMetricWithLabelValues("karpenter_nodeclaims_drifted", map[string]string{
				"type":     "CloudProviderDrifted",
				"nodepool": nodePool.Name,
			})
			Expect(found).To(BeTrue())
			Expect(metric.GetCounter().GetValue()).To(BeNumerically("==", 1))
		})
		It("should pass-through the correct drifted type value through the karpenter_nodeclaims_drifted metric", func() {
			cp.Drifted = "drifted"
			nodePool.Spec.Template.Spec.Requirements = []v1beta1.NodeSelectorRequirementWithMinValues{
				{
					NodeSelectorRequirement: v1.NodeSelectorRequirement{
						Key:      v1.LabelInstanceTypeStable,
						Operator: v1.NodeSelectorOpDoesNotExist,
					},
				},
			}
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim)
			ExpectReconcileSucceeded(ctx, nodeClaimDisruptionController, client.ObjectKeyFromObject(nodeClaim))

			nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
			Expect(nodeClaim.StatusConditions().GetCondition(v1beta1.Drifted).IsTrue()).To(BeTrue())
			Expect(nodeClaim.StatusConditions().GetCondition(v1beta1.Drifted).Reason).To(Equal(string(disruption.RequirementsDrifted)))

			metric, found := FindMetricWithLabelValues("karpenter_nodeclaims_drifted", map[string]string{
				"type":     "RequirementsDrifted",
				"nodepool": nodePool.Name,
			})
			Expect(found).To(BeTrue())
			Expect(metric.GetCounter().GetValue()).To(BeNumerically("==", 1))
		})
		It("should fire a karpenter_nodeclaims_disrupted metric when drifted", func() {
			cp.Drifted = "drifted"
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim)
			ExpectReconcileSucceeded(ctx, nodeClaimDisruptionController, client.ObjectKeyFromObject(nodeClaim))

			nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
			Expect(nodeClaim.StatusConditions().GetCondition(v1beta1.Drifted).IsTrue()).To(BeTrue())

			metric, found := FindMetricWithLabelValues("karpenter_nodeclaims_disrupted", map[string]string{
				"type":     "drift",
				"nodepool": nodePool.Name,
			})
			Expect(found).To(BeTrue())
			Expect(metric.GetCounter().GetValue()).To(BeNumerically("==", 1))
		})
	})
	It("should detect drift", func() {
		cp.Drifted = "drifted"
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim)
		ExpectReconcileSucceeded(ctx, nodeClaimDisruptionController, client.ObjectKeyFromObject(nodeClaim))

		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		Expect(nodeClaim.StatusConditions().GetCondition(v1beta1.Drifted).IsTrue()).To(BeTrue())
	})
	It("should detect static drift before cloud provider drift", func() {
		cp.Drifted = "drifted"
		nodePool.Annotations = lo.Assign(nodePool.Annotations, map[string]string{
			v1beta1.NodePoolHashAnnotationKey:        "test-123456789",
			v1beta1.NodePoolHashVersionAnnotationKey: v1beta1.NodePoolHashVersion,
		})
		nodeClaim.Annotations = lo.Assign(nodeClaim.Annotations, map[string]string{
			v1beta1.NodePoolHashAnnotationKey:        "test-123",
			v1beta1.NodePoolHashVersionAnnotationKey: v1beta1.NodePoolHashVersion,
		})
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim)
		ExpectReconcileSucceeded(ctx, nodeClaimDisruptionController, client.ObjectKeyFromObject(nodeClaim))

		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		Expect(nodeClaim.StatusConditions().GetCondition(v1beta1.Drifted).IsTrue()).To(BeTrue())
		Expect(nodeClaim.StatusConditions().GetCondition(v1beta1.Drifted).Reason).To(Equal(string(disruption.NodePoolDrifted)))
	})
	It("should detect node requirement drift before cloud provider drift", func() {
		cp.Drifted = "drifted"
		nodePool.Spec.Template.Spec.Requirements = []v1beta1.NodeSelectorRequirementWithMinValues{
			{
				NodeSelectorRequirement: v1.NodeSelectorRequirement{
					Key:      v1.LabelInstanceTypeStable,
					Operator: v1.NodeSelectorOpDoesNotExist,
				},
			},
		}
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim)
		ExpectReconcileSucceeded(ctx, nodeClaimDisruptionController, client.ObjectKeyFromObject(nodeClaim))

		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		Expect(nodeClaim.StatusConditions().GetCondition(v1beta1.Drifted).IsTrue()).To(BeTrue())
		Expect(nodeClaim.StatusConditions().GetCondition(v1beta1.Drifted).Reason).To(Equal(string(disruption.RequirementsDrifted)))
	})
	It("should not detect drift if the feature flag is disabled", func() {
		cp.Drifted = "drifted"
		global.Config.FeatureGates.Drift = false
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim)
		ExpectReconcileSucceeded(ctx, nodeClaimDisruptionController, client.ObjectKeyFromObject(nodeClaim))

		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		Expect(nodeClaim.StatusConditions().GetCondition(v1beta1.Drifted)).To(BeNil())
	})
	It("should remove the status condition from the nodeClaim if the feature flag is disabled", func() {
		cp.Drifted = "drifted"
		global.Config.FeatureGates.Drift = false
		nodeClaim.StatusConditions().MarkTrue(v1beta1.Drifted)
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim)

		ExpectReconcileSucceeded(ctx, nodeClaimDisruptionController, client.ObjectKeyFromObject(nodeClaim))

		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		Expect(nodeClaim.StatusConditions().GetCondition(v1beta1.Drifted)).To(BeNil())
	})
	It("should remove the status condition from the nodeClaim when the nodeClaim launch condition is false", func() {
		cp.Drifted = "drifted"
		nodeClaim.StatusConditions().MarkTrue(v1beta1.Drifted)
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
		nodeClaim.StatusConditions().MarkFalse(v1beta1.Launched, "", "")
		ExpectApplied(ctx, env.Client, nodeClaim)

		ExpectReconcileSucceeded(ctx, nodeClaimDisruptionController, client.ObjectKeyFromObject(nodeClaim))

		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		Expect(nodeClaim.StatusConditions().GetCondition(v1beta1.Drifted)).To(BeNil())
	})
	It("should remove the status condition from the nodeClaim when the nodeClaim launch condition doesn't exist", func() {
		cp.Drifted = "drifted"
		nodeClaim.StatusConditions().MarkTrue(v1beta1.Drifted)
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
		nodeClaim.Status.Conditions = lo.Reject(nodeClaim.Status.Conditions, func(s apis.Condition, _ int) bool {
			return s.Type == v1beta1.Launched
		})
		ExpectApplied(ctx, env.Client, nodeClaim)

		ExpectReconcileSucceeded(ctx, nodeClaimDisruptionController, client.ObjectKeyFromObject(nodeClaim))

		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		Expect(nodeClaim.StatusConditions().GetCondition(v1beta1.Drifted)).To(BeNil())
	})
	It("should not detect drift if the nodePool does not exist", func() {
		cp.Drifted = "drifted"
		ExpectApplied(ctx, env.Client, nodeClaim)
		ExpectReconcileSucceeded(ctx, nodeClaimDisruptionController, client.ObjectKeyFromObject(nodeClaim))

		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		Expect(nodeClaim.StatusConditions().GetCondition(v1beta1.Drifted)).To(BeNil())
	})
	It("should remove the status condition from the nodeClaim if the nodeClaim is no longer drifted", func() {
		cp.Drifted = ""
		nodeClaim.StatusConditions().MarkTrue(v1beta1.Drifted)
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim)

		ExpectReconcileSucceeded(ctx, nodeClaimDisruptionController, client.ObjectKeyFromObject(nodeClaim))

		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		Expect(nodeClaim.StatusConditions().GetCondition(v1beta1.Drifted)).To(BeNil())
	})
	Context("NodeRequirement Drift", func() {
		DescribeTable("",
			func(oldNodePoolReq []v1beta1.NodeSelectorRequirementWithMinValues, newNodePoolReq []v1beta1.NodeSelectorRequirementWithMinValues, labels map[string]string, drifted bool) {
				cp.Drifted = ""
				nodePool.Spec.Template.Spec.Requirements = oldNodePoolReq
				nodeClaim.Labels = lo.Assign(nodeClaim.Labels, labels)

				ExpectApplied(ctx, env.Client, nodePool, nodeClaim)
				ExpectReconcileSucceeded(ctx, nodeClaimDisruptionController, client.ObjectKeyFromObject(nodeClaim))
				nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
				Expect(nodeClaim.StatusConditions().GetCondition(v1beta1.Drifted)).To(BeNil())

				nodePool.Spec.Template.Spec.Requirements = newNodePoolReq
				ExpectApplied(ctx, env.Client, nodePool)
				ExpectReconcileSucceeded(ctx, nodeClaimDisruptionController, client.ObjectKeyFromObject(nodeClaim))
				nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
				if drifted {
					Expect(nodeClaim.StatusConditions().GetCondition(v1beta1.Drifted).IsTrue()).To(BeTrue())
				} else {
					Expect(nodeClaim.StatusConditions().GetCondition(v1beta1.Drifted)).To(BeNil())
				}
			},
			Entry(
				"should return drifted if the nodePool node requirement is updated",
				[]v1beta1.NodeSelectorRequirementWithMinValues{
					{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: v1beta1.CapacityTypeLabelKey, Operator: v1.NodeSelectorOpIn, Values: []string{v1beta1.CapacityTypeOnDemand}}},
					{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: v1.LabelArchStable, Operator: v1.NodeSelectorOpIn, Values: []string{v1beta1.ArchitectureAmd64}}},
					{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: v1.LabelOSStable, Operator: v1.NodeSelectorOpIn, Values: []string{string(v1.Linux)}}},
				},
				[]v1beta1.NodeSelectorRequirementWithMinValues{
					{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: v1beta1.CapacityTypeLabelKey, Operator: v1.NodeSelectorOpIn, Values: []string{v1beta1.CapacityTypeSpot}}},
				},
				map[string]string{
					v1beta1.CapacityTypeLabelKey: v1beta1.CapacityTypeOnDemand,
					v1.LabelArchStable:           v1beta1.ArchitectureAmd64,
					v1.LabelOSStable:             string(v1.Linux),
				},
				true),
			Entry(
				"should return drifted if a new node requirement is added",
				[]v1beta1.NodeSelectorRequirementWithMinValues{
					{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: v1beta1.CapacityTypeLabelKey, Operator: v1.NodeSelectorOpIn, Values: []string{v1beta1.CapacityTypeOnDemand}}},
					{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: v1.LabelOSStable, Operator: v1.NodeSelectorOpIn, Values: []string{string(v1.Linux)}}},
				},
				[]v1beta1.NodeSelectorRequirementWithMinValues{
					{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: v1beta1.CapacityTypeLabelKey, Operator: v1.NodeSelectorOpIn, Values: []string{v1beta1.CapacityTypeOnDemand}}},
					{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: v1.LabelOSStable, Operator: v1.NodeSelectorOpIn, Values: []string{string(v1.Linux)}}},
					{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: v1.LabelArchStable, Operator: v1.NodeSelectorOpIn, Values: []string{v1beta1.ArchitectureAmd64}}},
				},
				map[string]string{
					v1beta1.CapacityTypeLabelKey: v1beta1.CapacityTypeOnDemand,
					v1.LabelOSStable:             string(v1.Linux),
				},
				true,
			),
			Entry(
				"should return drifted if a node requirement is reduced",
				[]v1beta1.NodeSelectorRequirementWithMinValues{
					{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: v1beta1.CapacityTypeLabelKey, Operator: v1.NodeSelectorOpIn, Values: []string{v1beta1.CapacityTypeOnDemand}}},
					{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: v1.LabelOSStable, Operator: v1.NodeSelectorOpIn, Values: []string{string(v1.Linux), string(v1.Windows)}}},
				},
				[]v1beta1.NodeSelectorRequirementWithMinValues{
					{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: v1beta1.CapacityTypeLabelKey, Operator: v1.NodeSelectorOpIn, Values: []string{v1beta1.CapacityTypeOnDemand}}},
					{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: v1.LabelOSStable, Operator: v1.NodeSelectorOpIn, Values: []string{string(v1.Windows)}}},
				},
				map[string]string{
					v1beta1.CapacityTypeLabelKey: v1beta1.CapacityTypeOnDemand,
					v1.LabelOSStable:             string(v1.Linux),
				},
				true,
			),
			Entry(
				"should not return drifted if a node requirement is expanded",
				[]v1beta1.NodeSelectorRequirementWithMinValues{
					{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: v1beta1.CapacityTypeLabelKey, Operator: v1.NodeSelectorOpIn, Values: []string{v1beta1.CapacityTypeOnDemand}}},
					{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: v1.LabelOSStable, Operator: v1.NodeSelectorOpIn, Values: []string{string(v1.Linux)}}},
				},
				[]v1beta1.NodeSelectorRequirementWithMinValues{
					{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: v1beta1.CapacityTypeLabelKey, Operator: v1.NodeSelectorOpIn, Values: []string{v1beta1.CapacityTypeOnDemand}}},
					{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: v1.LabelOSStable, Operator: v1.NodeSelectorOpIn, Values: []string{string(v1.Linux), string(v1.Windows)}}},
				},
				map[string]string{
					v1beta1.CapacityTypeLabelKey: v1beta1.CapacityTypeOnDemand,
					v1.LabelOSStable:             string(v1.Linux),
				},
				false,
			),
			Entry(
				"should not return drifted if a node requirement set to Exists",
				[]v1beta1.NodeSelectorRequirementWithMinValues{
					{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: v1beta1.CapacityTypeLabelKey, Operator: v1.NodeSelectorOpIn, Values: []string{v1beta1.CapacityTypeOnDemand}}},
					{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: v1.LabelOSStable, Operator: v1.NodeSelectorOpIn, Values: []string{string(v1.Linux)}}},
				},
				[]v1beta1.NodeSelectorRequirementWithMinValues{
					{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: v1beta1.CapacityTypeLabelKey, Operator: v1.NodeSelectorOpIn, Values: []string{v1beta1.CapacityTypeOnDemand}}},
					{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: v1.LabelOSStable, Operator: v1.NodeSelectorOpExists, Values: []string{}}},
				},
				map[string]string{
					v1beta1.CapacityTypeLabelKey: v1beta1.CapacityTypeOnDemand,
					v1.LabelOSStable:             string(v1.Linux),
				},
				false,
			),
			Entry(
				"should return drifted if a node requirement set to DoesNotExists",
				[]v1beta1.NodeSelectorRequirementWithMinValues{
					{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: v1beta1.CapacityTypeLabelKey, Operator: v1.NodeSelectorOpIn, Values: []string{v1beta1.CapacityTypeOnDemand}}},
					{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: v1.LabelOSStable, Operator: v1.NodeSelectorOpIn, Values: []string{string(v1.Linux)}}},
				},
				[]v1beta1.NodeSelectorRequirementWithMinValues{
					{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: v1beta1.CapacityTypeLabelKey, Operator: v1.NodeSelectorOpIn, Values: []string{v1beta1.CapacityTypeOnDemand}}},
					{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: v1.LabelOSStable, Operator: v1.NodeSelectorOpDoesNotExist, Values: []string{}}},
				},
				map[string]string{
					v1beta1.CapacityTypeLabelKey: v1beta1.CapacityTypeOnDemand,
					v1.LabelOSStable:             string(v1.Linux),
				},
				true,
			),
			Entry(
				"should not return drifted if a nodeClaim is grater then node requirement",
				[]v1beta1.NodeSelectorRequirementWithMinValues{
					{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: v1beta1.CapacityTypeLabelKey, Operator: v1.NodeSelectorOpIn, Values: []string{v1beta1.CapacityTypeOnDemand}}},
					{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: v1.LabelInstanceTypeStable, Operator: v1.NodeSelectorOpGt, Values: []string{"2"}}},
				},
				[]v1beta1.NodeSelectorRequirementWithMinValues{
					{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: v1beta1.CapacityTypeLabelKey, Operator: v1.NodeSelectorOpIn, Values: []string{v1beta1.CapacityTypeOnDemand}}},
					{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: v1.LabelInstanceTypeStable, Operator: v1.NodeSelectorOpGt, Values: []string{"10"}}},
				},
				map[string]string{
					v1beta1.CapacityTypeLabelKey: v1beta1.CapacityTypeOnDemand,
					v1.LabelInstanceTypeStable:   "5",
				},
				true,
			),
			Entry(
				"should not return drifted if a nodeClaim is less then node requirement",
				[]v1beta1.NodeSelectorRequirementWithMinValues{
					{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: v1beta1.CapacityTypeLabelKey, Operator: v1.NodeSelectorOpIn, Values: []string{v1beta1.CapacityTypeOnDemand}}},
					{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: v1.LabelInstanceTypeStable, Operator: v1.NodeSelectorOpLt, Values: []string{"5"}}},
				},
				[]v1beta1.NodeSelectorRequirementWithMinValues{
					{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: v1beta1.CapacityTypeLabelKey, Operator: v1.NodeSelectorOpIn, Values: []string{v1beta1.CapacityTypeOnDemand}}},
					{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: v1.LabelInstanceTypeStable, Operator: v1.NodeSelectorOpLt, Values: []string{"1"}}},
				},
				map[string]string{
					v1beta1.CapacityTypeLabelKey: v1beta1.CapacityTypeOnDemand,
					v1.LabelInstanceTypeStable:   "2",
				},
				true,
			),
		)
		It("should return drifted only on NodeClaims that are drifted from an updated nodePool", func() {
			cp.Drifted = ""
			nodePool.Spec.Template.Spec.Requirements = []v1beta1.NodeSelectorRequirementWithMinValues{
				{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: v1beta1.CapacityTypeLabelKey, Operator: v1.NodeSelectorOpIn, Values: []string{v1beta1.CapacityTypeOnDemand}}},
				{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: v1.LabelOSStable, Operator: v1.NodeSelectorOpIn, Values: []string{string(v1.Linux), string(v1.Windows)}}},
			}
			nodeClaim.Labels = lo.Assign(nodeClaim.Labels, map[string]string{
				v1beta1.CapacityTypeLabelKey: v1beta1.CapacityTypeOnDemand,
				v1.LabelOSStable:             string(v1.Linux),
			})
			nodeClaimTwo, _ := test.NodeClaimAndNode(v1beta1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1beta1.NodePoolLabelKey:     nodePool.Name,
						v1.LabelInstanceTypeStable:   test.RandomName(),
						v1beta1.CapacityTypeLabelKey: v1beta1.CapacityTypeOnDemand,
						v1.LabelOSStable:             string(v1.Windows),
					},
					Annotations: map[string]string{
						v1beta1.NodePoolHashAnnotationKey: nodePool.Hash(),
					},
				},
				Status: v1beta1.NodeClaimStatus{
					ProviderID: test.RandomProviderID(),
				},
			})
			nodeClaimTwo.StatusConditions().MarkTrue(v1beta1.Launched)
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim, nodeClaimTwo)

			ExpectReconcileSucceeded(ctx, nodeClaimDisruptionController, client.ObjectKeyFromObject(nodeClaim))
			ExpectReconcileSucceeded(ctx, nodeClaimDisruptionController, client.ObjectKeyFromObject(nodeClaimTwo))
			nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
			nodeClaimTwo = ExpectExists(ctx, env.Client, nodeClaimTwo)
			Expect(nodeClaim.StatusConditions().GetCondition(v1beta1.Drifted)).To(BeNil())
			Expect(nodeClaimTwo.StatusConditions().GetCondition(v1beta1.Drifted)).To(BeNil())

			// Removed Windows OS
			nodePool.Spec.Template.Spec.Requirements = []v1beta1.NodeSelectorRequirementWithMinValues{
				{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: v1beta1.CapacityTypeLabelKey, Operator: v1.NodeSelectorOpIn, Values: []string{v1beta1.CapacityTypeOnDemand}}},
				{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: v1.LabelOSStable, Operator: v1.NodeSelectorOpIn, Values: []string{string(v1.Linux)}}},
			}
			ExpectApplied(ctx, env.Client, nodePool)

			ExpectReconcileSucceeded(ctx, nodeClaimDisruptionController, client.ObjectKeyFromObject(nodeClaim))
			nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
			Expect(nodeClaim.StatusConditions().GetCondition(v1beta1.Drifted)).To(BeNil())

			ExpectReconcileSucceeded(ctx, nodeClaimDisruptionController, client.ObjectKeyFromObject(nodeClaimTwo))
			nodeClaimTwo = ExpectExists(ctx, env.Client, nodeClaimTwo)
			Expect(nodeClaimTwo.StatusConditions().GetCondition(v1beta1.Drifted).IsTrue()).To(BeTrue())
		})

	})
	Context("NodePool Static Drift", func() {
		var nodePoolController controller.Controller
		BeforeEach(func() {
			cp.Drifted = ""
			nodePoolController = hash.NewController(env.Client)
			nodePool = &v1beta1.NodePool{
				ObjectMeta: nodePool.ObjectMeta,
				Spec: v1beta1.NodePoolSpec{
					Template: v1beta1.NodeClaimTemplate{
						ObjectMeta: v1beta1.ObjectMeta{
							Annotations: map[string]string{
								"keyAnnotation":  "valueAnnotation",
								"keyAnnotation2": "valueAnnotation2",
							},
							Labels: map[string]string{
								"keyLabel":  "valueLabel",
								"keyLabel2": "valueLabel2",
							},
						},
						Spec: v1beta1.NodeClaimSpec{
							Requirements: nodePool.Spec.Template.Spec.Requirements,
							NodeClassRef: &v1beta1.NodeClassReference{
								Kind:       "fakeKind",
								Name:       "fakeName",
								APIVersion: "fakeGroup/fakeVerion",
							},
							Taints: []v1.Taint{
								{
									Key:    "keyvalue1",
									Effect: v1.TaintEffectNoExecute,
								},
							},
							StartupTaints: []v1.Taint{
								{
									Key:    "startupkeyvalue1",
									Effect: v1.TaintEffectNoExecute,
								},
							},
							Kubelet: &v1beta1.KubeletConfiguration{
								ClusterDNS:  []string{"fakeDNS"},
								MaxPods:     ptr.Int32(0),
								PodsPerCore: ptr.Int32(0),
								SystemReserved: map[string]string{
									"cpu": "2",
								},
								KubeReserved: map[string]string{
									"memory": "10Gi",
								},
								EvictionHard: map[string]string{
									"memory.available": "20Gi",
								},
								EvictionSoft: map[string]string{
									"nodefs.available": "20Gi",
								},
								EvictionMaxPodGracePeriod: ptr.Int32(0),
								EvictionSoftGracePeriod: map[string]metav1.Duration{
									"nodefs.available": {Duration: time.Second},
								},
								ImageGCHighThresholdPercent: ptr.Int32(11),
								ImageGCLowThresholdPercent:  ptr.Int32(0),
								CPUCFSQuota:                 lo.ToPtr(false),
							},
						},
					},
				},
			}
			nodeClaim.ObjectMeta.Annotations[v1beta1.NodePoolHashAnnotationKey] = nodePool.Hash()
		})
		// We need to test each all the fields on the NodePool when we expect the field to be drifted
		// This will also test that the NodePool fields can be hashed.
		DescribeTable("should detect drift on changes to the static fields",
			func(changes v1beta1.NodePool) {
				ExpectApplied(ctx, env.Client, nodePool, nodeClaim)
				ExpectReconcileSucceeded(ctx, nodePoolController, client.ObjectKeyFromObject(nodePool))
				ExpectReconcileSucceeded(ctx, nodeClaimDisruptionController, client.ObjectKeyFromObject(nodeClaim))
				nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
				Expect(nodeClaim.StatusConditions().GetCondition(v1beta1.Drifted)).To(BeNil())

				nodePool = ExpectExists(ctx, env.Client, nodePool)
				Expect(mergo.Merge(nodePool, changes, mergo.WithOverride)).To(Succeed())
				ExpectApplied(ctx, env.Client, nodePool)

				ExpectReconcileSucceeded(ctx, nodePoolController, client.ObjectKeyFromObject(nodePool))
				ExpectReconcileSucceeded(ctx, nodeClaimDisruptionController, client.ObjectKeyFromObject(nodeClaim))
				nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
				Expect(nodeClaim.StatusConditions().GetCondition(v1beta1.Drifted).IsTrue()).To(BeTrue())
			},
			Entry("Annoations", v1beta1.NodePool{Spec: v1beta1.NodePoolSpec{Template: v1beta1.NodeClaimTemplate{ObjectMeta: v1beta1.ObjectMeta{Annotations: map[string]string{"keyAnnotationTest": "valueAnnotationTest"}}}}}),
			Entry("Labels", v1beta1.NodePool{Spec: v1beta1.NodePoolSpec{Template: v1beta1.NodeClaimTemplate{ObjectMeta: v1beta1.ObjectMeta{Labels: map[string]string{"keyLabelTest": "valueLabelTest"}}}}}),
			Entry("Taints", v1beta1.NodePool{Spec: v1beta1.NodePoolSpec{Template: v1beta1.NodeClaimTemplate{Spec: v1beta1.NodeClaimSpec{Taints: []v1.Taint{{Key: "keytest2taint", Effect: v1.TaintEffectNoExecute}}}}}}),
			Entry("StartupTaints", v1beta1.NodePool{Spec: v1beta1.NodePoolSpec{Template: v1beta1.NodeClaimTemplate{Spec: v1beta1.NodeClaimSpec{StartupTaints: []v1.Taint{{Key: "keytest2taint", Effect: v1.TaintEffectNoExecute}}}}}}),
			Entry("NodeClassRef APIVersion", v1beta1.NodePool{Spec: v1beta1.NodePoolSpec{Template: v1beta1.NodeClaimTemplate{Spec: v1beta1.NodeClaimSpec{NodeClassRef: &v1beta1.NodeClassReference{APIVersion: "testVersion"}}}}}),
			Entry("NodeClassRef Name", v1beta1.NodePool{Spec: v1beta1.NodePoolSpec{Template: v1beta1.NodeClaimTemplate{Spec: v1beta1.NodeClaimSpec{NodeClassRef: &v1beta1.NodeClassReference{Name: "testName"}}}}}),
			Entry("NodeClassRef Kind", v1beta1.NodePool{Spec: v1beta1.NodePoolSpec{Template: v1beta1.NodeClaimTemplate{Spec: v1beta1.NodeClaimSpec{NodeClassRef: &v1beta1.NodeClassReference{Kind: "testKind"}}}}}),
			Entry("kubeletConfiguration ClusterDNS", v1beta1.NodePool{Spec: v1beta1.NodePoolSpec{Template: v1beta1.NodeClaimTemplate{Spec: v1beta1.NodeClaimSpec{Kubelet: &v1beta1.KubeletConfiguration{ClusterDNS: []string{"testDNS"}}}}}}),
			Entry("KubeletConfiguration MaxPods", v1beta1.NodePool{Spec: v1beta1.NodePoolSpec{Template: v1beta1.NodeClaimTemplate{Spec: v1beta1.NodeClaimSpec{Kubelet: &v1beta1.KubeletConfiguration{MaxPods: ptr.Int32(5)}}}}}),
			Entry("KubeletConfiguration PodsPerCore", v1beta1.NodePool{Spec: v1beta1.NodePoolSpec{Template: v1beta1.NodeClaimTemplate{Spec: v1beta1.NodeClaimSpec{Kubelet: &v1beta1.KubeletConfiguration{PodsPerCore: ptr.Int32(5)}}}}}),
			Entry("KubeletConfiguration SystemReserved", v1beta1.NodePool{Spec: v1beta1.NodePoolSpec{Template: v1beta1.NodeClaimTemplate{Spec: v1beta1.NodeClaimSpec{Kubelet: &v1beta1.KubeletConfiguration{SystemReserved: map[string]string{"memory": "30Gi"}}}}}}),
			Entry("KubeletConfiguration KubeReserved", v1beta1.NodePool{Spec: v1beta1.NodePoolSpec{Template: v1beta1.NodeClaimTemplate{Spec: v1beta1.NodeClaimSpec{Kubelet: &v1beta1.KubeletConfiguration{KubeReserved: map[string]string{"cpu": "10"}}}}}}),
			Entry("KubeletConfiguration EvictionHard", v1beta1.NodePool{Spec: v1beta1.NodePoolSpec{Template: v1beta1.NodeClaimTemplate{Spec: v1beta1.NodeClaimSpec{Kubelet: &v1beta1.KubeletConfiguration{EvictionHard: map[string]string{"memory.available": "30Gi"}}}}}}),
			Entry("KubeletConfiguration EvictionSoft", v1beta1.NodePool{Spec: v1beta1.NodePoolSpec{Template: v1beta1.NodeClaimTemplate{Spec: v1beta1.NodeClaimSpec{Kubelet: &v1beta1.KubeletConfiguration{EvictionSoft: map[string]string{"nodefs.available": "30Gi"}}}}}}),
			Entry("KubeletConfiguration EvictionSoftGracePeriod", v1beta1.NodePool{Spec: v1beta1.NodePoolSpec{Template: v1beta1.NodeClaimTemplate{Spec: v1beta1.NodeClaimSpec{Kubelet: &v1beta1.KubeletConfiguration{EvictionSoftGracePeriod: map[string]metav1.Duration{"nodefs.available": {Duration: time.Minute}}}}}}}),
			Entry("KubeletConfiguration EvictionMaxPodGracePeriod", v1beta1.NodePool{Spec: v1beta1.NodePoolSpec{Template: v1beta1.NodeClaimTemplate{Spec: v1beta1.NodeClaimSpec{Kubelet: &v1beta1.KubeletConfiguration{EvictionMaxPodGracePeriod: ptr.Int32(5)}}}}}),
			Entry("KubeletConfiguration ImageGCHighThresholdPercent", v1beta1.NodePool{Spec: v1beta1.NodePoolSpec{Template: v1beta1.NodeClaimTemplate{Spec: v1beta1.NodeClaimSpec{Kubelet: &v1beta1.KubeletConfiguration{ImageGCHighThresholdPercent: ptr.Int32(20)}}}}}),
			Entry("KubeletConfiguration ImageGCLowThresholdPercent", v1beta1.NodePool{Spec: v1beta1.NodePoolSpec{Template: v1beta1.NodeClaimTemplate{Spec: v1beta1.NodeClaimSpec{Kubelet: &v1beta1.KubeletConfiguration{ImageGCLowThresholdPercent: ptr.Int32(10)}}}}}),
			Entry("KubeletConfiguration CPUCFSQuota", v1beta1.NodePool{Spec: v1beta1.NodePoolSpec{Template: v1beta1.NodeClaimTemplate{Spec: v1beta1.NodeClaimSpec{Kubelet: &v1beta1.KubeletConfiguration{CPUCFSQuota: lo.ToPtr(true)}}}}}),
		)
		It("should not return drifted if karpenter.sh/nodepool-hash annotation is not present on the NodePool", func() {
			nodePool.ObjectMeta.Annotations = map[string]string{}
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim)
			ExpectReconcileSucceeded(ctx, nodeClaimDisruptionController, client.ObjectKeyFromObject(nodeClaim))
			nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
			Expect(nodeClaim.StatusConditions().GetCondition(v1beta1.Drifted)).To(BeNil())
		})
		It("should not return drifted if karpenter.sh/nodepool-hash annotation is not present on the NodeClaim", func() {
			nodeClaim.ObjectMeta.Annotations = map[string]string{
				v1beta1.NodePoolHashVersionAnnotationKey: v1beta1.NodePoolHashVersion,
			}
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim)
			ExpectReconcileSucceeded(ctx, nodeClaimDisruptionController, client.ObjectKeyFromObject(nodeClaim))
			nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
			Expect(nodeClaim.StatusConditions().GetCondition(v1beta1.Drifted)).To(BeNil())
		})
		It("should not return drifted if the NodeClaim's karpenter.sh/nodepool-hash-version annotation does not match the NodePool's", func() {
			nodePool.ObjectMeta.Annotations = map[string]string{
				v1beta1.NodePoolHashAnnotationKey:        "test-hash-1",
				v1beta1.NodePoolHashVersionAnnotationKey: "test-version-1",
			}
			nodeClaim.ObjectMeta.Annotations = map[string]string{
				v1beta1.NodePoolHashAnnotationKey:        "test-hash-2",
				v1beta1.NodePoolHashVersionAnnotationKey: "test-version-2",
			}
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim)
			ExpectReconcileSucceeded(ctx, nodeClaimDisruptionController, client.ObjectKeyFromObject(nodeClaim))
			nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
			Expect(nodeClaim.StatusConditions().GetCondition(v1beta1.Drifted)).To(BeNil())
		})
		It("should not return drifted if karpenter.sh/nodepool-hash-version annotation is not present on the NodeClaim", func() {
			nodeClaim.ObjectMeta.Annotations = map[string]string{
				v1beta1.NodePoolHashAnnotationKey: "test-hash-111111111",
			}
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim)
			ExpectReconcileSucceeded(ctx, nodeClaimDisruptionController, client.ObjectKeyFromObject(nodeClaim))
			nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
			Expect(nodeClaim.StatusConditions().GetCondition(v1beta1.Drifted)).To(BeNil())
		})
	})
})
