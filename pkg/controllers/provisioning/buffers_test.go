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
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"sigs.k8s.io/karpenter/pkg/state/virtualpods"

	autoscalingv1beta1 "sigs.k8s.io/karpenter/pkg/apis/autoscaling/v1beta1"
	scheduler "sigs.k8s.io/karpenter/pkg/controllers/provisioning/scheduling"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/test"
)

var _ = Describe("bufferKeyOf", func() {
	It("should return empty string for nil pod", func() {
		Expect(bufferKeyOf(nil)).To(Equal(""))
	})

	It("should return empty string for non-virtual pod", func() {
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "real", Namespace: "default"}}
		Expect(bufferKeyOf(pod)).To(Equal(""))
	})

	It("should return empty string when namespace label is missing", func() {
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Name:      "virt",
			Namespace: "default",
			Annotations: map[string]string{
				autoscalingv1beta1.FakePodAnnotationKey: autoscalingv1beta1.FakePodAnnotationValue,
			},
			Labels: map[string]string{
				autoscalingv1beta1.BufferNameAnnotation: "my-buffer",
			},
		}}
		Expect(bufferKeyOf(pod)).To(Equal(""))
	})

	It("should return empty string when name label is missing", func() {
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Name:      "virt",
			Namespace: "default",
			Annotations: map[string]string{
				autoscalingv1beta1.FakePodAnnotationKey: autoscalingv1beta1.FakePodAnnotationValue,
			},
			Labels: map[string]string{
				autoscalingv1beta1.BufferNamespaceAnnotation: "default",
			},
		}}
		Expect(bufferKeyOf(pod)).To(Equal(""))
	})

	It("should return namespace/name for complete virtual pod", func() {
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Name:      "virt",
			Namespace: "default",
			Annotations: map[string]string{
				autoscalingv1beta1.FakePodAnnotationKey: autoscalingv1beta1.FakePodAnnotationValue,
			},
			Labels: map[string]string{
				autoscalingv1beta1.BufferNameAnnotation:      "my-buffer",
				autoscalingv1beta1.BufferNamespaceAnnotation: "default",
			},
		}}
		Expect(bufferKeyOf(pod)).To(Equal("default/my-buffer"))
	})
})

var _ = Describe("IsVirtualPod", func() {
	It("should return false for nil pod", func() {
		Expect(IsVirtualPod(nil)).To(BeFalse())
	})

	It("should return false for pod without annotations", func() {
		p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "real"}}
		Expect(IsVirtualPod(p)).To(BeFalse())
	})

	It("should return true for virtual pod", func() {
		p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{autoscalingv1beta1.FakePodAnnotationKey: "true"},
		}}
		Expect(IsVirtualPod(p)).To(BeTrue())
	})

	It("should return false for pod with other annotations only", func() {
		p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{"other": "value"},
		}}
		Expect(IsVirtualPod(p)).To(BeFalse())
	})
})

var _ = Describe("classifyBufferPods", func() {
	It("should bucket virtual pods by buffer namespace/name across existing and new nodes", func() {
		cbA := test.ReadyBuffer("a", 3)
		cbB := test.ReadyBuffer("b", 2)
		buffers := map[string]*autoscalingv1beta1.CapacityBuffer{"default/a": cbA, "default/b": cbB}
		spec := corev1.PodTemplateSpec{}

		aPods := virtualpods.BuildVirtualPods(cbA, spec)
		bPods := virtualpods.BuildVirtualPods(cbB, spec)

		results := scheduler.Results{
			ExistingNodes: []*scheduler.ExistingNode{{Pods: []*corev1.Pod{aPods[0], aPods[1]}}},
			NewNodeClaims: []*scheduler.NodeClaim{{Pods: []*corev1.Pod{aPods[2], bPods[0], bPods[1]}}},
			PodErrors:     map[*corev1.Pod]error{},
		}

		summary := classifyBufferPods(results, buffers)
		Expect(summary["default/a"].existing).To(Equal(2))
		Expect(summary["default/a"].requiresNewClaim).To(Equal(1))
		Expect(summary["default/b"].existing).To(Equal(0))
		Expect(summary["default/b"].requiresNewClaim).To(Equal(2))
		Expect(summary["default/a"].desiredReplicas).To(Equal(3))
	})

	It("should ignore real pods", func() {
		realPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "real", Namespace: "default"}}
		results := scheduler.Results{
			ExistingNodes: []*scheduler.ExistingNode{{Pods: []*corev1.Pod{realPod}}},
		}
		summary := classifyBufferPods(results, map[string]*autoscalingv1beta1.CapacityBuffer{})
		Expect(summary).To(BeEmpty())
	})

	It("should distinguish buffers with the same name in different namespaces", func() {
		cbA := test.ReadyBufferInNamespace("buffer", "ns-a", 2)
		cbB := test.ReadyBufferInNamespace("buffer", "ns-b", 3)
		buffers := map[string]*autoscalingv1beta1.CapacityBuffer{
			"ns-a/buffer": cbA,
			"ns-b/buffer": cbB,
		}
		spec := corev1.PodTemplateSpec{}

		aPods := virtualpods.BuildVirtualPods(cbA, spec)
		bPods := virtualpods.BuildVirtualPods(cbB, spec)

		results := scheduler.Results{
			ExistingNodes: []*scheduler.ExistingNode{{Pods: []*corev1.Pod{aPods[0], bPods[0], bPods[1]}}},
			NewNodeClaims: []*scheduler.NodeClaim{{Pods: []*corev1.Pod{aPods[1], bPods[2]}}},
			PodErrors:     map[*corev1.Pod]error{},
		}

		summary := classifyBufferPods(results, buffers)
		Expect(summary["ns-a/buffer"].existing).To(Equal(1))
		Expect(summary["ns-a/buffer"].requiresNewClaim).To(Equal(1))
		Expect(summary["ns-a/buffer"].desiredReplicas).To(Equal(2))
		Expect(summary["ns-b/buffer"].existing).To(Equal(2))
		Expect(summary["ns-b/buffer"].requiresNewClaim).To(Equal(1))
		Expect(summary["ns-b/buffer"].desiredReplicas).To(Equal(3))
	})
})

