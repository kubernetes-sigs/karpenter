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
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/test/pkg/environment/common"
)

const terminateFirstNodeCount = 1

var _ = Describe("TerminateFirst", Ordered, func() {
	var originalFeatureGates *corev1.EnvVar

	withFeatureGate := func(featureGates, gate string, enabled bool) string {
		entry := fmt.Sprintf("%s=%t", gate, enabled)
		if featureGates == "" {
			return entry
		}
		parts := strings.Split(featureGates, ",")
		found := false
		for i, part := range parts {
			if key, _, ok := strings.Cut(strings.TrimSpace(part), "="); ok && key == gate {
				parts[i] = entry
				found = true
			}
		}
		if !found {
			parts = append(parts, entry)
		}
		return strings.Join(parts, ",")
	}

	BeforeAll(func() {
		for _, setting := range env.ExpectSettings() {
			if setting.Name == "FEATURE_GATES" {
				originalFeatureGates = setting.DeepCopy()
			}
		}
		featureGates := withFeatureGate(lo.FromPtrOr(originalFeatureGates, corev1.EnvVar{}).Value, "NodeRepair", true)
		featureGates = withFeatureGate(featureGates, "TerminateFirstRepair", true)
		featureGates = withFeatureGate(featureGates, "TerminateFirstDrift", true)
		env.ExpectSettingsOverridden(corev1.EnvVar{Name: "FEATURE_GATES", Value: featureGates})
	})

	AfterAll(func() {
		if originalFeatureGates == nil {
			env.ExpectSettingsRemoved(corev1.EnvVar{Name: "FEATURE_GATES"})
		} else {
			env.ExpectSettingsOverridden(*originalFeatureGates)
		}
	})

	configureStaticNodePool := func() {
		nodePool.Spec.Replicas = lo.ToPtr(int64(terminateFirstNodeCount))
		nodePool.Spec.Limits = v1.Limits{
			corev1.ResourceName("nodes"): resource.MustParse("1"),
		}
		if env.IsDefaultNodeClassKWOK() {
			nodePool.Spec.Template.Spec.Requirements = append(nodePool.Spec.Template.Spec.Requirements, v1.NodeSelectorRequirementWithMinValues{
				Key:      corev1.LabelInstanceTypeStable,
				Operator: corev1.NodeSelectorOpIn,
				Values: []string{
					"c-16x-amd64-linux",
					"c-16x-arm64-linux",
				},
			})
		}
	}

	listNodeClaims := func(g Gomega) []*v1.NodeClaim {
		nodeClaims := &v1.NodeClaimList{}
		g.Expect(env.Client.List(env, nodeClaims, client.MatchingLabels{v1.NodePoolLabelKey: nodePool.Name})).To(Succeed())
		return lo.ToSlicePtr(nodeClaims.Items)
	}

	nodeClaimUIDs := func(nodeClaims []*v1.NodeClaim) map[types.UID]struct{} {
		return lo.SliceToMap(nodeClaims, func(nodeClaim *v1.NodeClaim) (types.UID, struct{}) {
			return nodeClaim.UID, struct{}{}
		})
	}

	hold := func(node *corev1.Node) {
		Expect(env.Client.Get(env.Context, client.ObjectKeyFromObject(node), node)).To(Succeed())
		node.Finalizers = append(node.Finalizers, common.TestingFinalizer)
		env.ExpectUpdated(node)
		Eventually(func(g Gomega) {
			current := &corev1.Node{}
			g.Expect(env.Client.Get(env.Context, client.ObjectKeyFromObject(node), current)).To(Succeed())
			g.Expect(current.Finalizers).To(ContainElement(common.TestingFinalizer))
		}).Should(Succeed())
	}

	release := func(node *corev1.Node) {
		Expect(env.Client.Get(env.Context, client.ObjectKeyFromObject(node), node)).To(Succeed())
		Expect(env.ExpectTestingFinalizerRemoved(node)).To(Succeed())
	}

	assertReplacementDidNotPrecedeDeletion := func(originalUIDs map[types.UID]struct{}, target *v1.NodeClaim, node *corev1.Node) {
		Eventually(func(g Gomega) {
			currentTarget := &v1.NodeClaim{}
			g.Expect(env.Client.Get(env, client.ObjectKeyFromObject(target), currentTarget)).To(Succeed())
			g.Expect(currentTarget.DeletionTimestamp.IsZero()).To(BeFalse())
			currentNode := &corev1.Node{}
			g.Expect(env.Client.Get(env, client.ObjectKeyFromObject(node), currentNode)).To(Succeed())
			g.Expect(currentNode.DeletionTimestamp.IsZero()).To(BeFalse())
			g.Expect(nodeClaimUIDs(listNodeClaims(g))).To(Equal(originalUIDs))
		}).Should(Succeed())

		Consistently(func(g Gomega) {
			g.Expect(nodeClaimUIDs(listNodeClaims(g))).To(Equal(originalUIDs))
		}, 10*time.Second, 2*time.Second).Should(Succeed())
	}

	eventuallyExpectReplacement := func(originalUIDs map[types.UID]struct{}) {
		Eventually(func(g Gomega) bool {
			nodeClaims := listNodeClaims(g)
			if len(nodeClaims) > terminateFirstNodeCount {
				StopTrying(fmt.Sprintf("static NodePool exceeded node limit: %d NodeClaims", len(nodeClaims))).Now()
			}
			if len(nodeClaims) != terminateFirstNodeCount {
				return false
			}
			nodeClaim := nodeClaims[0]
			_, original := originalUIDs[nodeClaim.UID]
			return nodeClaim.DeletionTimestamp.IsZero() &&
				nodeClaim.StatusConditions().Root().IsTrue() &&
				!original
		}).Should(BeTrue())
	}

	It("terminates first when repairing an at-limit static NodePool", func() {
		if _, _, ok := env.RepairCondition(); !ok {
			Skip("terminate-first repair requires --repair-condition for this provider")
		}
		configureStaticNodePool()
		env.ExpectCreated(nodeClass, nodePool)
		nodes := env.EventuallyExpectNodeCount("==", terminateFirstNodeCount)
		originalNodeClaims := env.EventuallyExpectCreatedNodeClaimCount("==", terminateFirstNodeCount)
		env.EventuallyExpectNodeClaimsReady(originalNodeClaims...)
		originalUIDs := nodeClaimUIDs(originalNodeClaims)

		target, found := lo.Find(originalNodeClaims, func(nodeClaim *v1.NodeClaim) bool {
			return nodeClaim.Status.ProviderID == nodes[0].Spec.ProviderID
		})
		Expect(found).To(BeTrue())
		hold(nodes[0])
		env.ExpectRepairFaultInjected(nodes[0])

		assertReplacementDidNotPrecedeDeletion(originalUIDs, target, nodes[0])
		release(nodes[0])
		env.EventuallyExpectNotFound(target)
		eventuallyExpectReplacement(originalUIDs)
		env.EventuallyExpectNodeCount("==", terminateFirstNodeCount)
	})

	It("terminates first when drifting an at-limit static NodePool", func() {
		const driftAnnotation = "testing.karpenter.sh/terminate-first-drift"

		configureStaticNodePool()
		env.ExpectCreated(nodeClass, nodePool)
		originalNodes := env.EventuallyExpectNodeCount("==", terminateFirstNodeCount)
		originalNodeClaims := env.EventuallyExpectCreatedNodeClaimCount("==", terminateFirstNodeCount)
		env.EventuallyExpectNodeClaimsReady(originalNodeClaims...)
		originalUIDs := nodeClaimUIDs(originalNodeClaims)
		for _, node := range originalNodes {
			hold(node)
		}

		nodePool.Spec.Template.Annotations = lo.Assign(nodePool.Spec.Template.Annotations, map[string]string{driftAnnotation: "true"})
		env.ExpectUpdated(nodePool)
		env.EventuallyExpectDrifted(originalNodeClaims...)

		var target *v1.NodeClaim
		Eventually(func(g Gomega) {
			nodeClaims := listNodeClaims(g)
			deleting := lo.Filter(nodeClaims, func(nodeClaim *v1.NodeClaim, _ int) bool {
				return !nodeClaim.DeletionTimestamp.IsZero()
			})
			g.Expect(deleting).NotTo(BeEmpty())
			target = deleting[0].DeepCopy()
		}).Should(Succeed())

		assertReplacementDidNotPrecedeDeletion(originalUIDs, target, originalNodes[0])
		for _, node := range originalNodes {
			release(node)
		}
		env.EventuallyExpectNotFound(lo.Map(originalNodeClaims, func(nodeClaim *v1.NodeClaim, _ int) client.Object { return nodeClaim })...)
		eventuallyExpectReplacement(originalUIDs)
		env.EventuallyExpectNodeCount("==", terminateFirstNodeCount)
	})
})
