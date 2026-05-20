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

package nodeoverlay

import (
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
)

var _ = Describe("Issue 3003 Reproduction", func() {
	It("should not block all provisioning if one NodePool fails evaluation", func() {
		// Create two NodePools
		np1 := test.NodePool()
		np2 := test.NodePool()
		ExpectApplied(ctx, env.Client, np1, np2)

		// Make GetInstanceTypes fail for np1 but succeed for np2
		cloudProvider.ErrorsForNodePool[np1.Name] = fmt.Errorf("failed to get instance types for np1")

		// Run Reconcile — the whole point of the fix is that np1's GetInstanceTypes
		// failure must not propagate into a reconcile error that aborts processing
		// of np2 and subsequent NodePools.
		_, err := nodeOverlayController.Reconcile(ctx, reconcile.Request{})
		Expect(err).ToNot(HaveOccurred())

		// Checking the store for np2
		_, err = store.ApplyAll(np2.Name, cloudProvider.InstanceTypes)
		Expect(err).ToNot(HaveOccurred())
	})
})
