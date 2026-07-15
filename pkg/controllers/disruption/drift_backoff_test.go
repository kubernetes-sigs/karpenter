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

	"github.com/awslabs/operatorpkg/status"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/controllers/disruption"
	"sigs.k8s.io/karpenter/pkg/events"
	"sigs.k8s.io/karpenter/pkg/metrics"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
)

// backoffNodePool builds a dynamic NodePool that disrupts freely (100% budget) so that back-off,
// not budgets, is what gates selection in these tests.
func backoffNodePool() *v1.NodePool {
	return test.NodePool(v1.NodePool{
		Spec: v1.NodePoolSpec{
			Disruption: v1.Disruption{
				ConsolidateAfter: v1.MustParseNillableDuration("1h"),
				Budgets:          []v1.Budget{{Nodes: "100%"}},
			},
		},
	})
}

// driftedNodeClaimAndNode builds a drifted NodeClaim/Node in the given NodePool. driftAge is added
// to the current time to control the Drifted condition's LastTransitionTime (more negative == older
// == selected first). When rs is non-nil, a bound pod is returned so the node is non-empty and drift
// launches a replacement for it.
func driftedNodeClaimAndNode(nodePoolName string, driftAge time.Duration, rs *appsv1.ReplicaSet) (*v1.NodeClaim, *corev1.Node, *corev1.Pod) {
	nodeClaim, node := test.NodeClaimAndNode(v1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{
				v1.NodePoolLabelKey:            nodePoolName,
				corev1.LabelInstanceTypeStable: mostExpensiveInstance.Name,
				v1.CapacityTypeLabelKey:        mostExpensiveOffering.Requirements.Get(v1.CapacityTypeLabelKey).Any(),
				corev1.LabelTopologyZone:       mostExpensiveOffering.Requirements.Get(corev1.LabelTopologyZone).Any(),
			},
		},
		Status: v1.NodeClaimStatus{
			ProviderID: test.RandomProviderID(),
			Allocatable: map[corev1.ResourceName]resource.Quantity{
				corev1.ResourceCPU:  resource.MustParse("32"),
				corev1.ResourcePods: resource.MustParse("100"),
			},
		},
	})
	nodeClaim.Status.Conditions = append(nodeClaim.Status.Conditions, status.Condition{
		Type:               v1.ConditionTypeDrifted,
		Status:             metav1.ConditionTrue,
		Reason:             v1.ConditionTypeDrifted,
		Message:            v1.ConditionTypeDrifted,
		LastTransitionTime: metav1.Time{Time: time.Now().Add(driftAge)},
	})
	var pod *corev1.Pod
	if rs != nil {
		pod = test.Pod(test.PodOptions{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{"app": "test"},
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion:         "apps/v1",
					Kind:               "ReplicaSet",
					Name:               rs.Name,
					UID:                rs.UID,
					Controller:         lo.ToPtr(true),
					BlockOwnerDeletion: lo.ToPtr(true),
				}},
			},
			ResourceRequirements: corev1.ResourceRequirements{
				Requests: map[corev1.ResourceName]resource.Quantity{corev1.ResourceCPU: resource.MustParse("30")},
			},
		})
	}
	return nodeClaim, node, pod
}