var _ = Describe("computeProvisioningCondition", func() {
	It("should return True/FitsExistingCapacity when all pods fit", func() {
		cb := test.ReadyBuffer("web", 3)
		cond := computeProvisioningCondition(cb, &bufferProvisioningStatus{existing: 3, desiredReplicas: 3})
		Expect(cond).ToNot(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		Expect(cond.Reason).To(Equal("FitsExistingCapacity"))
	})

	It("should return False/RequiresNewCapacity when new nodes are needed", func() {
		cb := test.ReadyBuffer("web", 3)
		cond := computeProvisioningCondition(cb, &bufferProvisioningStatus{existing: 1, requiresNewClaim: 2, desiredReplicas: 3})
		Expect(cond).ToNot(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal("RequiresNewCapacity"))
	})

	It("should return False/NotReadyForProvisioning when buffer is not ready", func() {
		notReady := test.ReadyBuffer("web", 3)
		notReady.Status.Conditions[0].Status = metav1.ConditionFalse
		cond := computeProvisioningCondition(notReady, nil)
		Expect(cond).ToNot(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal("NotReadyForProvisioning"))
	})

	It("should return False/BufferEmpty when replicas is zero", func() {
		empty := test.ReadyBuffer("web", 0)
		cond := computeProvisioningCondition(empty, nil)
		Expect(cond).ToNot(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal("BufferEmpty"))
	})

	It("should return nil when ready but no results observed", func() {
		cb := test.ReadyBuffer("web", 3)
		cond := computeProvisioningCondition(cb, nil)
		Expect(cond).To(BeNil())
	})
})

var _ = Describe("bufferPodCountsFromResults", func() {
	It("should count virtual pods per providerID on existing nodes", func() {
		cbA := test.ReadyBuffer("a", 3)
		cbB := test.ReadyBuffer("b", 2)
		spec := corev1.PodTemplateSpec{}

		aPods := virtualpods.BuildVirtualPods(cbA, spec)
		bPods := virtualpods.BuildVirtualPods(cbB, spec)
		realPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "real-pod", Namespace: "default"}}

		nodeA := makeExistingNode("provider-a")
		nodeA.Pods = []*corev1.Pod{aPods[0], aPods[1], realPod}

		nodeB := makeExistingNode("provider-b")
		nodeB.Pods = []*corev1.Pod{bPods[0], bPods[1]}

		results := scheduler.Results{
			ExistingNodes: []*scheduler.ExistingNode{nodeA, nodeB},
			NewNodeClaims: []*scheduler.NodeClaim{{Pods: []*corev1.Pod{aPods[2]}}},
			PodErrors:     map[*corev1.Pod]error{},
		}

		counts := bufferPodCountsFromResults(results)
		Expect(counts["provider-a"]).To(Equal(2))
		Expect(counts["provider-b"]).To(Equal(2))
	})

	It("should return empty map for empty results", func() {
		counts := bufferPodCountsFromResults(scheduler.Results{})
		Expect(counts).To(BeEmpty())
	})

	It("should return empty map when only real pods are present", func() {
		realPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "real", Namespace: "default"}}
		nodeA := makeExistingNode("provider-a")
		nodeA.Pods = []*corev1.Pod{realPod}

		results := scheduler.Results{
			ExistingNodes: []*scheduler.ExistingNode{nodeA},
		}
		counts := bufferPodCountsFromResults(results)
		Expect(counts).To(BeEmpty())
	})
})

