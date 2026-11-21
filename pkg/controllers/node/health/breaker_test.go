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

package health

import (
	"context"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clocktesting "k8s.io/utils/clock/testing"

	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	. "sigs.k8s.io/karpenter/pkg/utils/testing"
)

var _ = Describe("Breaker", func() {
	now := time.Date(2026, time.January, 1, 12, 0, 0, 0, time.UTC)
	created := now.Add(-24 * time.Hour)

	// node returns a node created a day ago, with the conditions.
	node := func(conditions ...corev1.NodeCondition) corev1.Node {
		return corev1.Node{
			ObjectMeta: metav1.ObjectMeta{CreationTimestamp: metav1.NewTime(created)},
			Status:     corev1.NodeStatus{Conditions: conditions},
		}
	}
	// minutesAgo returns the times that many minutes before now.
	minutesAgo := func(minutes ...int) []time.Time {
		return lo.Map(minutes, func(m int, _ int) time.Time { return now.Add(-time.Duration(m) * time.Minute) })
	}

	DescribeTable("should trip only when more than 20% of the nodes, rounded up, are unhealthy",
		func(nodes, unhealthy int, tripped bool) {
			Expect(CheckBreaker(nodes, unhealthy)).To(Equal(Breaker{Nodes: nodes, Unhealthy: unhealthy, Tripped: tripped}))
			// TrippedAt agrees on whether the breaker is tripped.
			Expect(TrippedAt(nodes, minutesAgo(lo.Range(unhealthy)...)).IsZero()).To(Equal(!tripped))
		},
		Entry("no nodes", 0, 0, false),
		Entry("1 of 1 rounds up to the limit", 1, 1, false),
		Entry("2 of 2 is over the limit", 2, 2, true),
		Entry("1 of 3 rounds up to the limit", 3, 1, false),
		Entry("2 of 3 is over the limit", 3, 2, true),
		Entry("2 of 10 is at the limit", 10, 2, false),
		Entry("3 of 10 is over the limit", 10, 3, true),
	)
	It("should date the trip to when the first node past the limit became unhealthy", func() {
		// 10 nodes allow 2 unhealthy, so the third node to become unhealthy tripped the breaker. The order is shuffled, so
		// the third time in the list isn't the answer.
		Expect(TrippedAt(10, minutesAgo(20, 10, 40, 30))).To(BeTemporally("==", now.Add(-20*time.Minute)))
	})

	Context("TransitionTime", func() {
		It("should not be earlier than the node's creation", func() {
			n := node()
			Expect(TransitionTime(&n, corev1.NodeCondition{LastTransitionTime: metav1.NewTime(created.Add(-time.Hour))})).To(BeTemporally("==", created))
			Expect(TransitionTime(&n, corev1.NodeCondition{})).To(BeTemporally("==", created))
			Expect(TransitionTime(&n, corev1.NodeCondition{LastTransitionTime: metav1.NewTime(now)})).To(BeTemporally("==", now))
		})
	})

	Context("UnhealthySince", func() {
		var matcher *RepairPolicyMatcher
		BeforeEach(func() {
			matcher = lo.Must(newRepairPolicyMatcher([]cloudprovider.RepairPolicy{
				{ConditionType: corev1.NodeReady, ConditionStatus: corev1.ConditionFalse, TolerationDuration: 30 * time.Minute, Action: cloudprovider.ReplaceNode},
				{ConditionType: "AcceleratorReady", ConditionStatus: corev1.ConditionFalse, ReasonRegex: "^XID$", TolerationDuration: 10 * time.Minute, Action: cloudprovider.RebootNode},
			}, supportedRepairActions))
		})
		It("should be the earliest condition the policies cover, whatever its reason or toleration", func() {
			n := node(
				corev1.NodeCondition{Type: corev1.NodeReady, Status: corev1.ConditionFalse, LastTransitionTime: metav1.NewTime(now.Add(-5 * time.Minute))},
				// The reason doesn't match the reboot policy, but the breaker doesn't look at reasons.
				corev1.NodeCondition{Type: "AcceleratorReady", Status: corev1.ConditionFalse, Reason: "Other", LastTransitionTime: metav1.NewTime(now.Add(-10 * time.Minute))},
				corev1.NodeCondition{Type: corev1.NodeDiskPressure, Status: corev1.ConditionTrue, LastTransitionTime: metav1.NewTime(now.Add(-time.Hour))},
			)
			since, unhealthy := matcher.UnhealthySince(&n)
			Expect(unhealthy).To(BeTrue())
			Expect(since).To(BeTemporally("==", now.Add(-10*time.Minute)))
		})
		It("should report a node whose conditions no policy covers as healthy", func() {
			n := node(
				corev1.NodeCondition{Type: corev1.NodeReady, Status: corev1.ConditionTrue, LastTransitionTime: metav1.NewTime(now)},
				corev1.NodeCondition{Type: corev1.NodeDiskPressure, Status: corev1.ConditionFalse, LastTransitionTime: metav1.NewTime(now)},
			)
			_, unhealthy := matcher.UnhealthySince(&n)
			Expect(unhealthy).To(BeFalse())
		})
		It("should clamp to the node's creation, as Match does", func() {
			n := node(corev1.NodeCondition{Type: corev1.NodeReady, Status: corev1.ConditionFalse, LastTransitionTime: metav1.NewTime(created.Add(-time.Hour))})
			since, unhealthy := matcher.UnhealthySince(&n)
			Expect(unhealthy).To(BeTrue())
			Expect(since).To(BeTemporally("==", created))
			Expect(matcher.Match(&n)).To(HaveLen(1))
			Expect(matcher.Match(&n)[0].eligibleAt).To(BeTemporally("==", since.Add(30*time.Minute)))
		})
	})

	Context("BreakerLog", func() {
		var clk *clocktesting.FakeClock
		var logCtx context.Context
		var logs func() []string
		var breakerLog *BreakerLog
		tripped := Breaker{Nodes: 10, Unhealthy: 3, Tripped: true}

		BeforeEach(func() {
			clk = clocktesting.NewFakeClock(now)
			logCtx, logs = CaptureLogs(context.Background(), "blocking node repair")
			breakerLog = NewBreakerLog(clk)
		})
		It("should log the NodePool and its counts", func() {
			breakerLog.Blocked(logCtx, "pool-a", tripped)
			Expect(logs()).To(HaveLen(1))
			Expect(logs()[0]).To(ContainSubstring(`"msg"="blocking node repair, more than 20% of nodes are unhealthy"`))
			Expect(logs()[0]).To(ContainSubstring(`"NodePool"={"name"="pool-a"}`))
			Expect(logs()[0]).To(ContainSubstring(`"nodes"=10 "unhealthy-nodes"=3`))
		})
		It("should log each NodePool at most once per interval", func() {
			breakerLog.Blocked(logCtx, "pool-a", tripped)
			breakerLog.Blocked(logCtx, "pool-a", tripped)
			breakerLog.Blocked(logCtx, "pool-b", tripped)
			Expect(logs()).To(HaveLen(2))

			clk.Step(BlockedRepeatInterval - time.Second)
			breakerLog.Blocked(logCtx, "pool-a", tripped)
			Expect(logs()).To(HaveLen(2))

			clk.Step(time.Second)
			breakerLog.Blocked(logCtx, "pool-a", tripped)
			Expect(logs()).To(HaveLen(3))
		})
		It("should log a cluster-wide block without a NodePool", func() {
			breakerLog.Blocked(logCtx, "", tripped)
			breakerLog.Blocked(logCtx, "", tripped)
			Expect(logs()).To(HaveLen(1))
			Expect(logs()[0]).ToNot(ContainSubstring(`"NodePool"`))
		})
		It("should log each NodePool once when many reconciles block at the same time", func() {
			var wg sync.WaitGroup
			for i := range 50 {
				wg.Go(func() { breakerLog.Blocked(logCtx, []string{"pool-a", "pool-b", ""}[i%3], tripped) })
			}
			wg.Wait()
			Expect(logs()).To(HaveLen(3))
		})
	})
})
