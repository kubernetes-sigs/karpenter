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

package launched_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/controllers/nodeclaim/lifecycle/launched"
)

func TestLaunched(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Launched")
}

func created(providerID string) *v1.NodeClaim {
	return &v1.NodeClaim{Status: v1.NodeClaimStatus{ProviderID: providerID}}
}

var _ = Describe("Registry", func() {
	var registry *launched.Registry

	BeforeEach(func() {
		registry = launched.NewRegistry()
	})

	It("should return launch results by UID", func() {
		registry.Add("a", created("p-a"))
		nc, ok := registry.Get("a")
		Expect(ok).To(BeTrue())
		Expect(nc.Status.ProviderID).To(Equal("p-a"))
		_, ok = registry.Get("b")
		Expect(ok).To(BeFalse())
	})
	It("should list providerIDs, skipping empty ones", func() {
		registry.Add("a", created("p-a"))
		registry.Add("b", created("p-b"))
		registry.Add("c", created(""))
		Expect(registry.ProviderIDs().UnsortedList()).To(ConsistOf("p-a", "p-b"))
	})
	It("should replace the providerID when a UID is re-added", func() {
		registry.Add("a", created("p-1"))
		registry.Add("a", created("p-2"))
		Expect(registry.ProviderIDs().UnsortedList()).To(ConsistOf("p-2"))
	})
	It("should drop deleted entries", func() {
		registry.Add("a", created("p-a"))
		registry.Delete("a")
		_, ok := registry.Get("a")
		Expect(ok).To(BeFalse())
		Expect(registry.ProviderIDs()).To(BeEmpty())
	})
	It("should hold nothing when nil", func() {
		var nilRegistry *launched.Registry
		Expect(nilRegistry.ProviderIDs()).To(BeEmpty())
	})
})