var _ = Describe("computeProvisioningCondition with scalableRef buffer", func() {
	It("should return True/FitsExistingCapacity for scalableRef buffer when all pods fit", func() {
		cb := test.ReadyScalableRefBuffer("scalable", 2)
		cond := computeProvisioningCondition(cb, &bufferProvisioningStatus{existing: 2, desiredReplicas: 2})
		Expect(cond).ToNot(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		Expect(cond.Reason).To(Equal("FitsExistingCapacity"))
	})

	It("should return False/RequiresNewCapacity for scalableRef buffer when new nodes needed", func() {
		cb := test.ReadyScalableRefBuffer("scalable", 3)
		cond := computeProvisioningCondition(cb, &bufferProvisioningStatus{existing: 1, requiresNewClaim: 2, desiredReplicas: 3})
		Expect(cond).ToNot(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal("RequiresNewCapacity"))
	})
})

var _ = Describe("filterVirtualPodErrors", func() {
	It("should remove virtual pods from error map", func() {
		cb := test.ReadyBuffer("web", 2)
		virtualPods := virtualpods.BuildVirtualPods(cb, corev1.PodTemplateSpec{})
		realPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "real", Namespace: "default"}}

		input := map[*corev1.Pod]error{
			virtualPods[0]: fmt.Errorf("no capacity"),
			virtualPods[1]: fmt.Errorf("no capacity"),
			realPod:        fmt.Errorf("unschedulable"),
		}

		result := filterVirtualPodErrors(input)
		Expect(result).To(HaveLen(1))
		Expect(result[realPod]).To(HaveOccurred())
	})

	It("should return empty map when all pods are virtual", func() {
		cb := test.ReadyBuffer("web", 2)
		virtualPods := virtualpods.BuildVirtualPods(cb, corev1.PodTemplateSpec{})

		input := map[*corev1.Pod]error{
			virtualPods[0]: fmt.Errorf("err"),
			virtualPods[1]: fmt.Errorf("err"),
		}

		result := filterVirtualPodErrors(input)
		Expect(result).To(BeEmpty())
	})

	It("should return all pods when none are virtual", func() {
		podA := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "default"}}
		podB := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "b", Namespace: "default"}}

		input := map[*corev1.Pod]error{
			podA: fmt.Errorf("err"),
			podB: fmt.Errorf("err"),
		}

		result := filterVirtualPodErrors(input)
		Expect(result).To(HaveLen(2))
	})

	It("should handle empty input", func() {
		result := filterVirtualPodErrors(map[*corev1.Pod]error{})
		Expect(result).To(BeEmpty())
	})
})

var _ = Describe("filterVirtualPodMapping", func() {
	It("should remove virtual pods from pod slices", func() {
		cb := test.ReadyBuffer("web", 2)
		virtualPods := virtualpods.BuildVirtualPods(cb, corev1.PodTemplateSpec{})
		realPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "real", Namespace: "default"}}

		input := map[string][]*corev1.Pod{
			"nodepool-a": {virtualPods[0], realPod, virtualPods[1]},
		}

		result := filterVirtualPodMapping(input)
		Expect(result).To(HaveLen(1))
		Expect(result["nodepool-a"]).To(HaveLen(1))
		Expect(result["nodepool-a"][0].Name).To(Equal("real"))
	})

	It("should drop keys entirely when only virtual pods remain", func() {
		cb := test.ReadyBuffer("web", 2)
		virtualPods := virtualpods.BuildVirtualPods(cb, corev1.PodTemplateSpec{})
		realPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "real", Namespace: "default"}}

		input := map[string][]*corev1.Pod{
			"nodepool-a": {virtualPods[0], virtualPods[1]},
			"nodepool-b": {realPod},
		}

		result := filterVirtualPodMapping(input)
		Expect(result).To(HaveLen(1))
		Expect(result).To(HaveKey("nodepool-b"))
		Expect(result).ToNot(HaveKey("nodepool-a"))
	})

	It("should return empty map when all pods are virtual", func() {
		cb := test.ReadyBuffer("web", 2)
		virtualPods := virtualpods.BuildVirtualPods(cb, corev1.PodTemplateSpec{})

		input := map[string][]*corev1.Pod{
			"nodepool-a": {virtualPods[0], virtualPods[1]},
		}

		result := filterVirtualPodMapping(input)
		Expect(result).To(BeEmpty())
	})

	It("should handle empty input", func() {
		result := filterVirtualPodMapping(map[string][]*corev1.Pod{})
		Expect(result).To(BeEmpty())
	})
})

func makeExistingNode(providerID string) *scheduler.ExistingNode {
	sn := state.NewNode()
	sn.Node = &corev1.Node{
		Spec: corev1.NodeSpec{ProviderID: providerID},
	}
	return &scheduler.ExistingNode{StateNode: sn}
}
