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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/types"
	clock "k8s.io/utils/clock/testing"

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
	var clk *clock.FakeClock
	var registry *launched.Registry

	BeforeEach(func() {
		clk = clock.NewFakeClock(time.Now())
		registry = launched.NewRegistry(clk)
	})

	It("should look up launch results by UID and providerID", func() {
		registry.Add("a", created("p-a"))
		Expect(registry.ProviderID("a")).To(Equal("p-a"))
		Expect(registry.HasProviderID("p-a")).To(BeTrue())
		Expect(registry.HasProviderID("p-b")).To(BeFalse())
		Expect(registry.HasProviderID("")).To(BeFalse())
		Expect(registry.ProviderID("b")).To(BeEmpty())
	})
	It("should remove both indexes on delete", func() {
		registry.Add("a", created("p-a"))
		registry.Delete("a")
		Expect(registry.ProviderID("a")).To(BeEmpty())
		Expect(registry.HasProviderID("p-a")).To(BeFalse())
		Expect(registry.Len()).To(BeZero())
	})
	It("should drop the old providerID when a UID is re-added with a new one", func() {
		registry.Add("a", created("p-1"))
		registry.Add("a", created("p-2"))
		Expect(registry.HasProviderID("p-1")).To(BeFalse())
		Expect(registry.HasProviderID("p-2")).To(BeTrue())
	})
	It("should not index an empty providerID", func() {
		registry.Add("a", created(""))
		_, ok := registry.Get("a")
		Expect(ok).To(BeTrue())
		Expect(registry.HasProviderID("")).To(BeFalse())
	})
	It("should expire entries that are not refreshed within the TTL", func() {
		registry.Add("a", created("p-a"))
		clk.Step(launched.TTL - time.Second)
		registry.Add("a", created("p-a")) // refresh
		clk.Step(launched.TTL - time.Second)
		Expect(registry.HasProviderID("p-a")).To(BeTrue())
		clk.Step(time.Second)
		Expect(registry.HasProviderID("p-a")).To(BeFalse())
		_, ok := registry.Get("a")
		Expect(ok).To(BeFalse())
		registry.Add(types.UID("b"), created("p-b")) // sweeps expired entries
		Expect(registry.Len()).To(Equal(1))
	})
})
