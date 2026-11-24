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

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/test"
)

var _ = Describe("StateNode condition predicates", func() {
	// Reading a status condition through the default ConditionSet initializes any absent root or dependent condition
	// to Unknown, which writes the object. These predicates are called against cluster state's own NodeClaim while
	// only its read lock is held -- Nodes() hands out the live StateNode -- so none of them may write it.
	It("should not write the NodeClaim they read a condition from", func() {
		nodeClaim := test.NodeClaim()
		nodeClaim.Status.Conditions = nil
		stateNode := &state.StateNode{NodeClaim: nodeClaim}

		Expect(stateNode.RebootInProgress()).To(BeFalse())
		Expect(stateNode.RebootReinitializing()).To(BeFalse())
		Expect(stateNode.Deleted()).To(BeFalse())
		Expect(stateNode.MarkedForDeletion()).To(BeFalse())
		Expect(stateNode.GetRepairResult(env.Clock.Now()).Action).To(BeEmpty())

		Expect(nodeClaim.Status.Conditions).To(BeEmpty())
	})

	It("should still read a condition that is set", func() {
		nodeClaim := test.NodeClaim()
		nodeClaim.StatusConditions().SetTrue(v1.ConditionTypeRebooting)
		stateNode := &state.StateNode{NodeClaim: nodeClaim}
		Expect(stateNode.RebootInProgress()).To(BeTrue())

		nodeClaim.StatusConditions().SetTrue(v1.ConditionTypeInstanceTerminating)
		Expect(stateNode.Deleted()).To(BeTrue())
	})
})
