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
	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/test"
)

// These specs drive the reboot node action end-to-end: repair matches a reboot-clearable fault (env.RebootCondition())
// and commits a reboot, and the reboot controller fences, drains, issues, and observes the node's return. KWOK defaults
// to its simulated KWOKRebootRequired condition and simulates the reboot by changing the node's boot ID; other providers
// pass --reboot-condition, and the specs skip without one.
var _ = Describe("Reboot", func() {
	var dep *appsv1.Deployment
	var selector labels.Selector

	BeforeEach(func() {
		if _, ok := env.RebootCondition(); !ok {
			Skip("reboot specs require --reboot-condition for this provider")
		}
		dep = test.Deployment(test.DeploymentOptions{
			Replicas: 1,
			PodOptions: test.PodOptions{
				ObjectMeta:                    metav1.ObjectMeta{Labels: map[string]string{"app": "reboot"}},
				TerminationGracePeriodSeconds: lo.ToPtr[int64](0),
			},
		})
		selector = labels.SelectorFromSet(dep.Spec.Selector.MatchLabels)
	})

	rebooting := func(g Gomega, nodeClaim *v1.NodeClaim) *v1.NodeClaim {
		nc := &v1.NodeClaim{}
		g.Expect(env.Client.Get(env, client.ObjectKeyFromObject(nodeClaim), nc)).To(Succeed())
		return nc
	}

	// expectRebootedInPlace waits for a reboot of nodeClaim to succeed, then checks it was in place: the same
	// NodeClaim and Node, a new boot, the fence removed, the node re-initialized, and no replacement launched.
	expectRebootedInPlace := func(nodeClaim *v1.NodeClaim, node *corev1.Node, preBootID string) {
		GinkgoHelper()
		Eventually(func(g Gomega) {
			cond := rebooting(g, nodeClaim).StatusConditions().Get(v1.ConditionTypeRebooting)
			g.Expect(cond).ToNot(BeNil())
			g.Expect(cond.IsFalse()).To(BeTrue())
			g.Expect(cond.Reason).To(Equal(v1.RebootReasonSucceeded))
		}).Should(Succeed())
		Eventually(func(g Gomega) {
			n := &corev1.Node{}
			g.Expect(env.Client.Get(env, client.ObjectKeyFromObject(node), n)).To(Succeed())
			g.Expect(n.Status.NodeInfo.BootID).ToNot(Equal(preBootID))
			g.Expect(n.Spec.Taints).ToNot(ContainElement(HaveField("Key", v1.RebootingTaintKey)))
			g.Expect(n.Labels).To(HaveKeyWithValue(v1.NodeInitializedLabelKey, "true"))
		}).Should(Succeed())
		nodeClaims := env.EventuallyExpectCreatedNodeClaimCount("==", 1)
		Expect(nodeClaims[0].Name).To(Equal(nodeClaim.Name))
		Expect(nodeClaims[0].DeletionTimestamp.IsZero()).To(BeTrue())
		env.EventuallyExpectHealthyPodCount(selector, 1)
	}

	It("should reboot a node in place when repair matches a reboot policy", func() {
		env.ExpectCreated(nodeClass, nodePool, dep)
		env.EventuallyExpectHealthyPodCount(selector, 1)
		node := env.EventuallyExpectInitializedNodeCount("==", 1)[0]
		nodeClaim := env.EventuallyExpectCreatedNodeClaimCount("==", 1)[0]
		preBootID := node.Status.NodeInfo.BootID

		env.ExpectRebootFaultInjected(node)
		// Repair commits the reboot: the fault is recorded as the reason, and the node is fenced while it drains.
		Eventually(func(g Gomega) {
			nc := rebooting(g, nodeClaim)
			cond := nc.StatusConditions().Get(v1.ConditionTypeRebooting)
			g.Expect(cond).ToNot(BeNil())
			g.Expect(cond.IsTrue()).To(BeTrue())
			fault, _ := env.RebootCondition()
			g.Expect(cond.Message).To(ContainSubstring(string(fault.Type)))
			g.Expect(nc.StatusConditions().Get(v1.ConditionTypeDisruptionReason).IsTrue()).To(BeTrue())
		}).Should(Succeed())
		// The reboot clears the fault.
		env.ExpectRebootFaultCleared(node)
		// The displaced pod waits for the node to return rather than triggering replacement capacity.
		env.ConsistentlyExpectNodeClaimCountNotExceed(10*time.Second, 1)

		expectRebootedInPlace(nodeClaim, node, preBootID)
		// The cleared fault doesn't trigger another action.
		Consistently(func(g Gomega) {
			g.Expect(rebooting(g, nodeClaim).StatusConditions().Get(v1.ConditionTypeRebooting).Reason).To(Equal(v1.RebootReasonSucceeded))
		}, 30*time.Second).Should(Succeed())
		env.ExpectNodeClaimCount("==", 1)
	})

	It("should reboot twice, then replace, when the fault persists across reboots", func() {
		env.ExpectCreated(nodeClass, nodePool, dep)
		env.EventuallyExpectHealthyPodCount(selector, 1)
		node := env.EventuallyExpectInitializedNodeCount("==", 1)[0]
		nodeClaim := env.EventuallyExpectCreatedNodeClaimCount("==", 1)[0]
		preBootID := node.Status.NodeInfo.BootID

		// The fault never clears. Repair's escalation is reboot, reboot, then replace: each reboot gives the node a
		// new boot, so record every boot until the node is replaced. A boot stays visible for at least minDrainTime
		// before the next reboot can issue, so polling every second doesn't miss one.
		env.ExpectRebootFaultInjected(node)
		boots := map[string]bool{}
		Eventually(func(g Gomega) {
			n := &corev1.Node{}
			if err := env.Client.Get(env, client.ObjectKeyFromObject(node), n); err == nil {
				if n.Status.NodeInfo.BootID != preBootID {
					boots[n.Status.NodeInfo.BootID] = true
				}
				g.Expect(n.DeletionTimestamp.IsZero()).To(BeFalse(), "node not yet replaced")
			}
		}).WithTimeout(35 * time.Minute).WithPolling(time.Second).Should(Succeed())
		Expect(boots).To(HaveLen(2), "expected exactly two reboots before replacement")

		env.EventuallyExpectNotFound(nodeClaim, node)
		env.EventuallyExpectHealthyPodCount(selector, 1)
		replacement := env.EventuallyExpectCreatedNodeClaimCount("==", 1)[0]
		Expect(replacement.Name).ToNot(Equal(nodeClaim.Name))
	})

	It("should keep a pod that can't be evicted on the node through the reboot", func() {
		dep.Spec.Template.Annotations = lo.Assign(dep.Spec.Template.Annotations, map[string]string{v1.DoNotDisruptAnnotationKey: "true"})
		dep.Spec.Template.Spec.TerminationGracePeriodSeconds = lo.ToPtr[int64](30)
		env.ExpectCreated(nodeClass, nodePool, dep)
		pod := env.EventuallyExpectHealthyPodCount(selector, 1)[0]
		node := env.EventuallyExpectInitializedNodeCount("==", 1)[0]
		nodeClaim := env.EventuallyExpectCreatedNodeClaimCount("==", 1)[0]
		preBootID := node.Status.NodeInfo.BootID

		env.ExpectRebootFaultInjected(node)
		Eventually(func(g Gomega) {
			g.Expect(rebooting(g, nodeClaim).StatusConditions().Get(v1.ConditionTypeRebooting).IsTrue()).To(BeTrue())
		}).Should(Succeed())
		env.ExpectRebootFaultCleared(node)

		expectRebootedInPlace(nodeClaim, node, preBootID)
		Consistently(func(g Gomega) {
			p := &corev1.Pod{}
			g.Expect(env.Client.Get(env, client.ObjectKeyFromObject(pod), p)).To(Succeed())
			g.Expect(p.UID).To(Equal(pod.UID))
			g.Expect(p.DeletionTimestamp.IsZero()).To(BeTrue())
			g.Expect(p.Spec.NodeName).To(Equal(node.Name))
		}, 15*time.Second).Should(Succeed())
	})

	It("should not reboot more nodes at once than the disruption budget allows", func() {
		// Two faulted nodes in a pool of 10 stay within repair's breaker (it trips only when more than ceil(20%) = 2
		// nodes are unhealthy), so both are eligible at once and only the budget of 1 can keep them one at a time.
		nodePool.Spec.Disruption.Budgets = []v1.Budget{{Nodes: "1"}}
		dep.Spec.Replicas = lo.ToPtr[int32](10)
		dep.Spec.Template.Spec.Affinity = &corev1.Affinity{PodAntiAffinity: &corev1.PodAntiAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
				TopologyKey:   corev1.LabelHostname,
				LabelSelector: dep.Spec.Selector,
			}},
		}}
		env.ExpectCreated(nodeClass, nodePool, dep)
		env.EventuallyExpectHealthyPodCount(selector, 10)
		nodes := env.EventuallyExpectInitializedNodeCount("==", 10)
		nodeClaims := env.EventuallyExpectCreatedNodeClaimCount("==", 10)
		faulted := sets.New(nodes[0].Name, nodes[1].Name)
		for _, node := range nodes[:2] {
			env.ExpectRebootFaultInjected(node)
		}

		// Clear each node's fault once its reboot is committed (the reboot clears it), and wait until both have rebooted.
		cleared := map[string]bool{}
		Eventually(func(g Gomega) {
			var inFlight, succeeded int
			for _, nodeClaim := range nodeClaims {
				nc := rebooting(g, nodeClaim)
				if !faulted.Has(nc.Status.NodeName) {
					continue
				}
				cond := nc.StatusConditions().Get(v1.ConditionTypeRebooting)
				switch {
				case cond == nil:
				case cond.IsTrue():
					inFlight++
					if !cleared[nc.Name] {
						env.ExpectRebootFaultCleared(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: nc.Status.NodeName}})
						cleared[nc.Name] = true
					}
				case cond.Reason == v1.RebootReasonSucceeded:
					succeeded++
				}
			}
			if inFlight > 1 {
				StopTrying("more reboots in flight than the disruption budget allows").Now()
			}
			g.Expect(succeeded).To(Equal(2))
		}).WithPolling(time.Second).Should(Succeed())
		env.ExpectNodeClaimCount("==", 10)
	})
})
