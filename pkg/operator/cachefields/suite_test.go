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

package cachefields_test

import (
	"reflect"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
	toolscache "k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"sigs.k8s.io/karpenter/pkg/operator/cachefields"
)

func TestCacheFields(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "CacheFields")
}

var managedFields = []metav1.ManagedFieldsEntry{{Manager: "kubectl"}}

func newNode() *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node", Labels: map[string]string{"a": "b"}, ManagedFields: managedFields},
		Status: corev1.NodeStatus{
			Images:   []corev1.ContainerImage{{Names: []string{"image"}}},
			NodeInfo: corev1.NodeSystemInfo{BootID: "boot"},
		},
	}
}

func newPod() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pod", ManagedFields: managedFields},
		Spec:       corev1.PodSpec{NodeName: "node"},
	}
}

// transformFor returns the transform the cache runs on obj: its type's Transform, or DefaultTransform.
func transformFor(opts cache.Options, obj client.Object) toolscache.TransformFunc {
	for k, v := range opts.ByObject {
		if reflect.TypeOf(k) == reflect.TypeOf(obj) && v.Transform != nil {
			return v.Transform
		}
	}
	return opts.DefaultTransform
}

func run(opts cache.Options, obj client.Object) client.Object {
	t := transformFor(opts, obj)
	if t == nil {
		return obj
	}
	out, err := t(obj)
	Expect(err).ToNot(HaveOccurred())
	return out.(client.Object)
}

var _ = Describe("CacheFields", func() {
	var opts cache.Options
	BeforeEach(func() {
		opts = cache.Options{}
	})

	It("should resolve every stripped field", func() {
		for _, f := range cachefields.Stripped {
			_, _, err := f.Resolve()
			Expect(err).ToNot(HaveOccurred(), f.Path)
		}
	})
	It("should strip managedFields from every type", func() {
		Expect(cachefields.SetTransforms(&opts)).To(Succeed())
		pod := run(opts, newPod()).(*corev1.Pod)
		Expect(pod.ManagedFields).To(BeNil())
		Expect(pod.Name).To(Equal("pod"))
		Expect(pod.Spec.NodeName).To(Equal("node"))

		meta := run(opts, &metav1.PartialObjectMetadata{ObjectMeta: metav1.ObjectMeta{ManagedFields: managedFields}}).(*metav1.PartialObjectMetadata)
		Expect(meta.ManagedFields).To(BeNil())
	})
	It("should strip both the node's own fields and the fields stripped from every type", func() {
		Expect(cachefields.SetTransforms(&opts)).To(Succeed())
		node := run(opts, newNode()).(*corev1.Node)
		Expect(node.ManagedFields).To(BeNil())
		Expect(node.Status.Images).To(BeNil())
		Expect(node.Labels).To(Equal(map[string]string{"a": "b"}))
		Expect(node.Status.NodeInfo.BootID).To(Equal("boot"))
	})
	It("should leave objects without the fields unchanged", func() {
		Expect(cachefields.SetTransforms(&opts)).To(Succeed())
		obj := &unstructured.Unstructured{}
		obj.SetManagedFields(managedFields)
		out, err := opts.DefaultTransform(obj)
		Expect(err).ToNot(HaveOccurred())
		Expect(out.(*unstructured.Unstructured).GetManagedFields()).To(Equal(managedFields))
	})
	It("should keep a field on every type", func() {
		Expect(cachefields.SetTransforms(&opts, cachefields.Field{Path: "ObjectMeta.ManagedFields"})).To(Succeed())
		Expect(run(opts, newPod()).(*corev1.Pod).ManagedFields).To(Equal(managedFields))
		node := run(opts, newNode()).(*corev1.Node)
		Expect(node.ManagedFields).To(Equal(managedFields))
		Expect(node.Status.Images).To(BeNil())
	})
	It("should keep a field on one type", func() {
		Expect(cachefields.SetTransforms(&opts, cachefields.Field{Object: &corev1.Pod{}, Path: "ObjectMeta.ManagedFields"})).To(Succeed())
		Expect(run(opts, newPod()).(*corev1.Pod).ManagedFields).To(Equal(managedFields))
		Expect(run(opts, newNode()).(*corev1.Node).ManagedFields).To(BeNil())
		Expect(run(opts, &corev1.Service{ObjectMeta: metav1.ObjectMeta{ManagedFields: managedFields}}).(*corev1.Service).ManagedFields).To(BeNil())
	})
	It("should keep every stripped field on a type", func() {
		Expect(cachefields.SetTransforms(&opts,
			cachefields.Field{Object: &corev1.Node{}, Path: "ObjectMeta.ManagedFields"},
			cachefields.Field{Object: &corev1.Node{}, Path: "Status.Images"},
		)).To(Succeed())
		// A nil Transform would fall back to DefaultTransform and strip managedFields.
		node := run(opts, newNode()).(*corev1.Node)
		Expect(node.ManagedFields).To(Equal(managedFields))
		Expect(node.Status.Images).ToNot(BeNil())
	})
	It("should keep existing cache options for a type", func() {
		selector := fields.SelectorFromSet(fields.Set{"metadata.name": "node"})
		lease := fields.SelectorFromSet(fields.Set{"metadata.namespace": "kube-node-lease"})
		opts.ByObject = map[client.Object]cache.ByObject{
			&corev1.Node{}:          {Field: selector},
			&coordinationv1.Lease{}: {Field: lease},
		}
		Expect(cachefields.SetTransforms(&opts)).To(Succeed())
		Expect(opts.ByObject).To(HaveLen(2))
		for k, v := range opts.ByObject {
			switch k.(type) {
			case *corev1.Node:
				Expect(v.Field).To(Equal(selector))
				Expect(v.Transform).ToNot(BeNil())
			case *coordinationv1.Lease:
				Expect(v.Field).To(Equal(lease))
				// Falls back to DefaultTransform.
				Expect(v.Transform).To(BeNil())
			}
		}
	})
	It("should fail when the default transform is already set", func() {
		opts.DefaultTransform = cache.TransformStripManagedFields()
		Expect(cachefields.SetTransforms(&opts)).ToNot(Succeed())
	})
	It("should fail when a type already has a transform", func() {
		opts.ByObject = map[client.Object]cache.ByObject{
			&corev1.Node{}: {Transform: cache.TransformStripManagedFields()},
		}
		Expect(cachefields.SetTransforms(&opts)).ToNot(Succeed())
	})
	DescribeTable("should reject fields that don't resolve",
		func(f cachefields.Field) {
			Expect(cachefields.SetTransforms(&opts, f)).ToNot(Succeed())
		},
		Entry("missing field", cachefields.Field{Object: &corev1.Node{}, Path: "Status.Imagez"}),
		Entry("promoted field", cachefields.Field{Object: &corev1.Node{}, Path: "ManagedFields"}),
		Entry("field under a non-struct", cachefields.Field{Object: &corev1.Pod{}, Path: "Spec.Affinity.NodeAffinity"}),
		Entry("field missing from every type", cachefields.Field{Path: "Status.Images"}),
		Entry("empty path", cachefields.Field{Object: &corev1.Node{}}),
	)
})
