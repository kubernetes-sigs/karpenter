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

package perf

import (
	"time"

	"github.com/awslabs/operatorpkg/status"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func at(s int) metav1.Time { return metav1.NewTime(t0.Add(time.Duration(s) * time.Second)) }

func nc(name string, s, reg int) v1.NodeClaim {
	c := v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Name: name, CreationTimestamp: at(s)}, Status: v1.NodeClaimStatus{NodeName: name}}
	if reg > 0 {
		c.Status.Conditions = []status.Condition{{Type: v1.ConditionTypeRegistered, Status: metav1.ConditionTrue, LastTransitionTime: at(reg)}}
	}
	return c
}

func pod(node string, s int) corev1.Pod {
	return corev1.Pod{ObjectMeta: metav1.ObjectMeta{CreationTimestamp: at(s)}, Spec: corev1.PodSpec{NodeName: node}}
}

func node(it, ct, zone string) corev1.Node {
	return corev1.Node{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{corev1.LabelInstanceTypeStable: it, v1.CapacityTypeLabelKey: ct, corev1.LabelTopologyZone: zone}}}
}

var _ = Describe("binding timeline", func() {
	DescribeTable("counts rounds and where pods were bound", func(pods []corev1.Pod, ncs []v1.NodeClaim, want Timeline) {
		got := timeline(t0, pods, nil, ncs)
		got.NodeMix = nil
		Expect(got).To(Equal(want))
	},
		Entry("nothing", nil, nil, Timeline{}),
		Entry("one round, all pods on it", []corev1.Pod{pod("a", 0), pod("b", 0)}, []v1.NodeClaim{nc("a", 7, 40), nc("b", 8, 41)}, Timeline{NodeClaimRounds: 1}),
		Entry("pre- and post-registration rounds",
			[]corev1.Pod{pod("a", 0), pod("dup", 0), pod("post", 0), pod("post", 1)},
			[]v1.NodeClaim{nc("a", 7, 40), nc("dup", 16, 45), nc("dup2", 26, 0), nc("post", 40, 70)},
			Timeline{NodeClaimRounds: 4, PreRegDupNodeClaims: 2, PostRegNodeClaims: 1, LatePods: 2, OffRound1Pods: 3}),
		Entry("gaps up to 5s stay in one round", nil, []v1.NodeClaim{nc("a", 0, 0), nc("b", 5, 0), nc("c", 10, 0), nc("d", 16, 0)},
			Timeline{NodeClaimRounds: 2, PreRegDupNodeClaims: 1}),
		Entry("pods on unknown nodes are off round 1 but not late", []corev1.Pod{pod("old", 0), pod("a", 0)}, []v1.NodeClaim{nc("a", 7, 40)},
			Timeline{NodeClaimRounds: 1, OffRound1Pods: 1}),
		Entry("skips NodeClaims and pods from before the phase, and unbound pods",
			[]corev1.Pod{pod("b", -10), pod("", 0), pod("b", 0)}, []v1.NodeClaim{nc("old", -30, -20), nc("b", 7, 40)},
			Timeline{NodeClaimRounds: 1}),
	)
	It("counts the node mix by offering", func() {
		mix := timeline(t0, nil, []corev1.Node{node("m5.large", "spot", "a"), node("c5.xlarge", "on-demand", "b"), node("m5.large", "spot", "a")}, nil).NodeMix
		Expect(mix).To(Equal([]NodeCount{{InstanceType: "c5.xlarge", CapacityType: "on-demand", Zone: "b", Count: 1}, {InstanceType: "m5.large", CapacityType: "spot", Zone: "a", Count: 2}}))
	})
	It("keeps offering prices only for instance types in the node mix", func() {
		g := map[string]float64{
			OfferingPrice + "{capacity_type=spot,instance_type=m5.large,zone=a}":  0.03,
			OfferingPrice + "{capacity_type=spot,instance_type=c5.xlarge,zone=a}": 0.05,
			"go_goroutines": 40,
		}
		keepMixPrices(g, []NodeCount{{InstanceType: "m5.large"}})
		Expect(g).To(HaveLen(2))
		Expect(g).To(HaveKey(OfferingPrice + "{capacity_type=spot,instance_type=m5.large,zone=a}"))
	})
})
