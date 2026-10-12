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

package cloudprovider_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"sigs.k8s.io/karpenter/pkg/cloudprovider"
)

var _ = Describe("RepairAction", func() {
	It("should rank replacement as more disruptive than reboot", func() {
		Expect(cloudprovider.ReplaceNode.IsMoreDisruptiveThan(cloudprovider.RebootNode)).To(BeTrue())
		Expect(cloudprovider.RebootNode.IsMoreDisruptiveThan(cloudprovider.ReplaceNode)).To(BeFalse())
		Expect(cloudprovider.ReplaceNode.IsMoreDisruptiveThan(cloudprovider.ReplaceNode)).To(BeFalse())
	})
	It("should panic on an unsupported action", func() {
		Expect(func() { cloudprovider.RepairAction("Unknown").IsMoreDisruptiveThan(cloudprovider.ReplaceNode) }).To(Panic())
	})
})
