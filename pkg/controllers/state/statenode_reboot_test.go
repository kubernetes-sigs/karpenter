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

package state_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/clock"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/test"
)

var _ = Describe("StateNode Reboot", func() {
	// stateNode builds an initialized, registered, managed StateNode. rebootStatus, when non-empty, sets
	// the Rebooting condition to that status; taints are placed on the Node.
	stateNode := func(rebootStatus metav1.ConditionStatus, taints ...corev1.Taint) *state.StateNode {
		nc := test.NodeClaim(v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{v1.NodePoolLabelKey: "default"}}})
		n := test.Node(test.NodeOptions{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
			v1.NodePoolLabelKey:        "default",
			v1.NodeRegisteredLabelKey:  "true",
			v1.NodeInitializedLabelKey: "true",
		}}})
		n.Spec.Taints = taints
		switch rebootStatus {
		case metav1.ConditionTrue:
			nc.StatusConditions().SetTrueWithReason(v1.ConditionTypeRebooting, v1.RebootReasonRequested, "reboot requested")
		case metav1.ConditionFalse:
			nc.StatusConditions().SetFalse(v1.ConditionTypeRebooting, v1.RebootReasonSucceeded, "done")
		}
		return &state.StateNode{Node: n, NodeClaim: nc}
	}

	Context("RebootInProgress", func() {
		It("is true when the Rebooting condition is True", func() {
			Expect(stateNode(metav1.ConditionTrue).RebootInProgress()).To(BeTrue())
		})
		It("is false when the Rebooting condition is terminal (False)", func() {
			Expect(stateNode(metav1.ConditionFalse).RebootInProgress()).To(BeFalse())
		})
		It("is false when there is no Rebooting condition", func() {
			Expect(stateNode("").RebootInProgress()).To(BeFalse())
		})
		It("is false when the StateNode has no NodeClaim", func() {
			Expect((&state.StateNode{Node: test.Node()}).RebootInProgress()).To(BeFalse())
		})
	})

	Context("ValidateNodeDisruptable", func() {
		It("rejects a rebooting node so other disruption skips it", func() {
			err := stateNode(metav1.ConditionTrue).ValidateNodeDisruptable(clock.RealClock{})
			Expect(err).To(MatchError(ContainSubstring("node is rebooting")))
		})
		It("does not reject a node that is not rebooting", func() {
			Expect(stateNode(metav1.ConditionFalse).ValidateNodeDisruptable(clock.RealClock{})).To(Succeed())
		})
	})

	Context("Taints", func() {
		custom := corev1.Taint{Key: "example.com/custom", Effect: corev1.TaintEffectNoSchedule}
		It("strips the reboot fence taint while rebooting so capacity models as returning", func() {
			taints := stateNode(metav1.ConditionTrue, v1.RebootingNoScheduleTaint, custom).Taints()
			Expect(taints).To(ContainElement(custom))
			Expect(lo.ContainsBy(taints, func(t corev1.Taint) bool { return t.MatchTaint(&v1.RebootingNoScheduleTaint) })).To(BeFalse())
		})
		It("retains the reboot fence taint on an initialized node that is not rebooting", func() {
			taints := stateNode(metav1.ConditionFalse, v1.RebootingNoScheduleTaint, custom).Taints()
			Expect(lo.ContainsBy(taints, func(t corev1.Taint) bool { return t.MatchTaint(&v1.RebootingNoScheduleTaint) })).To(BeTrue())
		})
	})
})
