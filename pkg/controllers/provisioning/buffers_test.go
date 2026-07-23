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
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	autoscalingv1beta1 "sigs.k8s.io/karpenter/pkg/apis/autoscaling/v1beta1"
	scheduler "sigs.k8s.io/karpenter/pkg/controllers/provisioning/scheduling"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
)

var _ = Describe("buildVirtualPods", func() {
	It("should create the correct number of pods with expected metadata", func() {
		cb := readyBuffer("web", 3)
		spec := corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app", Image: "pause:v1"}},
		}

		pods := buildVirtualPods(cb, spec)
		Expect(pods).To(HaveLen(3))

		for i, p := range pods {
			idx := i + 1
			Expect(p.Name).To(Equal("capacity-buffer-web-" + itoa(idx)))
			Expect(p.Namespace).To(Equal("default"))
			Expect(string(p.UID)).To(Equal("uid-web-" + itoa(idx)))
			Expect(p.Annotations[autoscalingv1beta1.FakePodAnnotationKey]).To(Equal("true"))
			Expect(p.Labels[autoscalingv1beta1.BufferNameLabel]).To(Equal("web"))
			Expect(p.Labels[autoscalingv1beta1.BufferNamespaceLabel]).To(Equal("default"))
			Expect(p.Spec.Priority).ToNot(BeNil())
			Expect(*p.Spec.Priority).To(Equal(autoscalingv1beta1.VirtualPodPriority))
			Expect(p.Spec.NodeName).To(BeEmpty())

			hasUnschedulable := false
			for _, c := range p.Status.Conditions {
				if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse && c.Reason == corev1.PodReasonUnschedulable {
					hasUnschedulable = true
				}
			}
			Expect(hasUnschedulable).To(BeTrue(), "pod[%d] missing PodScheduled=False/Unschedulable condition", i)
		}
	})

	It("should produce deterministic UIDs across calls", func() {
		cb := readyBuffer("web", 2)
		spec := corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}}
		a := buildVirtualPods(cb, spec)
		b := buildVirtualPods(cb, spec)
		Expect(a).To(HaveLen(len(b)))
		for i := range a {
			Expect(a[i].UID).To(Equal(b[i].UID))
		}
	})

	It("should return nil for zero replicas", func() {
		cb := readyBuffer("web", 0)
		spec := corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}}
		Expect(buildVirtualPods(cb, spec)).To(BeNil())
	})
})

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
				autoscalingv1beta1.BufferNameLabel: "my-buffer",
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
				autoscalingv1beta1.BufferNamespaceLabel: "default",
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
				autoscalingv1beta1.BufferNameLabel:      "my-buffer",
				autoscalingv1beta1.BufferNamespaceLabel: "default",
			},
		}}
		Expect(bufferKeyOf(pod)).To(Equal("default/my-buffer"))
	})
})

