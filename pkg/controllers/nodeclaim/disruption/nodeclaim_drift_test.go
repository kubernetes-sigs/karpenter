/*
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
	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"knative.dev/pkg/apis"
	"knative.dev/pkg/ptr"

	"github.com/aws/karpenter-core/pkg/apis/v1beta1"
	"github.com/aws/karpenter-core/pkg/controllers/nodeclaim/disruption"
	controllerprov "github.com/aws/karpenter-core/pkg/controllers/nodepool/hash"
	"github.com/aws/karpenter-core/pkg/operator/controller"
	"github.com/aws/karpenter-core/pkg/operator/options"
	. "github.com/aws/karpenter-core/pkg/test/expectations"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/aws/karpenter-core/pkg/test"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("NodeClaim/Drift", func() {
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
			v1beta1.NodePoolHashAnnotationKey: "123456789",
		})
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim)
		ExpectReconcileSucceeded(ctx, nodeClaimDisruptionController, client.ObjectKeyFromObject(nodeClaim))

		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		Expect(nodeClaim.StatusConditions().GetCondition(v1beta1.Drifted).IsTrue()).To(BeTrue())
		Expect(nodeClaim.StatusConditions().GetCondition(v1beta1.Drifted).Reason).To(Equal(string(disruption.NodePoolDrifted)))
	})
	It("should detect node requirement drift before cloud provider drift", func() {
		cp.Drifted = "drifted"
		nodePool.Spec.Template.Spec.Requirements = []v1.NodeSelectorRequirement{
			v1.NodeSelectorRequirement{
				Key:      v1.LabelInstanceTypeStable,
				Operator: v1.NodeSelectorOpDoesNotExist,
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
		ctx = options.ToContext(ctx, test.Options(test.OptionsFields{FeatureGates: test.FeatureGates{Drift: lo.ToPtr(false)}}))
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim)
		ExpectReconcileSucceeded(ctx, nodeClaimDisruptionController, client.ObjectKeyFromObject(nodeClaim))

		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		Expect(nodeClaim.StatusConditions().GetCondition(v1beta1.Drifted)).To(BeNil())
	})
	It("should remove the status condition from the nodeClaim if the feature flag is disabled", func() {
		cp.Drifted = "drifted"
		ctx = options.ToContext(ctx, test.Options(test.OptionsFields{FeatureGates: test.FeatureGates{Drift: lo.ToPtr(false)}}))
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
			func(oldProvisionerReq []v1.NodeSelectorRequirement, newProvisionerReq []v1.NodeSelectorRequirement, labels map[string]string, drifted bool) {
				cp.Drifted = ""
				nodePool.Spec.Template.Spec.Requirements = oldProvisionerReq
				nodeClaim.Labels = lo.Assign(nodeClaim.Labels, labels)

				ExpectApplied(ctx, env.Client, nodePool, nodeClaim)
				ExpectReconcileSucceeded(ctx, nodeClaimDisruptionController, client.ObjectKeyFromObject(nodeClaim))
				nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
				Expect(nodeClaim.StatusConditions().GetCondition(v1beta1.Drifted)).To(BeNil())

				nodePool.Spec.Template.Spec.Requirements = newProvisionerReq
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
				[]v1.NodeSelectorRequirement{
					{Key: v1beta1.CapacityTypeLabelKey, Operator: v1.NodeSelectorOpIn, Values: []string{v1beta1.CapacityTypeOnDemand}},
					{Key: v1.LabelArchStable, Operator: v1.NodeSelectorOpIn, Values: []string{v1beta1.ArchitectureAmd64}},
					{Key: v1.LabelOSStable, Operator: v1.NodeSelectorOpIn, Values: []string{string(v1.Linux)}},
				},
				[]v1.NodeSelectorRequirement{
					{Key: v1beta1.CapacityTypeLabelKey, Operator: v1.NodeSelectorOpIn, Values: []string{v1beta1.CapacityTypeSpot}},
				},
				map[string]string{
					v1beta1.CapacityTypeLabelKey: v1beta1.CapacityTypeOnDemand,
					v1.LabelArchStable:           v1beta1.ArchitectureAmd64,
					v1.LabelOSStable:             string(v1.Linux),
				},
				true),
			Entry(
				"should return drifted if a new node requirement is added",
				[]v1.NodeSelectorRequirement{
					{Key: v1beta1.CapacityTypeLabelKey, Operator: v1.NodeSelectorOpIn, Values: []string{v1beta1.CapacityTypeOnDemand}},
					{Key: v1.LabelOSStable, Operator: v1.NodeSelectorOpIn, Values: []string{string(v1.Linux)}},
				},
				[]v1.NodeSelectorRequirement{
					{Key: v1beta1.CapacityTypeLabelKey, Operator: v1.NodeSelectorOpIn, Values: []string{v1beta1.CapacityTypeOnDemand}},
					{Key: v1.LabelOSStable, Operator: v1.NodeSelectorOpIn, Values: []string{string(v1.Linux)}},
					{Key: v1.LabelArchStable, Operator: v1.NodeSelectorOpIn, Values: []string{v1beta1.ArchitectureAmd64}},
				},
				map[string]string{
					v1beta1.CapacityTypeLabelKey: v1beta1.CapacityTypeOnDemand,
					v1.LabelOSStable:             string(v1.Linux),
				},
				true,
			),
			Entry(
				"should return drifted if a node requirement is reduced",
				[]v1.NodeSelectorRequirement{
					{Key: v1beta1.CapacityTypeLabelKey, Operator: v1.NodeSelectorOpIn, Values: []string{v1beta1.CapacityTypeOnDemand}},
					{Key: v1.LabelOSStable, Operator: v1.NodeSelectorOpIn, Values: []string{string(v1.Linux), string(v1.Windows)}},
				},
				[]v1.NodeSelectorRequirement{
					{Key: v1beta1.CapacityTypeLabelKey, Operator: v1.NodeSelectorOpIn, Values: []string{v1beta1.CapacityTypeOnDemand}},
					{Key: v1.LabelOSStable, Operator: v1.NodeSelectorOpIn, Values: []string{string(v1.Windows)}},
				},
				map[string]string{
					v1beta1.CapacityTypeLabelKey: v1beta1.CapacityTypeOnDemand,
					v1.LabelOSStable:             string(v1.Linux),
				},
				true,
			),
			Entry(
				"should not return drifted if a node requirement is expanded",
				[]v1.NodeSelectorRequirement{
					{Key: v1beta1.CapacityTypeLabelKey, Operator: v1.NodeSelectorOpIn, Values: []string{v1beta1.CapacityTypeOnDemand}},
					{Key: v1.LabelOSStable, Operator: v1.NodeSelectorOpIn, Values: []string{string(v1.Linux)}},
				},
				[]v1.NodeSelectorRequirement{
					{Key: v1beta1.CapacityTypeLabelKey, Operator: v1.NodeSelectorOpIn, Values: []string{v1beta1.CapacityTypeOnDemand}},
					{Key: v1.LabelOSStable, Operator: v1.NodeSelectorOpIn, Values: []string{string(v1.Linux), string(v1.Windows)}},
				},
				map[string]string{
					v1beta1.CapacityTypeLabelKey: v1beta1.CapacityTypeOnDemand,
					v1.LabelOSStable:             string(v1.Linux),
				},
				false,
			),
			Entry(
				"should not return drifted if a node requirement set to Exists",
				[]v1.NodeSelectorRequirement{
					{Key: v1beta1.CapacityTypeLabelKey, Operator: v1.NodeSelectorOpIn, Values: []string{v1beta1.CapacityTypeOnDemand}},
					{Key: v1.LabelOSStable, Operator: v1.NodeSelectorOpIn, Values: []string{string(v1.Linux)}},
				},
				[]v1.NodeSelectorRequirement{
					{Key: v1beta1.CapacityTypeLabelKey, Operator: v1.NodeSelectorOpIn, Values: []string{v1beta1.CapacityTypeOnDemand}},
					{Key: v1.LabelOSStable, Operator: v1.NodeSelectorOpExists, Values: []string{}},
				},
				map[string]string{
					v1beta1.CapacityTypeLabelKey: v1beta1.CapacityTypeOnDemand,
					v1.LabelOSStable:             string(v1.Linux),
				},
				false,
			),
			Entry(
				"should return drifted if a node requirement set to DoesNotExists",
				[]v1.NodeSelectorRequirement{
					{Key: v1beta1.CapacityTypeLabelKey, Operator: v1.NodeSelectorOpIn, Values: []string{v1beta1.CapacityTypeOnDemand}},
					{Key: v1.LabelOSStable, Operator: v1.NodeSelectorOpIn, Values: []string{string(v1.Linux)}},
				},
				[]v1.NodeSelectorRequirement{
					{Key: v1beta1.CapacityTypeLabelKey, Operator: v1.NodeSelectorOpIn, Values: []string{v1beta1.CapacityTypeOnDemand}},
					{Key: v1.LabelOSStable, Operator: v1.NodeSelectorOpDoesNotExist, Values: []string{}},
				},
				map[string]string{
					v1beta1.CapacityTypeLabelKey: v1beta1.CapacityTypeOnDemand,
					v1.LabelOSStable:             string(v1.Linux),
				},
				true,
			),
			Entry(
				"should not return drifted if a nodeClaim is grater then node requirement",
				[]v1.NodeSelectorRequirement{
					{Key: v1beta1.CapacityTypeLabelKey, Operator: v1.NodeSelectorOpIn, Values: []string{v1beta1.CapacityTypeOnDemand}},
					{Key: v1.LabelInstanceTypeStable, Operator: v1.NodeSelectorOpGt, Values: []string{"2"}},
				},
				[]v1.NodeSelectorRequirement{
					{Key: v1beta1.CapacityTypeLabelKey, Operator: v1.NodeSelectorOpIn, Values: []string{v1beta1.CapacityTypeOnDemand}},
					{Key: v1.LabelInstanceTypeStable, Operator: v1.NodeSelectorOpGt, Values: []string{"10"}},
				},
				map[string]string{
					v1beta1.CapacityTypeLabelKey: v1beta1.CapacityTypeOnDemand,
					v1.LabelInstanceTypeStable:   "5",
				},
				true,
			),
			Entry(
				"should not return drifted if a nodeClaim is less then node requirement",
				[]v1.NodeSelectorRequirement{
					{Key: v1beta1.CapacityTypeLabelKey, Operator: v1.NodeSelectorOpIn, Values: []string{v1beta1.CapacityTypeOnDemand}},
					{Key: v1.LabelInstanceTypeStable, Operator: v1.NodeSelectorOpLt, Values: []string{"5"}},
				},
				[]v1.NodeSelectorRequirement{
					{Key: v1beta1.CapacityTypeLabelKey, Operator: v1.NodeSelectorOpIn, Values: []string{v1beta1.CapacityTypeOnDemand}},
					{Key: v1.LabelInstanceTypeStable, Operator: v1.NodeSelectorOpLt, Values: []string{"1"}},
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
			nodePool.Spec.Template.Spec.Requirements = []v1.NodeSelectorRequirement{
				{Key: v1beta1.CapacityTypeLabelKey, Operator: v1.NodeSelectorOpIn, Values: []string{v1beta1.CapacityTypeOnDemand}},
				{Key: v1.LabelOSStable, Operator: v1.NodeSelectorOpIn, Values: []string{string(v1.Linux), string(v1.Windows)}},
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
			nodePool.Spec.Template.Spec.Requirements = []v1.NodeSelectorRequirement{
				{Key: v1beta1.CapacityTypeLabelKey, Operator: v1.NodeSelectorOpIn, Values: []string{v1beta1.CapacityTypeOnDemand}},
				{Key: v1.LabelOSStable, Operator: v1.NodeSelectorOpIn, Values: []string{string(v1.Linux)}},
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
	Context("Provisioner Static Drift", func() {
		var nodePoolOptions v1beta1.NodePool
		var nodePoolController controller.Controller
		BeforeEach(func() {
			cp.Drifted = ""
			nodePoolController = controllerprov.NewNodePoolController(env.Client)
			nodePoolOptions = v1beta1.NodePool{
				ObjectMeta: nodePool.ObjectMeta,
				Spec: v1beta1.NodePoolSpec{
					Template: v1beta1.NodeClaimTemplate{
						ObjectMeta: metav1.ObjectMeta{
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
								MaxPods: ptr.Int32(10),
							},
						},
					},
				},
			}
			nodeClaim.ObjectMeta.Annotations[v1beta1.NodePoolHashAnnotationKey] = nodePool.Hash()
		})
		It("should detect drift on changes for all static fields", func() {
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim)
			ExpectReconcileSucceeded(ctx, nodePoolController, client.ObjectKeyFromObject(nodePool))
			ExpectReconcileSucceeded(ctx, nodeClaimDisruptionController, client.ObjectKeyFromObject(nodeClaim))
			nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
			Expect(nodeClaim.StatusConditions().GetCondition(v1beta1.Drifted)).To(BeNil())

			// Change one static field for the same nodePool
			nodePoolFieldToChange := []*v1beta1.NodePool{
				test.NodePool(nodePoolOptions, v1beta1.NodePool{Spec: v1beta1.NodePoolSpec{Template: v1beta1.NodeClaimTemplate{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{"keyAnnotationTest": "valueAnnotationTest"}}}}}),
				test.NodePool(nodePoolOptions, v1beta1.NodePool{Spec: v1beta1.NodePoolSpec{Template: v1beta1.NodeClaimTemplate{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"keyLabelTest": "valueLabelTest"}}}}}),
				test.NodePool(nodePoolOptions, v1beta1.NodePool{Spec: v1beta1.NodePoolSpec{Template: v1beta1.NodeClaimTemplate{Spec: v1beta1.NodeClaimSpec{Taints: []v1.Taint{{Key: "keytest2taint", Effect: v1.TaintEffectNoExecute}}}}}}),
				test.NodePool(nodePoolOptions, v1beta1.NodePool{Spec: v1beta1.NodePoolSpec{Template: v1beta1.NodeClaimTemplate{Spec: v1beta1.NodeClaimSpec{StartupTaints: []v1.Taint{{Key: "keytest2startuptaint", Effect: v1.TaintEffectNoExecute}}}}}}),
				test.NodePool(nodePoolOptions, v1beta1.NodePool{Spec: v1beta1.NodePoolSpec{Template: v1beta1.NodeClaimTemplate{Spec: v1beta1.NodeClaimSpec{Kubelet: &v1beta1.KubeletConfiguration{MaxPods: ptr.Int32(30)}}}}}),
			}

			for _, updatedNodePool := range nodePoolFieldToChange {
				ExpectApplied(ctx, env.Client, updatedNodePool)
				ExpectReconcileSucceeded(ctx, nodePoolController, client.ObjectKeyFromObject(updatedNodePool))
				ExpectReconcileSucceeded(ctx, nodeClaimDisruptionController, client.ObjectKeyFromObject(nodeClaim))
				nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
				Expect(nodeClaim.StatusConditions().GetCondition(v1beta1.Drifted).IsTrue()).To(BeTrue())
			}
		})
		It("should not return drifted if karpenter.sh/nodePool-hash annotation is not present on the nodePool", func() {
			nodePool.ObjectMeta.Annotations = map[string]string{}
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim)
			ExpectReconcileSucceeded(ctx, nodeClaimDisruptionController, client.ObjectKeyFromObject(nodeClaim))
			nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
			Expect(nodeClaim.StatusConditions().GetCondition(v1beta1.Drifted)).To(BeNil())
		})
		It("should not return drifted if karpenter.sh/nodePool-hash annotation is not present on the nodeClaim", func() {
			nodeClaim.ObjectMeta.Annotations = map[string]string{}
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim)
			ExpectReconcileSucceeded(ctx, nodeClaimDisruptionController, client.ObjectKeyFromObject(nodeClaim))
			nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
			Expect(nodeClaim.StatusConditions().GetCondition(v1beta1.Drifted)).To(BeNil())
		})
	})
})
