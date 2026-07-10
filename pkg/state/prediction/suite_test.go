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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
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

	It("should store and update a prediction by target key", func() {
		source := types.NamespacedName{Namespace: "default", Name: "vpa-1"}
		target := TargetKey{Namespace: "default", Kind: "Deployment", Name: "app"}
		pred1 := &Prediction{Containers: map[string]corev1.ResourceList{
			"container1": {
				corev1.ResourceCPU:    resource.MustParse("100m"),
				corev1.ResourceMemory: resource.MustParse("128Mi"),
			},
		}}
		pred2 := &Prediction{Containers: map[string]corev1.ResourceList{
			"container1": {corev1.ResourceCPU: resource.MustParse("200m")},
		}}

		store.Set(source, target, pred1)
		retrieved, ok := store.Get("default", "Deployment", "app")
		Expect(ok).To(BeTrue())
		Expect(retrieved).To(Equal(pred1))

		store.Set(source, target, pred2)
		retrieved, ok = store.Get("default", "Deployment", "app")
		Expect(ok).To(BeTrue())
		Expect(retrieved).To(Equal(pred2))
	})

	It("should delete previous target when source is retargeted", func() {
		source := types.NamespacedName{Namespace: "default", Name: "vpa-1"}
		target1 := TargetKey{Namespace: "default", Kind: "Deployment", Name: "app1"}
		target2 := TargetKey{Namespace: "default", Kind: "Deployment", Name: "app2"}
		pred1 := &Prediction{Containers: map[string]corev1.ResourceList{
			"container1": {corev1.ResourceCPU: resource.MustParse("100m")},
		}}
		pred2 := &Prediction{Containers: map[string]corev1.ResourceList{
			"container1": {corev1.ResourceCPU: resource.MustParse("200m")},
		}}

		store.Set(source, target1, pred1)
		store.Set(source, target2, pred2)

		_, ok := store.Get("default", "Deployment", "app1")
		Expect(ok).To(BeFalse())

		retrieved, ok := store.Get("default", "Deployment", "app2")
		Expect(ok).To(BeTrue())
		Expect(retrieved).To(Equal(pred2))
	})

	It("should delete a prediction and be idempotent", func() {
		source := types.NamespacedName{Namespace: "default", Name: "vpa-1"}
		target := TargetKey{Namespace: "default", Kind: "Deployment", Name: "app"}
		pred := &Prediction{Containers: map[string]corev1.ResourceList{
			"c": {corev1.ResourceCPU: resource.MustParse("100m")},
		}}

		store.Set(source, target, pred)
		store.Delete(source)

		_, ok := store.Get("default", "Deployment", "app")
		Expect(ok).To(BeFalse())

		// Deleting again should not panic
		Expect(func() { store.Delete(source) }).NotTo(Panic())
	})
})
