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

package disruption

import (
	"slices"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clocktesting "k8s.io/utils/clock/testing"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/controllers/node/health"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
)

var _ = Describe("RebootHistory", func() {
	candidateWithAction := func(uid types.UID, action cloudprovider.RepairAction) *Candidate {
		return &Candidate{
			StateNode:          &state.StateNode{NodeClaim: &v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{UID: uid}}},
			RepairPolicyResult: health.RepairResult{Action: action, Condition: "AcceleratorReady", Reason: "XID48"},
		}
	}

	It("escalates after two committed reboots", func() {
		history := NewRebootHistory()
		candidate := candidateWithAction("nodeclaim-uid", cloudprovider.RebootNode)
		Expect(history.Resolve(candidate)).To(BeTrue())
		Expect(candidate.RepairPolicyResult.Action).To(Equal(cloudprovider.RebootNode))

		history.RecordCommittedReboot(candidate.NodeClaim.UID)
		candidate = candidateWithAction("nodeclaim-uid", cloudprovider.RebootNode)
		Expect(history.Resolve(candidate)).To(BeTrue())
		Expect(candidate.RepairPolicyResult.Action).To(Equal(cloudprovider.RebootNode))
		Expect(candidate.RebootEscalated).To(BeFalse())

		history.RecordCommittedReboot(candidate.NodeClaim.UID)
		candidate = candidateWithAction("nodeclaim-uid", cloudprovider.RebootNode)
		Expect(history.Resolve(candidate)).To(BeTrue())
		Expect(candidate.RepairPolicyResult.Action).To(Equal(cloudprovider.ReplaceNode))
		Expect(candidate.RepairPolicyResult.Condition).To(Equal(corev1.NodeConditionType("AcceleratorReady")))
		Expect(candidate.RebootEscalated).To(BeTrue())
	})

	It("does not record reboots while resolving", func() {
		history := NewRebootHistory()
		for range 3 {
			candidate := candidateWithAction("nodeclaim-uid", cloudprovider.RebootNode)
			Expect(history.Resolve(candidate)).To(BeTrue())
			Expect(candidate.RepairPolicyResult.Action).To(Equal(cloudprovider.RebootNode))
		}
	})

	It("keys history by NodeClaim UID", func() {
		history := NewRebootHistory()
		history.RecordCommittedReboot("original-uid")
		history.RecordCommittedReboot("original-uid")

		successor := candidateWithAction("successor-uid", cloudprovider.RebootNode)
		Expect(history.Resolve(successor)).To(BeTrue())
		Expect(successor.RepairPolicyResult.Action).To(Equal(cloudprovider.RebootNode))
	})

	It("preserves replacement action and requires a current decision", func() {
		history := NewRebootHistory()
		history.RecordCommittedReboot("nodeclaim-uid")
		history.RecordCommittedReboot("nodeclaim-uid")

		replacement := candidateWithAction("nodeclaim-uid", cloudprovider.ReplaceNode)
		Expect(history.Resolve(replacement)).To(BeTrue())
		Expect(replacement.RepairPolicyResult.Action).To(Equal(cloudprovider.ReplaceNode))
		Expect(replacement.RebootEscalated).To(BeFalse())

		healthy := candidateWithAction("nodeclaim-uid", "")
		healthy.RebootEscalated = true
		Expect(history.Resolve(healthy)).To(BeFalse())
		Expect(healthy.RebootEscalated).To(BeFalse())
	})

	It("uses a sliding window", func() {
		clk := clocktesting.NewFakeClock(time.Unix(1, 0))
		history := newRebootHistory(clk)

		history.RecordCommittedReboot("nodeclaim-uid")
		clk.Step(13 * time.Hour)
		history.RecordCommittedReboot("nodeclaim-uid")

		clk.Step(12 * time.Hour)
		candidate := candidateWithAction("nodeclaim-uid", cloudprovider.RebootNode)
		Expect(history.Resolve(candidate)).To(BeTrue())
		Expect(candidate.RepairPolicyResult.Action).To(Equal(cloudprovider.RebootNode))

		history.RecordCommittedReboot("nodeclaim-uid")
		candidate = candidateWithAction("nodeclaim-uid", cloudprovider.RebootNode)
		Expect(history.Resolve(candidate)).To(BeTrue())
		Expect(candidate.RepairPolicyResult.Action).To(Equal(cloudprovider.ReplaceNode))
	})

	It("bounds concurrent commits at the escalation threshold", func() {
		history := NewRebootHistory()
		const commits = 32

		var wg sync.WaitGroup
		start := make(chan struct{})
		for range commits {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				history.RecordCommittedReboot("nodeclaim-uid")
			}()
		}
		close(start)
		wg.Wait()

		Expect(history.recentReboots("nodeclaim-uid", history.clock.Now()).count).To(Equal(rebootsBeforeReplacement))
	})

	It("logs reboot escalation with the command", func() {
		history := NewRebootHistory()
		history.RecordCommittedReboot("nodeclaim-uid")
		history.RecordCommittedReboot("nodeclaim-uid")

		escalated := candidateWithAction("nodeclaim-uid", cloudprovider.RebootNode)
		escalated.Node = &corev1.Node{}
		Expect(history.Resolve(escalated)).To(BeTrue())
		replaced := candidateWithAction("other-uid", cloudprovider.ReplaceNode)
		replaced.Node = &corev1.Node{}
		Expect(history.Resolve(replaced)).To(BeTrue())

		values := Command{Candidates: []*Candidate{escalated, replaced}}.LogValues()
		disruptedNodes := values[slices.Index(values, any("disrupted-nodes"))+1].([]any)
		Expect(disruptedNodes[0]).To(HaveKeyWithValue("reboot-escalated", true))
		Expect(disruptedNodes[1]).ToNot(HaveKey("reboot-escalated"))
	})
})
