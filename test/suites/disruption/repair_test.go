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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/test"
)

// These tests exercise node repair (voluntary disruption of unhealthy nodes) and the budgeted-breaker safeguard. They
// make a node unhealthy by injecting env.RepairCondition(): a condition matching one of the provider's RepairPolicies
// that nothing on the node resets, so the fault holds while the node stays Ready and drains normally. KWOK defaults to
// its simulated KWOKUnhealthy condition (see kwok/cloudprovider and hack/kwok/stages/node-heartbeat-with-lease.yaml);
// other providers pass --repair-condition, and the specs skip without one.
var _ = Describe("Repair", Ordered, ContinueOnFailure, func() {
	BeforeAll(func() {
		if _, _, ok := env.RepairCondition(); !ok {
			Skip("node repair regression specs require --repair-condition for this provider")
		}
	})

	It("repairs an isolated unhealthy node", func() {
		appLabels := map[string]string{"app": "repair-isolated"}
		dep := test.Deployment(test.DeploymentOptions{
			Replicas: 5,
			PodOptions: test.PodOptions{
				ObjectMeta:          metav1.ObjectMeta{Labels: appLabels},
				PodAntiRequirements: hostnameAntiAffinity(appLabels),
			},
		})
		selector := labels.SelectorFromSet(appLabels)
		env.ExpectCreated(nodeClass, nodePool, dep)
		env.EventuallyExpectHealthyPodCount(selector, 5)
		nodes := env.EventuallyExpectNodeCount("==", 5)

		// 1 of 5 unhealthy (20%) is at/under the breaker threshold (trips only when unhealthy > ceil(20%)=1), so repair
		// proceeds. This asserts the fault is eventually resolved and the workload recovers; the replace-before-terminate
		// ordering is proven separately below.
		env.ExpectRepairFaultInjected(nodes[0])
		env.EventuallyExpectNotFound(nodes[0]) // unhealthy node removed
		env.EventuallyExpectNodeCount("==", 5) // pool restored
		env.EventuallyExpectHealthyPodCount(selector, 5)
	})

	It("repairs replace-first: a healthy replacement joins before the unhealthy node is removed", func() {
		// Replace-then-terminate: repair launches a replacement and only removes the original once the replacement is
		// Initialized, so a sixth NodeClaim exists while the original isn't deleting yet. A delete-first implementation
		// would start deleting the original before any replacement exists.
		appLabels := map[string]string{"app": "repair-replace-first"}
		dep := test.Deployment(test.DeploymentOptions{
			Replicas: 5,
			PodOptions: test.PodOptions{
				ObjectMeta:                    metav1.ObjectMeta{Labels: appLabels},
				PodAntiRequirements:           hostnameAntiAffinity(appLabels),
				TerminationGracePeriodSeconds: lo.ToPtr(int64(30)),
			},
		})
		selector := labels.SelectorFromSet(appLabels)
		env.ExpectCreated(nodeClass, nodePool, dep)
		env.EventuallyExpectHealthyPodCount(selector, 5)
		nodes := env.EventuallyExpectNodeCount("==", 5)

		env.ExpectRepairFaultInjected(nodes[0]) // 1 of 5 unhealthy (under the breaker threshold)
		Eventually(func(g Gomega) {
			nodeClaims := &v1.NodeClaimList{}
			g.Expect(env.Client.List(env, nodeClaims, client.HasLabels{test.DiscoveryLabel})).To(Succeed())
			g.Expect(nodeClaims.Items).To(HaveLen(6))
			original := &corev1.Node{}
			g.Expect(env.Client.Get(env, client.ObjectKeyFromObject(nodes[0]), original)).To(Succeed())
			g.Expect(original.DeletionTimestamp.IsZero()).To(BeTrue())
		}).Should(Succeed())
		env.EventuallyExpectNotFound(nodes[0]) // original removed only after the replacement joined
		env.EventuallyExpectNodeCount("==", 5)
		env.EventuallyExpectHealthyPodCount(selector, 5)
	})

	It("trips the budgeted breaker and freezes repair when >20% of the pool is unhealthy", func() {
		appLabels := map[string]string{"app": "repair-breaker"}
		dep := test.Deployment(test.DeploymentOptions{
			Replicas: 5,
			PodOptions: test.PodOptions{
				ObjectMeta:          metav1.ObjectMeta{Labels: appLabels},
				PodAntiRequirements: hostnameAntiAffinity(appLabels),
			},
		})
		selector := labels.SelectorFromSet(appLabels)
		env.ExpectCreated(nodeClass, nodePool, dep)
		env.EventuallyExpectHealthyPodCount(selector, 5)
		nodes := env.EventuallyExpectNodeCount("==", 5)

		// 2 of 5 unhealthy (40% > 20%) trips the breaker for the pool: repair must NOT disrupt any node. The faults are
		// backdated past toleration, so they're eligible from the start and only the breaker holds them back.
		env.ExpectRepairFaultInjected(nodes[0])
		env.ExpectRepairFaultInjected(nodes[1])
		env.ConsistentlyExpectNoDisruptions(5, time.Minute)
		env.ExpectExists(nodes[0])
		env.ExpectExists(nodes[1])
	})

	It("resumes repair once the pool drops back under the breaker threshold", func() {
		appLabels := map[string]string{"app": "repair-reset"}
		dep := test.Deployment(test.DeploymentOptions{
			Replicas: 5,
			PodOptions: test.PodOptions{
				ObjectMeta:          metav1.ObjectMeta{Labels: appLabels},
				PodAntiRequirements: hostnameAntiAffinity(appLabels),
			},
		})
		selector := labels.SelectorFromSet(appLabels)
		env.ExpectCreated(nodeClass, nodePool, dep)
		env.EventuallyExpectHealthyPodCount(selector, 5)
		nodes := env.EventuallyExpectNodeCount("==", 5)

		// Trip the breaker (2/5 unhealthy) -> frozen. The faults are backdated past toleration, so the breaker is what
		// freezes repair.
		env.ExpectRepairFaultInjected(nodes[0])
		env.ExpectRepairFaultInjected(nodes[1])
		env.ConsistentlyExpectNoDisruptions(5, 1*time.Minute)

		// Heal one node -> 1/5 unhealthy is under the threshold -> the breaker resets and the remaining unhealthy node is repaired.
		env.ExpectRepairFaultCleared(nodes[0])
		env.EventuallyExpectNotFound(nodes[1])
		env.EventuallyExpectNodeCount("==", 5)
		env.EventuallyExpectHealthyPodCount(selector, 5)
	})

	It("force-terminates a drain-blocked unhealthy node", func() {
		// Node repair is non-discretionary: it ignores do-not-disrupt and PDBs (only the do-not-repair annotation vetoes
		// it). Every pod carries do-not-disrupt, so graceful eviction cannot complete — the node can only leave by a
		// FORCE-termination once repair's drain bound elapses. The pod grace stays below the drain bound, since the
		// terminator deletes a pod whose grace outlasts the remaining bound as soon as the drain starts. The drain bound
		// is min(RepairPolicy TGP, NodePool TGP), so the NodePool TGP bounds it even for a provider whose policy sets
		// none. Fault only 1 of 5 (under the breaker threshold) so repair runs; a single-node pool would read 100%
		// unhealthy and trip the breaker.
		// (This asserts the force behavior, not an exact TGP duration: on KWOK node removal is near-instant once the
		// force-delete is issued, so the elapsed time isn't a reliable signal.)
		nodePool.Spec.Template.Spec.TerminationGracePeriod = &metav1.Duration{Duration: time.Minute}
		appLabels := map[string]string{"app": "repair-drain"}
		dep := test.Deployment(test.DeploymentOptions{
			Replicas: 5,
			PodOptions: test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      appLabels,
					Annotations: map[string]string{v1.DoNotDisruptAnnotationKey: "true"},
				},
				PodAntiRequirements:           hostnameAntiAffinity(appLabels),
				TerminationGracePeriodSeconds: lo.ToPtr(int64(10)),
			},
		})
		selector := labels.SelectorFromSet(appLabels)
		env.ExpectCreated(nodeClass, nodePool, dep)
		env.EventuallyExpectHealthyPodCount(selector, 5)
		nodes := env.EventuallyExpectNodeCount("==", 5)

		env.ExpectRepairFaultInjected(nodes[0])
		env.EventuallyExpectTaintedNodeCount("==", 1) // repair decided to terminate it despite the blocking pod
		// The blocking pod holds the drain until the bound (1m) minus its grace (10s)
		Consistently(func(g Gomega) {
			g.Expect(env.Client.Get(env, client.ObjectKeyFromObject(nodes[0]), &corev1.Node{})).To(Succeed())
		}, 30*time.Second).Should(Succeed())
		env.EventuallyExpectNotFound(nodes[0]) // force-terminated despite the un-evictable do-not-disrupt pod
		env.EventuallyExpectNodeCount("==", 5)
		env.EventuallyExpectHealthyPodCount(selector, 5)
	})

	It("does not repair a node carrying the do-not-repair annotation", func() {
		// do-not-repair is the operator escape hatch. A faulted node under the breaker threshold would normally be
		// repaired; the annotation must veto it so the node is never disrupted.
		appLabels := map[string]string{"app": "repair-veto"}
		dep := test.Deployment(test.DeploymentOptions{
			Replicas: 5,
			PodOptions: test.PodOptions{
				ObjectMeta:          metav1.ObjectMeta{Labels: appLabels},
				PodAntiRequirements: hostnameAntiAffinity(appLabels),
			},
		})
		selector := labels.SelectorFromSet(appLabels)
		env.ExpectCreated(nodeClass, nodePool, dep)
		env.EventuallyExpectHealthyPodCount(selector, 5)
		nodes := env.EventuallyExpectNodeCount("==", 5)

		// Veto repair on the node that will be faulted (repair reads the registered node's annotations).
		nodes[0].Annotations = lo.Assign(nodes[0].Annotations, map[string]string{v1.DoNotRepairAnnotationKey: "true"})
		env.ExpectUpdated(nodes[0])
		env.ExpectRepairFaultInjected(nodes[0]) // 1/5 unhealthy (under the breaker) — would be repaired, but do-not-repair vetoes it.
		env.ConsistentlyExpectNoDisruptions(5, 1*time.Minute)
		env.ExpectExists(nodes[0])
	})

	It("respects disruption budgets", func() {
		nodePool.Spec.Disruption.Budgets = []v1.Budget{{Nodes: "0"}}
		appLabels := map[string]string{"app": "repair-budget"}
		dep := test.Deployment(test.DeploymentOptions{Replicas: 1, PodOptions: test.PodOptions{ObjectMeta: metav1.ObjectMeta{Labels: appLabels}}})
		selector := labels.SelectorFromSet(appLabels)
		env.ExpectCreated(nodeClass, nodePool, dep)
		env.EventuallyExpectHealthyPodCount(selector, 1)
		node := env.EventuallyExpectNodeCount("==", 1)[0]

		env.ExpectRepairFaultInjected(node)
		env.ConsistentlyExpectNoDisruptions(1, time.Minute)

		// Lifting the budget releases the repair, so it was the budget holding it back
		nodePool.Spec.Disruption.Budgets = []v1.Budget{{Nodes: "100%"}}
		env.ExpectUpdated(nodePool)
		env.EventuallyExpectNotFound(node)
		env.EventuallyExpectHealthyPodCount(selector, 1)
	})

	It("ignores the do-not-disrupt annotation on the node", func() {
		appLabels := map[string]string{"app": "repair-node-do-not-disrupt"}
		dep := test.Deployment(test.DeploymentOptions{Replicas: 1, PodOptions: test.PodOptions{ObjectMeta: metav1.ObjectMeta{Labels: appLabels}}})
		selector := labels.SelectorFromSet(appLabels)
		env.ExpectCreated(nodeClass, nodePool, dep)
		env.EventuallyExpectHealthyPodCount(selector, 1)
		node := env.EventuallyExpectNodeCount("==", 1)[0]

		node.Annotations = lo.Assign(node.Annotations, map[string]string{v1.DoNotDisruptAnnotationKey: "true"})
		env.ExpectUpdated(node)
		env.ExpectRepairFaultInjected(node)
		env.EventuallyExpectNotFound(node)
		env.EventuallyExpectHealthyPodCount(selector, 1)
	})

	Context("Kubelet conditions", func() {
		var selector labels.Selector
		var dep *appsv1.Deployment
		var numPods int
		// kwok resets the conditions of a node that isn't Ready, so label the node into a no-op stage
		// (hack/kwok/stages/node-unhealthy.yaml) before injecting the condition to keep it in place. KWOK skips the
		// drain of a NotReady node (its instance is the Node itself), so these specs cover the Ready policies' candidate
		// selection, not the drain bound.
		expectUnhealthy := func(node *corev1.Node, cond corev1.NodeCondition) {
			GinkgoHelper()
			node.Labels["kwok.x-k8s.io/stage"] = "unhealthy"
			env.ExpectUpdated(node)
			env.ExpectStatusUpdated(env.ReplaceNodeConditions(node, cond))
		}

		BeforeEach(func() {
			// A real kubelet owns Ready and resets an injected value; only KWOK can pin it (see expectUnhealthy).
			if !env.IsDefaultNodeClassKWOK() {
				Skip("injecting kubelet Ready conditions requires the KWOK provider")
			}
			numPods = 1
			// Add pods with a do-not-disrupt annotation so that we can check node metadata before we disrupt
			dep = test.Deployment(test.DeploymentOptions{
				Replicas: int32(numPods),
				PodOptions: test.PodOptions{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							"app": "my-app",
						},
						Annotations: map[string]string{
							v1.DoNotDisruptAnnotationKey: "true",
						},
					},
					TerminationGracePeriodSeconds: lo.ToPtr[int64](0),
				},
			})
			selector = labels.SelectorFromSet(dep.Spec.Selector.MatchLabels)
		})
		DescribeTable("Conditions", func(unhealthyCondition corev1.NodeCondition) {
			env.ExpectCreated(nodeClass, nodePool, dep)
			pod := env.EventuallyExpectHealthyPodCount(selector, numPods)[0]
			node := env.ExpectCreatedNodeCount("==", 1)[0]
			env.EventuallyExpectInitializedNodeCount("==", 1)

			expectUnhealthy(node, unhealthyCondition)

			env.EventuallyExpectNotFound(pod, node)
			env.EventuallyExpectHealthyPodCount(selector, numPods)
		},
			// Kubelet Supported Conditions
			Entry("Node Ready False", corev1.NodeCondition{
				Type:               corev1.NodeReady,
				Status:             corev1.ConditionFalse,
				LastTransitionTime: metav1.Time{Time: time.Now().Add(-31 * time.Hour)},
			}),
			Entry("Node Ready Unknown", corev1.NodeCondition{
				Type:               corev1.NodeReady,
				Status:             corev1.ConditionUnknown,
				LastTransitionTime: metav1.Time{Time: time.Now().Add(-31 * time.Hour)},
			}),
		)
	})
})

// hostnameAntiAffinity forces each pod carrying the given labels onto its own node.
func hostnameAntiAffinity(matchLabels map[string]string) []corev1.PodAffinityTerm {
	return []corev1.PodAffinityTerm{{
		TopologyKey:   corev1.LabelHostname,
		LabelSelector: &metav1.LabelSelector{MatchLabels: matchLabels},
	}}
}