var _ = Describe("Drift back-off", func() {
	Context("Selection", func() {
		It("skips a backed-off NodePool's candidates and services a healthy NodePool instead", func() {
			backedOff := backoffNodePool()
			healthy := backoffNodePool()
			// The backed-off pool's candidate is the oldest, so absent back-off it would be selected first.
			bNC, bNode, _ := driftedNodeClaimAndNode(backedOff.Name, -time.Hour, nil)
			hNC, hNode, _ := driftedNodeClaimAndNode(healthy.Name, -time.Minute, nil)

			ExpectApplied(ctx, env.Client, backedOff, healthy, bNC, bNode, hNC, hNode)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{bNode, hNode}, []*v1.NodeClaim{bNC, hNC})

			// Back off the older pool.
			queue.NodePoolBackoff().Fail(backedOff.Name)
			Expect(queue.NodePoolBackoff().IsBackedOff(backedOff.Name)).To(BeTrue())

			ExpectSingletonReconciled(ctx, disruptionController)

			cmds := queue.GetCommands()
			Expect(cmds).To(HaveLen(1))
			Expect(cmds[0].Candidates).To(HaveLen(1))
			Expect(cmds[0].Candidates[0].NodePool.Name).To(Equal(healthy.Name))
			// A back-off skip event should have been surfaced for the backed-off pool.
			Expect(recorder.Calls(events.DisruptionBackoff)).To(BeNumerically(">=", 1))
		})
		It("registers a zero-valued back-off counter for a healthy NodePool with drift candidates", func() {
			nodePool := backoffNodePool()
			nc, node, _ := driftedNodeClaimAndNode(nodePool.Name, -time.Hour, nil)
			ExpectApplied(ctx, env.Client, nodePool, nc, node)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nc})

			// Evaluating drift seeds a 0 series so operators see healthy pools, not a missing metric.
			ExpectSingletonReconciled(ctx, disruptionController)
			ExpectMetricCounterValue(disruption.DriftBackoffsTotal, 0, map[string]string{metrics.NodePoolLabel: nodePool.Name})

			// A real back-off increments the counter for that pool.
			queue.NodePoolBackoff().Fail(nodePool.Name)
			ExpectMetricCounterValue(disruption.DriftBackoffsTotal, 1, map[string]string{metrics.NodePoolLabel: nodePool.Name})
		})
		It("becomes eligible again once the back-off window elapses", func() {
			nodePool := backoffNodePool()
			nc, node, _ := driftedNodeClaimAndNode(nodePool.Name, -time.Hour, nil)
			ExpectApplied(ctx, env.Client, nodePool, nc, node)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nc})

			queue.NodePoolBackoff().Fail(nodePool.Name)
			Expect(queue.NodePoolBackoff().IsBackedOff(nodePool.Name)).To(BeTrue())

			// While backed off, drift produces no command for the pool.
			ExpectSingletonReconciled(ctx, disruptionController)
			Expect(queue.GetCommands()).To(HaveLen(0))

			// The default back-off window is <= 1m; step past it and the pool is serviceable again.
			env.Clock.Step(2 * time.Minute)
			Expect(queue.NodePoolBackoff().IsBackedOff(nodePool.Name)).To(BeFalse())

			ExpectSingletonReconciled(ctx, disruptionController)
			cmds := queue.GetCommands()
			Expect(cmds).To(HaveLen(1))
			Expect(cmds[0].Candidates[0].NodePool.Name).To(Equal(nodePool.Name))
		})
	})
	Context("Starvation regression", func() {
		It("lets a younger NodePool progress after the oldest pool's replacement fails unrecoverably", func() {
			rs := test.ReplicaSet()
			ExpectApplied(ctx, env.Client, rs)

			stuck := backoffNodePool()
			healthy := backoffNodePool()
			// Both non-empty so selection is ordered purely by drift age; stuck is oldest.
			stuckNC, stuckNode, stuckPod := driftedNodeClaimAndNode(stuck.Name, -time.Hour, rs)
			healthyNC, healthyNode, healthyPod := driftedNodeClaimAndNode(healthy.Name, -time.Minute, rs)

			ExpectApplied(ctx, env.Client, stuck, healthy, stuckNC, stuckNode, healthyNC, healthyNode, stuckPod, healthyPod)
			ExpectManualBinding(ctx, env.Client, stuckPod, stuckNode)
			ExpectManualBinding(ctx, env.Client, healthyPod, healthyNode)
			ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{stuckNode, healthyNode}, []*v1.NodeClaim{stuckNC, healthyNC})

			// Pass 1: the oldest (stuck) pool is selected and a replacement is launched.
			ExpectSingletonReconciled(ctx, disruptionController)
			cmds := queue.GetCommands()
			Expect(cmds).To(HaveLen(1))
			Expect(cmds[0].Candidates[0].NodePool.Name).To(Equal(stuck.Name))
			Expect(cmds[0].Replacements).To(HaveLen(1))

			// Simulate an ICE: the replacement NodeClaim is deleted before it initializes. The queue
			// observes this as an unrecoverable failure, which arms back-off for the stuck pool and
			// returns its candidate to the pool unchanged.
			replacementName := cmds[0].Replacements[0].Name
			replacementNC := &v1.NodeClaim{}
			Expect(env.Client.Get(ctx, types.NamespacedName{Name: replacementName}, replacementNC)).To(Succeed())
			ExpectDeleted(ctx, env.Client, replacementNC)
			cluster.DeleteNodeClaim(replacementName)

			ExpectObjectReconciled(ctx, env.Client, queue, cmds[0].Candidates[0].NodeClaim)
			Expect(queue.NodePoolBackoff().IsBackedOff(stuck.Name)).To(BeTrue())
			ExpectExists(ctx, env.Client, stuckNC)

			// Pass 2: the stuck pool is skipped, so the younger healthy pool finally makes progress.
			ExpectSingletonReconciled(ctx, disruptionController)
			cmds = queue.GetCommands()
			Expect(cmds).To(HaveLen(1))
			Expect(cmds[0].Candidates[0].NodePool.Name).To(Equal(healthy.Name))
		})
	})
})
