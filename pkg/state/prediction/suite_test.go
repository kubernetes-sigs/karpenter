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

package prediction

import (
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

func TestPrediction(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Prediction Store")
}

var _ = Describe("Store", func() {
	var store *Store

	BeforeEach(func() {
		store = NewStore()
	})

	It("should keep targets with the same kind and name in different groups apart", func() {
		source := types.NamespacedName{Namespace: "default", Name: "vpa-1"}
		other := TargetKey{GroupKind: schema.GroupKind{Group: "example.com", Kind: "Deployment"}, NamespacedName: types.NamespacedName{Namespace: "default", Name: "app"}}
		store.Set(source, TargetKey{GroupKind: schema.GroupKind{Group: "apps", Kind: "Deployment"}, NamespacedName: types.NamespacedName{Namespace: "default", Name: "app"}}, &Prediction{}, time.Now())

		_, ok := store.Get(other)
		Expect(ok).To(BeFalse())
	})

	It("should store and update a prediction by target key", func() {
		source := types.NamespacedName{Namespace: "default", Name: "vpa-1"}
		target := TargetKey{GroupKind: schema.GroupKind{Group: "apps", Kind: "Deployment"}, NamespacedName: types.NamespacedName{Namespace: "default", Name: "app"}}
		pred1 := &Prediction{Containers: map[string]corev1.ResourceList{
			"container1": {
				corev1.ResourceCPU:    resource.MustParse("100m"),
				corev1.ResourceMemory: resource.MustParse("128Mi"),
			},
		}}
		pred2 := &Prediction{Containers: map[string]corev1.ResourceList{
			"container1": {corev1.ResourceCPU: resource.MustParse("200m")},
		}}

		store.Set(source, target, pred1, time.Now())
		retrieved, ok := store.Get(target)
		Expect(ok).To(BeTrue())
		Expect(retrieved).To(Equal(pred1))

		store.Set(source, target, pred2, time.Now())
		retrieved, ok = store.Get(target)
		Expect(ok).To(BeTrue())
		Expect(retrieved).To(Equal(pred2))
	})

	It("should delete previous target when source is retargeted", func() {
		source := types.NamespacedName{Namespace: "default", Name: "vpa-1"}
		target1 := TargetKey{GroupKind: schema.GroupKind{Group: "apps", Kind: "Deployment"}, NamespacedName: types.NamespacedName{Namespace: "default", Name: "app1"}}
		target2 := TargetKey{GroupKind: schema.GroupKind{Group: "apps", Kind: "Deployment"}, NamespacedName: types.NamespacedName{Namespace: "default", Name: "app2"}}
		pred1 := &Prediction{Containers: map[string]corev1.ResourceList{
			"container1": {corev1.ResourceCPU: resource.MustParse("100m")},
		}}
		pred2 := &Prediction{Containers: map[string]corev1.ResourceList{
			"container1": {corev1.ResourceCPU: resource.MustParse("200m")},
		}}

		store.Set(source, target1, pred1, time.Now())
		store.Set(source, target2, pred2, time.Now())

		_, ok := store.Get(target1)
		Expect(ok).To(BeFalse())

		retrieved, ok := store.Get(target2)
		Expect(ok).To(BeTrue())
		Expect(retrieved).To(Equal(pred2))
	})

	It("should delete a prediction and be idempotent", func() {
		source := types.NamespacedName{Namespace: "default", Name: "vpa-1"}
		target := TargetKey{GroupKind: schema.GroupKind{Group: "apps", Kind: "Deployment"}, NamespacedName: types.NamespacedName{Namespace: "default", Name: "app"}}
		pred := &Prediction{Containers: map[string]corev1.ResourceList{
			"c": {corev1.ResourceCPU: resource.MustParse("100m")},
		}}

		store.Set(source, target, pred, time.Now())
		store.Delete(source)

		_, ok := store.Get(target)
		Expect(ok).To(BeFalse())

		Expect(func() { store.Delete(source) }).NotTo(Panic())
	})

	It("should report whether the active prediction changed", func() {
		target := TargetKey{GroupKind: schema.GroupKind{Group: "apps", Kind: "Deployment"}, NamespacedName: types.NamespacedName{Namespace: "default", Name: "app"}}
		older := types.NamespacedName{Namespace: "default", Name: "vpa-older"}
		newer := types.NamespacedName{Namespace: "default", Name: "vpa-newer"}
		cpu := func(q string) *Prediction {
			return &Prediction{Containers: map[string]corev1.ResourceList{"c": {corev1.ResourceCPU: resource.MustParse(q)}}}
		}
		t1 := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
		t2 := time.Date(2026, 1, 1, 10, 5, 0, 0, time.UTC)

		Expect(store.Set(older, target, cpu("500m"), t1)).To(BeTrue())
		Expect(store.Set(older, target, cpu("500m"), t1)).To(BeFalse())
		Expect(store.Set(older, target, cpu("1"), t1)).To(BeTrue())
		Expect(store.Set(newer, target, cpu("2"), t2)).To(BeFalse())
		Expect(store.Delete(newer)).To(BeFalse())
		Expect(store.Set(newer, target, cpu("2"), t2)).To(BeFalse())
		Expect(store.Delete(older)).To(BeTrue())
		Expect(store.Delete(older)).To(BeFalse())
		Expect(store.Reset()).To(BeTrue())
		Expect(store.Reset()).To(BeFalse())
	})

	Context("Tie-Breaking", func() {
		It("should use the earliest-created source's prediction", func() {
			target := TargetKey{GroupKind: schema.GroupKind{Group: "apps", Kind: "Deployment"}, NamespacedName: types.NamespacedName{Namespace: "default", Name: "app"}}
			older := types.NamespacedName{Namespace: "default", Name: "vpa-older"}
			newer := types.NamespacedName{Namespace: "default", Name: "vpa-newer"}
			predOlder := &Prediction{Containers: map[string]corev1.ResourceList{
				"c": {corev1.ResourceCPU: resource.MustParse("500m")},
			}}
			predNewer := &Prediction{Containers: map[string]corev1.ResourceList{
				"c": {corev1.ResourceCPU: resource.MustParse("800m")},
			}}

			t1 := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
			t2 := time.Date(2026, 1, 1, 10, 5, 0, 0, time.UTC)

			// Set newer first, then older — older should win regardless of insertion order
			store.Set(newer, target, predNewer, t2)
			store.Set(older, target, predOlder, t1)

			retrieved, ok := store.Get(target)
			Expect(ok).To(BeTrue())
			Expect(retrieved).To(Equal(predOlder))
		})

		It("should break ties by lexicographically smallest name when timestamps are equal", func() {
			target := TargetKey{GroupKind: schema.GroupKind{Group: "apps", Kind: "Deployment"}, NamespacedName: types.NamespacedName{Namespace: "default", Name: "app"}}
			sourceA := types.NamespacedName{Namespace: "default", Name: "vpa-alpha"}
			sourceB := types.NamespacedName{Namespace: "default", Name: "vpa-beta"}
			predA := &Prediction{Containers: map[string]corev1.ResourceList{
				"c": {corev1.ResourceCPU: resource.MustParse("500m")},
			}}
			predB := &Prediction{Containers: map[string]corev1.ResourceList{
				"c": {corev1.ResourceCPU: resource.MustParse("800m")},
			}}

			ts := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)

			store.Set(sourceB, target, predB, ts)
			store.Set(sourceA, target, predA, ts)

			retrieved, ok := store.Get(target)
			Expect(ok).To(BeTrue())
			Expect(retrieved).To(Equal(predA))
		})

		It("should promote next-strongest on delete of the winner", func() {
			target := TargetKey{GroupKind: schema.GroupKind{Group: "apps", Kind: "Deployment"}, NamespacedName: types.NamespacedName{Namespace: "default", Name: "app"}}
			older := types.NamespacedName{Namespace: "default", Name: "vpa-older"}
			newer := types.NamespacedName{Namespace: "default", Name: "vpa-newer"}
			predOlder := &Prediction{Containers: map[string]corev1.ResourceList{
				"c": {corev1.ResourceCPU: resource.MustParse("500m")},
			}}
			predNewer := &Prediction{Containers: map[string]corev1.ResourceList{
				"c": {corev1.ResourceCPU: resource.MustParse("800m")},
			}}

			t1 := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
			t2 := time.Date(2026, 1, 1, 10, 5, 0, 0, time.UTC)

			store.Set(older, target, predOlder, t1)
			store.Set(newer, target, predNewer, t2)

			// Older wins initially
			retrieved, ok := store.Get(target)
			Expect(ok).To(BeTrue())
			Expect(retrieved).To(Equal(predOlder))

			// Delete the winner — newer should be promoted
			store.Delete(older)
			retrieved, ok = store.Get(target)
			Expect(ok).To(BeTrue())
			Expect(retrieved).To(Equal(predNewer))
		})

		It("should remove target entirely when all contenders are deleted", func() {
			target := TargetKey{GroupKind: schema.GroupKind{Group: "apps", Kind: "Deployment"}, NamespacedName: types.NamespacedName{Namespace: "default", Name: "app"}}
			source1 := types.NamespacedName{Namespace: "default", Name: "vpa-1"}
			source2 := types.NamespacedName{Namespace: "default", Name: "vpa-2"}
			pred := &Prediction{Containers: map[string]corev1.ResourceList{
				"c": {corev1.ResourceCPU: resource.MustParse("100m")},
			}}

			t1 := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
			t2 := time.Date(2026, 1, 1, 10, 5, 0, 0, time.UTC)

			store.Set(source1, target, pred, t1)
			store.Set(source2, target, pred, t2)

			store.Delete(source1)
			store.Delete(source2)

			_, ok := store.Get(target)
			Expect(ok).To(BeFalse())
		})
	})
})
