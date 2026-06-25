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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
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
})
