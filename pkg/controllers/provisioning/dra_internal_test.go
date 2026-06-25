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

package provisioning

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"

	"sigs.k8s.io/karpenter/pkg/controllers/dynamicresources/deviceallocation"
)

var _ = Describe("DRA Provisioner Internals", func() {
	Describe("nodeOwnerName", func() {
		It("should return the Node owner reference name", func() {
			slice := &resourcev1.ResourceSlice{ObjectMeta: metav1.ObjectMeta{
				OwnerReferences: []metav1.OwnerReference{{Kind: "Node", Name: "node-a"}},
			}}
			name, owned := nodeOwnerName(slice)
			Expect(owned).To(BeTrue())
			Expect(name).To(Equal("node-a"))
		})
		It("should report no owner when there are no owner references", func() {
			slice := &resourcev1.ResourceSlice{}
			_, owned := nodeOwnerName(slice)
			Expect(owned).To(BeFalse())
		})
		It("should report no owner when no owner reference is a Node", func() {
			slice := &resourcev1.ResourceSlice{ObjectMeta: metav1.ObjectMeta{
				OwnerReferences: []metav1.OwnerReference{{Kind: "Pod", Name: "pod-a"}},
			}}
			_, owned := nodeOwnerName(slice)
			Expect(owned).To(BeFalse())
		})
		It("should find the Node owner reference among others", func() {
			slice := &resourcev1.ResourceSlice{ObjectMeta: metav1.ObjectMeta{
				OwnerReferences: []metav1.OwnerReference{{Kind: "Pod", Name: "pod-a"}, {Kind: "Node", Name: "node-b"}},
			}}
			name, owned := nodeOwnerName(slice)
			Expect(owned).To(BeTrue())
			Expect(name).To(Equal("node-b"))
		})
	})

	Describe("allConsumersDeleting", func() {
		var deleting sets.Set[types.UID]

		BeforeEach(func() {
			deleting = sets.New[types.UID]("a", "b")
		})

		It("should be true when all consumers are deleting", func() {
			Expect(allConsumersDeleting([]types.UID{"a", "b"}, deleting)).To(BeTrue())
		})
		It("should be true when consumers are a subset of deleting pods", func() {
			Expect(allConsumersDeleting([]types.UID{"a"}, deleting)).To(BeTrue())
		})
		It("should be false when any consumer is not deleting", func() {
			Expect(allConsumersDeleting([]types.UID{"a", "c"}, deleting)).To(BeFalse())
		})
		It("should be false when no consumer is deleting", func() {
			Expect(allConsumersDeleting([]types.UID{"c"}, deleting)).To(BeFalse())
		})
	})

	Describe("effectiveConsumedCapacity", func() {
		// contribution builds a single-claim contribution reserved by the given pods, consuming the given per-dimension
		// capacity.
		contribution := func(pods []types.UID, capacity map[resourcev1.QualifiedName]string) deviceallocation.ContributionMetadata {
			consumed := map[resourcev1.QualifiedName]resource.Quantity{}
			for dim, qty := range capacity {
				consumed[dim] = resource.MustParse(qty)
			}
			return deviceallocation.ContributionMetadata{PodUIDs: pods, ConsumedCapacity: consumed}
		}
		// sharedMeta builds shared-device metadata whose aggregated ConsumedCapacity is the sum of its contributions.
		sharedMeta := func(contributions ...deviceallocation.ContributionMetadata) deviceallocation.DeviceMetadata {
			aggregate := map[resourcev1.QualifiedName]resource.Quantity{}
			for _, c := range contributions {
				for dim, qty := range c.ConsumedCapacity {
					cur := aggregate[dim]
					cur.Add(qty)
					aggregate[dim] = cur
				}
			}
			return deviceallocation.DeviceMetadata{Shared: true, ConsumedCapacity: aggregate, Contributions: contributions}
		}
		expectCapacity := func(actual map[resourcev1.QualifiedName]resource.Quantity, expected map[resourcev1.QualifiedName]string) {
			GinkgoHelper()
			Expect(actual).To(HaveLen(len(expected)))
			for dim, qty := range expected {
				got, ok := actual[dim]
				Expect(ok).To(BeTrue(), "missing dimension %q", dim)
				Expect(got.Equal(resource.MustParse(qty))).To(BeTrue(), "dimension %q: got %s, want %s", dim, got.String(), qty)
			}
		}

		It("should retain full capacity when no consumer is deleting", func() {
			meta := sharedMeta(
				contribution([]types.UID{"live-a"}, map[resourcev1.QualifiedName]string{"memory": "256Mi", "connections": "1"}),
				contribution([]types.UID{"live-b"}, map[resourcev1.QualifiedName]string{"memory": "128Mi", "connections": "3"}),
			)
			effective := effectiveConsumedCapacity(meta, sets.New[types.UID]())
			expectCapacity(effective, map[resourcev1.QualifiedName]string{"memory": "384Mi", "connections": "4"})
		})

		It("should subtract only the deleting claim's share when mixed with live claims", func() {
			meta := sharedMeta(
				contribution([]types.UID{"live-a"}, map[resourcev1.QualifiedName]string{"memory": "256Mi", "connections": "1"}),
				contribution([]types.UID{"deleting-b"}, map[resourcev1.QualifiedName]string{"memory": "128Mi", "connections": "3"}),
			)
			effective := effectiveConsumedCapacity(meta, sets.New[types.UID]("deleting-b"))
			// Only the live claim's share remains.
			expectCapacity(effective, map[resourcev1.QualifiedName]string{"memory": "256Mi", "connections": "1"})
		})

		It("should free a dimension entirely when the deleting claim consumed all of it", func() {
			meta := sharedMeta(
				contribution([]types.UID{"live-a"}, map[resourcev1.QualifiedName]string{"memory": "256Mi"}),
				contribution([]types.UID{"deleting-b"}, map[resourcev1.QualifiedName]string{"memory": "128Mi", "connections": "3"}),
			)
			effective := effectiveConsumedCapacity(meta, sets.New[types.UID]("deleting-b"))
			// connections was consumed solely by the deleting claim, so it drops out; memory keeps the live share.
			expectCapacity(effective, map[resourcev1.QualifiedName]string{"memory": "256Mi"})
		})

		It("should return empty when every contribution is deleting", func() {
			meta := sharedMeta(
				contribution([]types.UID{"deleting-a"}, map[resourcev1.QualifiedName]string{"memory": "256Mi"}),
				contribution([]types.UID{"deleting-b"}, map[resourcev1.QualifiedName]string{"memory": "128Mi"}),
			)
			effective := effectiveConsumedCapacity(meta, sets.New[types.UID]("deleting-a", "deleting-b"))
			Expect(effective).To(BeEmpty())
		})

		It("should retain a contribution that mixes deleting and live pods", func() {
			// A single claim reserved by both a deleting and a live pod is not fully deleting, so its capacity stays.
			meta := sharedMeta(
				contribution([]types.UID{"deleting-a", "live-b"}, map[resourcev1.QualifiedName]string{"memory": "256Mi"}),
			)
			effective := effectiveConsumedCapacity(meta, sets.New[types.UID]("deleting-a"))
			expectCapacity(effective, map[resourcev1.QualifiedName]string{"memory": "256Mi"})
		})
	})
})