var _ = Describe("sanitizeVirtualPodSpec", func() {
	It("should drop PVC volumes and their mounts", func() {
		spec := corev1.PodSpec{
			Containers: []corev1.Container{{
				Name: "app",
				VolumeMounts: []corev1.VolumeMount{
					{Name: "data", MountPath: "/data"},
					{Name: "config", MountPath: "/config"},
				},
			}},
			InitContainers: []corev1.Container{{
				Name: "init",
				VolumeMounts: []corev1.VolumeMount{
					{Name: "data", MountPath: "/init-data"},
				},
			}},
			Volumes: []corev1.Volume{
				{Name: "data", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "my-pvc"}}},
				{Name: "config", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: "cfg"}}}},
			},
		}

		result := sanitizeVirtualPodSpec(spec)

		Expect(result.Volumes).To(HaveLen(1))
		Expect(result.Volumes[0].Name).To(Equal("config"))
		Expect(result.Containers[0].VolumeMounts).To(HaveLen(1))
		Expect(result.Containers[0].VolumeMounts[0].Name).To(Equal("config"))
		Expect(result.InitContainers[0].VolumeMounts).To(BeEmpty())
	})

	It("should drop ephemeral volumes and their mounts", func() {
		spec := corev1.PodSpec{
			Containers: []corev1.Container{{
				Name: "app",
				VolumeMounts: []corev1.VolumeMount{
					{Name: "scratch", MountPath: "/scratch"},
					{Name: "config", MountPath: "/config"},
				},
			}},
			Volumes: []corev1.Volume{
				{Name: "scratch", VolumeSource: corev1.VolumeSource{Ephemeral: &corev1.EphemeralVolumeSource{
					VolumeClaimTemplate: &corev1.PersistentVolumeClaimTemplate{
						Spec: corev1.PersistentVolumeClaimSpec{},
					},
				}}},
				{Name: "config", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: "cfg"}}}},
			},
		}

		result := sanitizeVirtualPodSpec(spec)

		Expect(result.Volumes).To(HaveLen(1))
		Expect(result.Volumes[0].Name).To(Equal("config"))
		Expect(result.Containers[0].VolumeMounts).To(HaveLen(1))
		Expect(result.Containers[0].VolumeMounts[0].Name).To(Equal("config"))
	})

	It("should drop both PVC and ephemeral volumes together", func() {
		spec := corev1.PodSpec{
			Containers: []corev1.Container{{
				Name: "app",
				VolumeMounts: []corev1.VolumeMount{
					{Name: "pvc-vol", MountPath: "/data"},
					{Name: "eph-vol", MountPath: "/scratch"},
					{Name: "secret-vol", MountPath: "/secret"},
				},
			}},
			Volumes: []corev1.Volume{
				{Name: "pvc-vol", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "my-pvc"}}},
				{Name: "eph-vol", VolumeSource: corev1.VolumeSource{Ephemeral: &corev1.EphemeralVolumeSource{
					VolumeClaimTemplate: &corev1.PersistentVolumeClaimTemplate{},
				}}},
				{Name: "secret-vol", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "s"}}},
			},
		}

		result := sanitizeVirtualPodSpec(spec)

		Expect(result.Volumes).To(HaveLen(1))
		Expect(result.Volumes[0].Name).To(Equal("secret-vol"))
		Expect(result.Containers[0].VolumeMounts).To(HaveLen(1))
		Expect(result.Containers[0].VolumeMounts[0].Name).To(Equal("secret-vol"))
	})

	It("should preserve tolerations, nodeSelector, and affinity", func() {
		spec := corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app", Image: "pause:latest"}},
			Tolerations: []corev1.Toleration{{
				Key:      "dedicated",
				Operator: corev1.TolerationOpEqual,
				Value:    "buffer",
				Effect:   corev1.TaintEffectNoSchedule,
			}},
			NodeSelector: map[string]string{
				"node-type": "buffer",
			},
			Affinity: &corev1.Affinity{
				PodAffinity: &corev1.PodAffinity{
					RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
						LabelSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{"app": "web"},
						},
						TopologyKey: "kubernetes.io/hostname",
					}},
				},
			},
		}

		result := sanitizeVirtualPodSpec(spec)

		Expect(result.Tolerations).To(HaveLen(1))
		Expect(result.Tolerations[0].Key).To(Equal("dedicated"))
		Expect(result.Tolerations[0].Value).To(Equal("buffer"))
		Expect(result.NodeSelector).To(HaveKeyWithValue("node-type", "buffer"))
		Expect(result.Affinity).ToNot(BeNil())
		Expect(result.Affinity.PodAffinity).ToNot(BeNil())
		Expect(result.Affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution).To(HaveLen(1))
		Expect(result.Affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution[0].TopologyKey).To(Equal("kubernetes.io/hostname"))
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
		cbA := readyBuffer("a", 3)
		cbB := readyBuffer("b", 2)
		buffers := map[string]*autoscalingv1beta1.CapacityBuffer{"default/a": cbA, "default/b": cbB}
		spec := corev1.PodSpec{}

		aPods := buildVirtualPods(cbA, spec)
		bPods := buildVirtualPods(cbB, spec)

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
		cbA := readyBufferInNamespace("buffer", "ns-a", 2)
		cbB := readyBufferInNamespace("buffer", "ns-b", 3)
		buffers := map[string]*autoscalingv1beta1.CapacityBuffer{
			"ns-a/buffer": cbA,
			"ns-b/buffer": cbB,
		}
		spec := corev1.PodSpec{}

		aPods := buildVirtualPods(cbA, spec)
		bPods := buildVirtualPods(cbB, spec)

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
		cb := readyBuffer("web", 3)
		cond := computeProvisioningCondition(cb, &bufferProvisioningStatus{existing: 3, desiredReplicas: 3})
		Expect(cond).ToNot(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		Expect(cond.Reason).To(Equal("FitsExistingCapacity"))
	})

	It("should return False/RequiresNewCapacity when new nodes are needed", func() {
		cb := readyBuffer("web", 3)
		cond := computeProvisioningCondition(cb, &bufferProvisioningStatus{existing: 1, requiresNewClaim: 2, desiredReplicas: 3})
		Expect(cond).ToNot(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal("RequiresNewCapacity"))
	})

	It("should return False/NotReadyForProvisioning when buffer is not ready", func() {
		notReady := readyBuffer("web", 3)
		notReady.Status.Conditions[0].Status = metav1.ConditionFalse
		cond := computeProvisioningCondition(notReady, nil)
		Expect(cond).ToNot(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal("NotReadyForProvisioning"))
	})

	It("should return False/BufferEmpty when replicas is zero", func() {
		empty := readyBuffer("web", 0)
		cond := computeProvisioningCondition(empty, nil)
		Expect(cond).ToNot(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal("BufferEmpty"))
	})

	It("should return nil when ready but no results observed", func() {
		cb := readyBuffer("web", 3)
		cond := computeProvisioningCondition(cb, nil)
		Expect(cond).To(BeNil())
	})
})

