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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/controllers/nodeclaim/disruption"
	"sigs.k8s.io/karpenter/pkg/controllers/nodepool/hash"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"

	"sigs.k8s.io/karpenter/pkg/test"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Drift", func() {
	var nodePool *v1.NodePool
	var nodeClaim *v1.NodeClaim
	var node *corev1.Node
	BeforeEach(func() {
		nodePool = test.NodePool()
		nodeClaim, node = test.NodeClaimAndNode(v1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1.NodePoolLabelKey:            nodePool.Name,
					corev1.LabelInstanceTypeStable: test.RandomName(),
				},
				Annotations: map[string]string{
					v1.NodePoolHashAnnotationKey: nodePool.Hash(),
				},
			},
		})
		// NodeClaims are required to be launched before they can be evaluated for drift
		nodeClaim.StatusConditions().SetTrue(v1.ConditionTypeLaunched)
	})
	It("should detect drift", func() {
		cp.Drifted = "drifted"
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim)
		ExpectObjectReconciled(ctx, env.Client, nodeClaimDisruptionController, nodeClaim)

		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		Expect(nodeClaim.StatusConditions().Get(v1.ConditionTypeDrifted).IsTrue()).To(BeTrue())
	})
	It("should detect static drift before cloud provider drift", func() {
		cp.Drifted = "drifted"
		nodePool.Annotations = lo.Assign(nodePool.Annotations, map[string]string{
			v1.NodePoolHashAnnotationKey:        "test-123456789",
			v1.NodePoolHashVersionAnnotationKey: v1.NodePoolHashVersion,
		})
		nodeClaim.Annotations = lo.Assign(nodeClaim.Annotations, map[string]string{
			v1.NodePoolHashAnnotationKey:        "test-123",
			v1.NodePoolHashVersionAnnotationKey: v1.NodePoolHashVersion,
		})
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim)
		ExpectObjectReconciled(ctx, env.Client, nodeClaimDisruptionController, nodeClaim)

		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		Expect(nodeClaim.StatusConditions().Get(v1.ConditionTypeDrifted).IsTrue()).To(BeTrue())
		Expect(nodeClaim.StatusConditions().Get(v1.ConditionTypeDrifted).Reason).To(Equal(string(disruption.NodePoolDrifted)))
	})
	It("should detect node requirement drift before cloud provider drift", func() {
		cp.Drifted = "drifted"
		nodePool.Spec.Template.Spec.Requirements = []v1.NodeSelectorRequirementWithMinValues{
			{
				NodeSelectorRequirement: corev1.NodeSelectorRequirement{
					Key:      corev1.LabelInstanceTypeStable,
					Operator: corev1.NodeSelectorOpDoesNotExist,
				},
			},
		}
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim)
		ExpectObjectReconciled(ctx, env.Client, nodeClaimDisruptionController, nodeClaim)

		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		Expect(nodeClaim.StatusConditions().Get(v1.ConditionTypeDrifted).IsTrue()).To(BeTrue())
		Expect(nodeClaim.StatusConditions().Get(v1.ConditionTypeDrifted).Reason).To(Equal(string(disruption.RequirementsDrifted)))
	})
	It("should remove the status condition from the nodeClaim when the nodeClaim launch condition is unknown", func() {
		cp.Drifted = "drifted"
		nodeClaim.StatusConditions().SetTrue(v1.ConditionTypeDrifted)
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
		nodeClaim.StatusConditions().SetUnknown(v1.ConditionTypeLaunched)
		ExpectApplied(ctx, env.Client, nodeClaim)

		ExpectObjectReconciled(ctx, env.Client, nodeClaimDisruptionController, nodeClaim)

		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		Expect(nodeClaim.StatusConditions().Get(v1.ConditionTypeDrifted)).To(BeNil())
	})
	It("should remove the status condition from the nodeClaim when the nodeClaim launch condition is false", func() {
		cp.Drifted = "drifted"
		nodeClaim.StatusConditions().SetTrue(v1.ConditionTypeDrifted)
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
		nodeClaim.StatusConditions().SetFalse(v1.ConditionTypeLaunched, "LaunchFailed", "LaunchFailed")
		ExpectApplied(ctx, env.Client, nodeClaim)

		ExpectObjectReconciled(ctx, env.Client, nodeClaimDisruptionController, nodeClaim)

		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		Expect(nodeClaim.StatusConditions().Get(v1.ConditionTypeDrifted)).To(BeNil())
	})
	It("should not detect drift if the nodePool does not exist", func() {
		cp.Drifted = "drifted"
		ExpectApplied(ctx, env.Client, nodeClaim)
		ExpectObjectReconciled(ctx, env.Client, nodeClaimDisruptionController, nodeClaim)

		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		Expect(nodeClaim.StatusConditions().Get(v1.ConditionTypeDrifted)).To(BeNil())
	})
	It("should remove the status condition from the nodeClaim if the nodeClaim is no longer drifted", func() {
		cp.Drifted = ""
		nodeClaim.StatusConditions().SetTrue(v1.ConditionTypeDrifted)
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim)

		ExpectObjectReconciled(ctx, env.Client, nodeClaimDisruptionController, nodeClaim)

		nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
		Expect(nodeClaim.StatusConditions().Get(v1.ConditionTypeDrifted)).To(BeNil())
	})
	Context("NodeRequirement Drift", func() {
		DescribeTable("",
			func(oldNodePoolReq []v1.NodeSelectorRequirementWithMinValues, newNodePoolReq []v1.NodeSelectorRequirementWithMinValues, labels map[string]string, drifted bool) {
				cp.Drifted = ""
				nodePool.Spec.Template.Spec.Requirements = oldNodePoolReq
				nodeClaim.Labels = lo.Assign(nodeClaim.Labels, labels)

				ExpectApplied(ctx, env.Client, nodePool, nodeClaim)
				ExpectObjectReconciled(ctx, env.Client, nodeClaimDisruptionController, nodeClaim)
				nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
				Expect(nodeClaim.StatusConditions().Get(v1.ConditionTypeDrifted)).To(BeNil())

				nodePool.Spec.Template.Spec.Requirements = newNodePoolReq
				ExpectApplied(ctx, env.Client, nodePool)
				ExpectObjectReconciled(ctx, env.Client, nodeClaimDisruptionController, nodeClaim)
				nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
				if drifted {
					Expect(nodeClaim.StatusConditions().Get(v1.ConditionTypeDrifted).IsTrue()).To(BeTrue())
				} else {
					Expect(nodeClaim.StatusConditions().Get(v1.ConditionTypeDrifted)).To(BeNil())
				}
			},
			Entry(
				"should return drifted if the nodePool node requirement is updated",
				[]v1.NodeSelectorRequirementWithMinValues{
					{NodeSelectorRequirement: corev1.NodeSelectorRequirement{Key: v1.CapacityTypeLabelKey, Operator: corev1.NodeSelectorOpIn, Values: []string{v1.CapacityTypeOnDemand}}},
					{NodeSelectorRequirement: corev1.NodeSelectorRequirement{Key: corev1.LabelArchStable, Operator: corev1.NodeSelectorOpIn, Values: []string{v1.ArchitectureAmd64}}},
					{NodeSelectorRequirement: corev1.NodeSelectorRequirement{Key: corev1.LabelOSStable, Operator: corev1.NodeSelectorOpIn, Values: []string{string(corev1.Linux)}}},
				},
				[]v1.NodeSelectorRequirementWithMinValues{
					{NodeSelectorRequirement: corev1.NodeSelectorRequirement{Key: v1.CapacityTypeLabelKey, Operator: corev1.NodeSelectorOpIn, Values: []string{v1.CapacityTypeSpot}}},
				},
				map[string]string{
					v1.CapacityTypeLabelKey: v1.CapacityTypeOnDemand,
					corev1.LabelArchStable:  v1.ArchitectureAmd64,
					corev1.LabelOSStable:    string(corev1.Linux),
				},
				true),
			Entry(
				"should return drifted if a new node requirement is added",
				[]v1.NodeSelectorRequirementWithMinValues{
					{NodeSelectorRequirement: corev1.NodeSelectorRequirement{Key: v1.CapacityTypeLabelKey, Operator: corev1.NodeSelectorOpIn, Values: []string{v1.CapacityTypeOnDemand}}},
					{NodeSelectorRequirement: corev1.NodeSelectorRequirement{Key: corev1.LabelOSStable, Operator: corev1.NodeSelectorOpIn, Values: []string{string(corev1.Linux)}}},
				},
				[]v1.NodeSelectorRequirementWithMinValues{
					{NodeSelectorRequirement: corev1.NodeSelectorRequirement{Key: v1.CapacityTypeLabelKey, Operator: corev1.NodeSelectorOpIn, Values: []string{v1.CapacityTypeOnDemand}}},
					{NodeSelectorRequirement: corev1.NodeSelectorRequirement{Key: corev1.LabelOSStable, Operator: corev1.NodeSelectorOpIn, Values: []string{string(corev1.Linux)}}},
					{NodeSelectorRequirement: corev1.NodeSelectorRequirement{Key: corev1.LabelArchStable, Operator: corev1.NodeSelectorOpIn, Values: []string{v1.ArchitectureAmd64}}},
				},
				map[string]string{
					v1.CapacityTypeLabelKey: v1.CapacityTypeOnDemand,
					corev1.LabelOSStable:    string(corev1.Linux),
				},
				true,
			),
			Entry(
				"should return drifted if a node requirement is reduced",
				[]v1.NodeSelectorRequirementWithMinValues{
					{NodeSelectorRequirement: corev1.NodeSelectorRequirement{Key: v1.CapacityTypeLabelKey, Operator: corev1.NodeSelectorOpIn, Values: []string{v1.CapacityTypeOnDemand}}},
					{NodeSelectorRequirement: corev1.NodeSelectorRequirement{Key: corev1.LabelOSStable, Operator: corev1.NodeSelectorOpIn, Values: []string{string(corev1.Linux), string(corev1.Windows)}}},
				},
				[]v1.NodeSelectorRequirementWithMinValues{
					{NodeSelectorRequirement: corev1.NodeSelectorRequirement{Key: v1.CapacityTypeLabelKey, Operator: corev1.NodeSelectorOpIn, Values: []string{v1.CapacityTypeOnDemand}}},
					{NodeSelectorRequirement: corev1.NodeSelectorRequirement{Key: corev1.LabelOSStable, Operator: corev1.NodeSelectorOpIn, Values: []string{string(corev1.Windows)}}},
				},
				map[string]string{
					v1.CapacityTypeLabelKey: v1.CapacityTypeOnDemand,
					corev1.LabelOSStable:    string(corev1.Linux),
				},
				true,
			),
			Entry(
				"should not return drifted if a node requirement is expanded",
				[]v1.NodeSelectorRequirementWithMinValues{
					{NodeSelectorRequirement: corev1.NodeSelectorRequirement{Key: v1.CapacityTypeLabelKey, Operator: corev1.NodeSelectorOpIn, Values: []string{v1.CapacityTypeOnDemand}}},
					{NodeSelectorRequirement: corev1.NodeSelectorRequirement{Key: corev1.LabelOSStable, Operator: corev1.NodeSelectorOpIn, Values: []string{string(corev1.Linux)}}},
				},
				[]v1.NodeSelectorRequirementWithMinValues{
					{NodeSelectorRequirement: corev1.NodeSelectorRequirement{Key: v1.CapacityTypeLabelKey, Operator: corev1.NodeSelectorOpIn, Values: []string{v1.CapacityTypeOnDemand}}},
					{NodeSelectorRequirement: corev1.NodeSelectorRequirement{Key: corev1.LabelOSStable, Operator: corev1.NodeSelectorOpIn, Values: []string{string(corev1.Linux), string(corev1.Windows)}}},
				},
				map[string]string{
					v1.CapacityTypeLabelKey: v1.CapacityTypeOnDemand,
					corev1.LabelOSStable:    string(corev1.Linux),
				},
				false,
			),
			Entry(
				"should not return drifted if a node requirement set to Exists",
				[]v1.NodeSelectorRequirementWithMinValues{
					{NodeSelectorRequirement: corev1.NodeSelectorRequirement{Key: v1.CapacityTypeLabelKey, Operator: corev1.NodeSelectorOpIn, Values: []string{v1.CapacityTypeOnDemand}}},
					{NodeSelectorRequirement: corev1.NodeSelectorRequirement{Key: corev1.LabelOSStable, Operator: corev1.NodeSelectorOpIn, Values: []string{string(corev1.Linux)}}},
				},
				[]v1.NodeSelectorRequirementWithMinValues{
					{NodeSelectorRequirement: corev1.NodeSelectorRequirement{Key: v1.CapacityTypeLabelKey, Operator: corev1.NodeSelectorOpIn, Values: []string{v1.CapacityTypeOnDemand}}},
					{NodeSelectorRequirement: corev1.NodeSelectorRequirement{Key: corev1.LabelOSStable, Operator: corev1.NodeSelectorOpExists, Values: []string{}}},
				},
				map[string]string{
					v1.CapacityTypeLabelKey: v1.CapacityTypeOnDemand,
					corev1.LabelOSStable:    string(corev1.Linux),
				},
				false,
			),
			Entry(
				"should return drifted if a node requirement set to DoesNotExists",
				[]v1.NodeSelectorRequirementWithMinValues{
					{NodeSelectorRequirement: corev1.NodeSelectorRequirement{Key: v1.CapacityTypeLabelKey, Operator: corev1.NodeSelectorOpIn, Values: []string{v1.CapacityTypeOnDemand}}},
					{NodeSelectorRequirement: corev1.NodeSelectorRequirement{Key: corev1.LabelOSStable, Operator: corev1.NodeSelectorOpIn, Values: []string{string(corev1.Linux)}}},
				},
				[]v1.NodeSelectorRequirementWithMinValues{
					{NodeSelectorRequirement: corev1.NodeSelectorRequirement{Key: v1.CapacityTypeLabelKey, Operator: corev1.NodeSelectorOpIn, Values: []string{v1.CapacityTypeOnDemand}}},
					{NodeSelectorRequirement: corev1.NodeSelectorRequirement{Key: corev1.LabelOSStable, Operator: corev1.NodeSelectorOpDoesNotExist, Values: []string{}}},
				},
				map[string]string{
					v1.CapacityTypeLabelKey: v1.CapacityTypeOnDemand,
					corev1.LabelOSStable:    string(corev1.Linux),
				},
				true,
			),
			Entry(
				"should not return drifted if a nodeClaim is grater then node requirement",
				[]v1.NodeSelectorRequirementWithMinValues{
					{NodeSelectorRequirement: corev1.NodeSelectorRequirement{Key: v1.CapacityTypeLabelKey, Operator: corev1.NodeSelectorOpIn, Values: []string{v1.CapacityTypeOnDemand}}},
					{NodeSelectorRequirement: corev1.NodeSelectorRequirement{Key: corev1.LabelInstanceTypeStable, Operator: corev1.NodeSelectorOpGt, Values: []string{"2"}}},
				},
				[]v1.NodeSelectorRequirementWithMinValues{
					{NodeSelectorRequirement: corev1.NodeSelectorRequirement{Key: v1.CapacityTypeLabelKey, Operator: corev1.NodeSelectorOpIn, Values: []string{v1.CapacityTypeOnDemand}}},
					{NodeSelectorRequirement: corev1.NodeSelectorRequirement{Key: corev1.LabelInstanceTypeStable, Operator: corev1.NodeSelectorOpGt, Values: []string{"10"}}},
				},
				map[string]string{
					v1.CapacityTypeLabelKey:        v1.CapacityTypeOnDemand,
					corev1.LabelInstanceTypeStable: "5",
				},
				true,
			),
			Entry(
				"should not return drifted if a nodeClaim is less then node requirement",
				[]v1.NodeSelectorRequirementWithMinValues{
					{NodeSelectorRequirement: corev1.NodeSelectorRequirement{Key: v1.CapacityTypeLabelKey, Operator: corev1.NodeSelectorOpIn, Values: []string{v1.CapacityTypeOnDemand}}},
					{NodeSelectorRequirement: corev1.NodeSelectorRequirement{Key: corev1.LabelInstanceTypeStable, Operator: corev1.NodeSelectorOpLt, Values: []string{"5"}}},
				},
				[]v1.NodeSelectorRequirementWithMinValues{
					{NodeSelectorRequirement: corev1.NodeSelectorRequirement{Key: v1.CapacityTypeLabelKey, Operator: corev1.NodeSelectorOpIn, Values: []string{v1.CapacityTypeOnDemand}}},
					{NodeSelectorRequirement: corev1.NodeSelectorRequirement{Key: corev1.LabelInstanceTypeStable, Operator: corev1.NodeSelectorOpLt, Values: []string{"1"}}},
				},
				map[string]string{
					v1.CapacityTypeLabelKey:        v1.CapacityTypeOnDemand,
					corev1.LabelInstanceTypeStable: "2",
				},
				true,
			),
		)
		It("should return drifted only on NodeClaims that are drifted from an updated nodePool", func() {
			cp.Drifted = ""
			nodePool.Spec.Template.Spec.Requirements = []v1.NodeSelectorRequirementWithMinValues{
				{NodeSelectorRequirement: corev1.NodeSelectorRequirement{Key: v1.CapacityTypeLabelKey, Operator: corev1.NodeSelectorOpIn, Values: []string{v1.CapacityTypeOnDemand}}},
				{NodeSelectorRequirement: corev1.NodeSelectorRequirement{Key: corev1.LabelOSStable, Operator: corev1.NodeSelectorOpIn, Values: []string{string(corev1.Linux), string(corev1.Windows)}}},
			}
			nodeClaim.Labels = lo.Assign(nodeClaim.Labels, map[string]string{
				v1.CapacityTypeLabelKey: v1.CapacityTypeOnDemand,
				corev1.LabelOSStable:    string(corev1.Linux),
			})
			nodeClaimTwo, _ := test.NodeClaimAndNode(v1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1.NodePoolLabelKey:            nodePool.Name,
						corev1.LabelInstanceTypeStable: test.RandomName(),
						v1.CapacityTypeLabelKey:        v1.CapacityTypeOnDemand,
						corev1.LabelOSStable:           string(corev1.Windows),
					},
					Annotations: map[string]string{
						v1.NodePoolHashAnnotationKey: nodePool.Hash(),
					},
				},
				Status: v1.NodeClaimStatus{
					ProviderID: test.RandomProviderID(),
				},
			})
			nodeClaimTwo.StatusConditions().SetTrue(v1.ConditionTypeLaunched)
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim, nodeClaimTwo)

			ExpectObjectReconciled(ctx, env.Client, nodeClaimDisruptionController, nodeClaim)
			ExpectObjectReconciled(ctx, env.Client, nodeClaimDisruptionController, nodeClaimTwo)
			nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
			nodeClaimTwo = ExpectExists(ctx, env.Client, nodeClaimTwo)
			Expect(nodeClaim.StatusConditions().Get(v1.ConditionTypeDrifted)).To(BeNil())
			Expect(nodeClaimTwo.StatusConditions().Get(v1.ConditionTypeDrifted)).To(BeNil())

			// Removed Windows OS
			nodePool.Spec.Template.Spec.Requirements = []v1.NodeSelectorRequirementWithMinValues{
				{NodeSelectorRequirement: corev1.NodeSelectorRequirement{Key: v1.CapacityTypeLabelKey, Operator: corev1.NodeSelectorOpIn, Values: []string{v1.CapacityTypeOnDemand}}},
				{NodeSelectorRequirement: corev1.NodeSelectorRequirement{Key: corev1.LabelOSStable, Operator: corev1.NodeSelectorOpIn, Values: []string{string(corev1.Linux)}}},
			}
			ExpectApplied(ctx, env.Client, nodePool)

			ExpectObjectReconciled(ctx, env.Client, nodeClaimDisruptionController, nodeClaim)
			nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
			Expect(nodeClaim.StatusConditions().Get(v1.ConditionTypeDrifted)).To(BeNil())

			ExpectObjectReconciled(ctx, env.Client, nodeClaimDisruptionController, nodeClaimTwo)
			nodeClaimTwo = ExpectExists(ctx, env.Client, nodeClaimTwo)
			Expect(nodeClaimTwo.StatusConditions().Get(v1.ConditionTypeDrifted).IsTrue()).To(BeTrue())
		})

	})
	Context("NodePool Static Drift", func() {
		var nodePoolController *hash.Controller
		BeforeEach(func() {
			cp.Drifted = ""
			nodePoolController = hash.NewController(env.Client)
			nodePool = &v1.NodePool{
				ObjectMeta: nodePool.ObjectMeta,
				Spec: v1.NodePoolSpec{
					Template: v1.NodeClaimTemplate{
						ObjectMeta: v1.ObjectMeta{
							Annotations: map[string]string{
								"keyAnnotation":  "valueAnnotation",
								"keyAnnotation2": "valueAnnotation2",
							},
							Labels: map[string]string{
								"keyLabel":  "valueLabel",
								"keyLabel2": "valueLabel2",
							},
						},
						Spec: v1.NodeClaimTemplateSpec{
							Requirements: nodePool.Spec.Template.Spec.Requirements,
							NodeClassRef: &v1.NodeClassReference{
								Kind:  "fakeKind",
								Name:  "fakeName",
								Group: "fakeGroup",
							},
							Taints: []corev1.Taint{
								{
									Key:    "keyvalue1",
									Effect: corev1.TaintEffectNoExecute,
								},
							},
							StartupTaints: []corev1.Taint{
								{
									Key:    "startupkeyvalue1",
									Effect: corev1.TaintEffectNoExecute,
								},
							},
							ExpireAfter:            v1.NillableDuration{Duration: lo.ToPtr(5 * time.Minute)},
							TerminationGracePeriod: &metav1.Duration{Duration: 5 * time.Minute},
						},
					},
				},
			}
			nodeClaim.ObjectMeta.Annotations[v1.NodePoolHashAnnotationKey] = nodePool.Hash()
		})
		// We need to test each all the fields on the NodePool when we expect the field to be drifted
		// This will also test that the NodePool fields can be hashed.
		DescribeTable("should detect drift on changes to the static fields",
			func(changes v1.NodePool) {
				ExpectApplied(ctx, env.Client, nodePool, nodeClaim)
				ExpectObjectReconciled(ctx, env.Client, nodePoolController, nodePool)
				ExpectObjectReconciled(ctx, env.Client, nodeClaimDisruptionController, nodeClaim)
				nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
				Expect(nodeClaim.StatusConditions().Get(v1.ConditionTypeDrifted)).To(BeNil())

				nodePool = ExpectExists(ctx, env.Client, nodePool)
				Expect(mergo.Merge(nodePool, changes, mergo.WithOverride)).To(Succeed())
				ExpectApplied(ctx, env.Client, nodePool)

				ExpectObjectReconciled(ctx, env.Client, nodePoolController, nodePool)
				ExpectObjectReconciled(ctx, env.Client, nodeClaimDisruptionController, nodeClaim)
				nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
				Expect(nodeClaim.StatusConditions().Get(v1.ConditionTypeDrifted).IsTrue()).To(BeTrue())
			},
			Entry("Annoations", v1.NodePool{Spec: v1.NodePoolSpec{Template: v1.NodeClaimTemplate{ObjectMeta: v1.ObjectMeta{Annotations: map[string]string{"keyAnnotationTest": "valueAnnotationTest"}}}}}),
			Entry("Labels", v1.NodePool{Spec: v1.NodePoolSpec{Template: v1.NodeClaimTemplate{ObjectMeta: v1.ObjectMeta{Labels: map[string]string{"keyLabelTest": "valueLabelTest"}}}}}),
			Entry("Taints", v1.NodePool{Spec: v1.NodePoolSpec{Template: v1.NodeClaimTemplate{Spec: v1.NodeClaimTemplateSpec{Taints: []corev1.Taint{{Key: "keytest2taint", Effect: corev1.TaintEffectNoExecute}}}}}}),
			Entry("StartupTaints", v1.NodePool{Spec: v1.NodePoolSpec{Template: v1.NodeClaimTemplate{Spec: v1.NodeClaimTemplateSpec{StartupTaints: []corev1.Taint{{Key: "keytest2taint", Effect: corev1.TaintEffectNoExecute}}}}}}),
			Entry("NodeClassRef APIVersion", v1.NodePool{Spec: v1.NodePoolSpec{Template: v1.NodeClaimTemplate{Spec: v1.NodeClaimTemplateSpec{NodeClassRef: &v1.NodeClassReference{Group: "testVersion"}}}}}),
			Entry("NodeClassRef Name", v1.NodePool{Spec: v1.NodePoolSpec{Template: v1.NodeClaimTemplate{Spec: v1.NodeClaimTemplateSpec{NodeClassRef: &v1.NodeClassReference{Name: "testName"}}}}}),
			Entry("NodeClassRef Kind", v1.NodePool{Spec: v1.NodePoolSpec{Template: v1.NodeClaimTemplate{Spec: v1.NodeClaimTemplateSpec{NodeClassRef: &v1.NodeClassReference{Kind: "testKind"}}}}}),
			Entry("ExpireAfter", v1.NodePool{Spec: v1.NodePoolSpec{Template: v1.NodeClaimTemplate{Spec: v1.NodeClaimTemplateSpec{ExpireAfter: v1.NillableDuration{Duration: lo.ToPtr(100 * time.Minute)}}}}}),
			Entry("TerminationGracePeriod", v1.NodePool{Spec: v1.NodePoolSpec{Template: v1.NodeClaimTemplate{Spec: v1.NodeClaimTemplateSpec{TerminationGracePeriod: &metav1.Duration{Duration: 100 * time.Minute}}}}}),
		)
		It("should not return drifted if karpenter.sh/nodepool-hash annotation is not present on the NodePool", func() {
			nodePool.ObjectMeta.Annotations = map[string]string{}
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim)
			ExpectObjectReconciled(ctx, env.Client, nodeClaimDisruptionController, nodeClaim)
			nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
			Expect(nodeClaim.StatusConditions().Get(v1.ConditionTypeDrifted)).To(BeNil())
		})
		It("should not return drifted if karpenter.sh/nodepool-hash annotation is not present on the NodeClaim", func() {
			nodeClaim.ObjectMeta.Annotations = map[string]string{
				v1.NodePoolHashVersionAnnotationKey: v1.NodePoolHashVersion,
			}
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim)
			ExpectObjectReconciled(ctx, env.Client, nodeClaimDisruptionController, nodeClaim)
			nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
			Expect(nodeClaim.StatusConditions().Get(v1.ConditionTypeDrifted)).To(BeNil())
		})
		It("should not return drifted if the NodeClaim's karpenter.sh/nodepool-hash-version annotation does not match the NodePool's", func() {
			nodePool.ObjectMeta.Annotations = map[string]string{
				v1.NodePoolHashAnnotationKey:        "test-hash-1",
				v1.NodePoolHashVersionAnnotationKey: "test-version-1",
			}
			nodeClaim.ObjectMeta.Annotations = map[string]string{
				v1.NodePoolHashAnnotationKey:        "test-hash-2",
				v1.NodePoolHashVersionAnnotationKey: "test-version-2",
			}
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim)
			ExpectObjectReconciled(ctx, env.Client, nodeClaimDisruptionController, nodeClaim)
			nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
			Expect(nodeClaim.StatusConditions().Get(v1.ConditionTypeDrifted)).To(BeNil())
		})
		It("should not return drifted if karpenter.sh/nodepool-hash-version annotation is not present on the NodeClaim", func() {
			nodeClaim.ObjectMeta.Annotations = map[string]string{
				v1.NodePoolHashAnnotationKey: "test-hash-111111111",
			}
			ExpectApplied(ctx, env.Client, nodePool, nodeClaim)
			ExpectObjectReconciled(ctx, env.Client, nodeClaimDisruptionController, nodeClaim)
			nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
			Expect(nodeClaim.StatusConditions().Get(v1.ConditionTypeDrifted)).To(BeNil())
		})
	})
})
