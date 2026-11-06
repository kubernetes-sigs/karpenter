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
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/test"
)

var _ = Describe("StateNode reads", func() {
	It("don't write conditions into the shared NodeClaim", func() {
		nc := test.NodeClaim()
		nc.Status.Conditions = nil
		sn := &state.StateNode{Node: test.Node(), NodeClaim: nc}

		var wg sync.WaitGroup
		for range 2 {
			wg.Go(func() {
				for range 100 {
					sn.Deleted()
					sn.RebootInProgress()
					sn.GetRepairResult(time.Now())
				}
			})
		}
		wg.Wait()
		Expect(nc.Status.Conditions).To(BeEmpty())
	})
})