var _ = Describe("bufferPodCountsFromResults", func() {
	It("should count virtual pods per providerID on existing nodes", func() {
		cbA := readyBuffer("a", 3)
		cbB := readyBuffer("b", 2)
		spec := corev1.PodSpec{}

		aPods := buildVirtualPods(cbA, spec)
		bPods := buildVirtualPods(cbB, spec)
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

var _ = Describe("listBuffersReadyForProvisioning", func() {
	It("should include scalableRef buffers without Status.PodTemplateRef", func() {
		cb := readyScalableRefBuffer("scalable", 3)
		Expect(cb.Status.PodTemplateRef).To(BeNil())

		buffers := filterReadyBuffers([]*autoscalingv1beta1.CapacityBuffer{cb})
		Expect(buffers).To(HaveLen(1))
		Expect(buffers[0].Name).To(Equal("scalable"))
	})

	It("should exclude buffers with neither PodTemplateRef nor ScalableRef", func() {
		cb := &autoscalingv1beta1.CapacityBuffer{
			ObjectMeta: metav1.ObjectMeta{Name: "orphan", Namespace: "default"},
			Spec:       autoscalingv1beta1.CapacityBufferSpec{Replicas: lo.ToPtr(int32(2))},
			Status: autoscalingv1beta1.CapacityBufferStatus{
				Replicas: lo.ToPtr(int32(2)),
				Conditions: []metav1.Condition{{
					Type:   autoscalingv1beta1.ReadyForProvisioningCondition,
					Status: metav1.ConditionTrue,
					Reason: "Resolved",
				}},
			},
		}
		buffers := filterReadyBuffers([]*autoscalingv1beta1.CapacityBuffer{cb})
		Expect(buffers).To(BeEmpty())
	})

	It("should exclude buffers with zero replicas", func() {
		cb := readyScalableRefBuffer("zero", 0)
		buffers := filterReadyBuffers([]*autoscalingv1beta1.CapacityBuffer{cb})
		Expect(buffers).To(BeEmpty())
	})

	It("should exclude buffers without ReadyForProvisioning condition", func() {
		cb := readyScalableRefBuffer("notready", 3)
		cb.Status.Conditions[0].Status = metav1.ConditionFalse
		buffers := filterReadyBuffers([]*autoscalingv1beta1.CapacityBuffer{cb})
		Expect(buffers).To(BeEmpty())
	})

	It("should include podTemplateRef buffers with Status.PodTemplateRef set", func() {
		cb := readyBuffer("ptref", 2)
		buffers := filterReadyBuffers([]*autoscalingv1beta1.CapacityBuffer{cb})
		Expect(buffers).To(HaveLen(1))
	})
})

var _ = Describe("buildVirtualPods with scalableRef buffer", func() {
	It("should create pods with expected metadata for scalableRef buffers", func() {
		cb := readyScalableRefBuffer("scalable-app", 2)
		spec := corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app", Image: "nginx:latest"}},
		}

		pods := buildVirtualPods(cb, spec)
		Expect(pods).To(HaveLen(2))

		for i, p := range pods {
			idx := i + 1
			Expect(p.Name).To(Equal("capacity-buffer-scalable-app-" + itoa(idx)))
			Expect(p.Namespace).To(Equal("default"))
			Expect(p.Annotations[autoscalingv1beta1.FakePodAnnotationKey]).To(Equal("true"))
			Expect(p.Labels[autoscalingv1beta1.BufferNameLabel]).To(Equal("scalable-app"))
			Expect(p.Labels[autoscalingv1beta1.BufferNamespaceLabel]).To(Equal("default"))
			Expect(*p.Spec.Priority).To(Equal(autoscalingv1beta1.VirtualPodPriority))
		}
	})
})

var _ = Describe("computeProvisioningCondition with scalableRef buffer", func() {
	It("should return True/FitsExistingCapacity for scalableRef buffer when all pods fit", func() {
		cb := readyScalableRefBuffer("scalable", 2)
		cond := computeProvisioningCondition(cb, &bufferProvisioningStatus{existing: 2, desiredReplicas: 2})
		Expect(cond).ToNot(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		Expect(cond.Reason).To(Equal("FitsExistingCapacity"))
	})

	It("should return False/RequiresNewCapacity for scalableRef buffer when new nodes needed", func() {
		cb := readyScalableRefBuffer("scalable", 3)
		cond := computeProvisioningCondition(cb, &bufferProvisioningStatus{existing: 1, requiresNewClaim: 2, desiredReplicas: 3})
		Expect(cond).ToNot(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal("RequiresNewCapacity"))
	})
})

var _ = Describe("filterVirtualPodErrors", func() {
	It("should remove virtual pods from error map", func() {
		cb := readyBuffer("web", 2)
		virtualPods := buildVirtualPods(cb, corev1.PodSpec{})
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
		cb := readyBuffer("web", 2)
		virtualPods := buildVirtualPods(cb, corev1.PodSpec{})

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
		cb := readyBuffer("web", 2)
		virtualPods := buildVirtualPods(cb, corev1.PodSpec{})
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
		cb := readyBuffer("web", 2)
		virtualPods := buildVirtualPods(cb, corev1.PodSpec{})
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
		cb := readyBuffer("web", 2)
		virtualPods := buildVirtualPods(cb, corev1.PodSpec{})

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

// filterReadyBuffers mirrors the filtering logic of listBuffersReadyForProvisioning
// without needing a real kube client.
func filterReadyBuffers(items []*autoscalingv1beta1.CapacityBuffer) []*autoscalingv1beta1.CapacityBuffer {
	var out []*autoscalingv1beta1.CapacityBuffer
	for _, cb := range items {
		if !apimeta.IsStatusConditionTrue(cb.Status.Conditions, autoscalingv1beta1.ReadyForProvisioningCondition) {
			continue
		}
		if cb.Status.Replicas == nil || *cb.Status.Replicas <= 0 {
			continue
		}
		if cb.Status.PodTemplateRef == nil && cb.Spec.ScalableRef == nil {
			continue
		}
		out = append(out, cb)
	}
	return out
}

func readyScalableRefBuffer(name string, replicas int32) *autoscalingv1beta1.CapacityBuffer {
	return &autoscalingv1beta1.CapacityBuffer{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			UID:       types.UID("uid-" + name),
		},
		Spec: autoscalingv1beta1.CapacityBufferSpec{
			ScalableRef: &autoscalingv1beta1.ScalableRef{
				APIGroup: "apps",
				Kind:     "Deployment",
				Name:     name + "-deploy",
			},
			Percentage: lo.ToPtr(int32(20)),
		},
		Status: autoscalingv1beta1.CapacityBufferStatus{
			Replicas: lo.ToPtr(replicas),
			Conditions: []metav1.Condition{{
				Type:   autoscalingv1beta1.ReadyForProvisioningCondition,
				Status: metav1.ConditionTrue,
				Reason: "Resolved",
			}},
		},
	}
}

func makeExistingNode(providerID string) *scheduler.ExistingNode {
	sn := state.NewNode()
	sn.Node = &corev1.Node{
		Spec: corev1.NodeSpec{ProviderID: providerID},
	}
	return &scheduler.ExistingNode{StateNode: sn}
}

func readyBuffer(name string, replicas int32) *autoscalingv1beta1.CapacityBuffer {
	return &autoscalingv1beta1.CapacityBuffer{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			UID:       types.UID("uid-" + name),
		},
		Spec: autoscalingv1beta1.CapacityBufferSpec{
			PodTemplateRef: &autoscalingv1beta1.LocalObjectRef{Name: name + "-template"},
			Replicas:       lo.ToPtr(replicas),
		},
		Status: autoscalingv1beta1.CapacityBufferStatus{
			Replicas:       lo.ToPtr(replicas),
			PodTemplateRef: &autoscalingv1beta1.LocalObjectRef{Name: name + "-template"},
			Conditions: []metav1.Condition{{
				Type:   autoscalingv1beta1.ReadyForProvisioningCondition,
				Status: metav1.ConditionTrue,
				Reason: "Resolved",
			}},
		},
	}
}

func readyBufferInNamespace(name, namespace string, replicas int32) *autoscalingv1beta1.CapacityBuffer {
	return &autoscalingv1beta1.CapacityBuffer{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			UID:       types.UID("uid-" + namespace + "-" + name),
		},
		Spec: autoscalingv1beta1.CapacityBufferSpec{
			PodTemplateRef: &autoscalingv1beta1.LocalObjectRef{Name: name + "-template"},
			Replicas:       lo.ToPtr(replicas),
		},
		Status: autoscalingv1beta1.CapacityBufferStatus{
			Replicas:       lo.ToPtr(replicas),
			PodTemplateRef: &autoscalingv1beta1.LocalObjectRef{Name: name + "-template"},
			Conditions: []metav1.Condition{{
				Type:   autoscalingv1beta1.ReadyForProvisioningCondition,
				Status: metav1.ConditionTrue,
				Reason: "Resolved",
			}},
		},
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var buf [20]byte
	n := len(buf)
	for i > 0 {
		n--
		buf[n] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		n--
		buf[n] = '-'
	}
	return string(buf[n:])
}
