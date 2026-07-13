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

package virtualpods

import (
	"strconv"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	autoscalingv1beta1 "sigs.k8s.io/karpenter/pkg/apis/autoscaling/v1beta1"
)

var _ = Describe("buildVirtualPods", func() {
	It("should create the correct number of pods with expected metadata", func() {
		cb := readyBuffer("web", 3)
		spec := corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app", Image: "pause:v1"}},
		}

		pods := BuildVirtualPods(cb, spec)
		Expect(pods).To(HaveLen(3))

		for i, p := range pods {
			idx := i + 1
			Expect(p.Name).To(Equal("capacity-buffer-web-" + strconv.Itoa(idx)))
			Expect(p.Namespace).To(Equal("default"))
			Expect(string(p.UID)).To(Equal("uid-web-" + strconv.Itoa(idx)))
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
		a := BuildVirtualPods(cb, spec)
		b := BuildVirtualPods(cb, spec)
		Expect(a).To(HaveLen(len(b)))
		for i := range a {
			Expect(a[i].UID).To(Equal(b[i].UID))
		}
	})

	It("should return nil for zero replicas", func() {
		cb := readyBuffer("web", 0)
		spec := corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}}
		Expect(BuildVirtualPods(cb, spec)).To(BeNil())
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

		pods := BuildVirtualPods(cb, spec)
		Expect(pods).To(HaveLen(2))

		for i, p := range pods {
			idx := i + 1
			Expect(p.Name).To(Equal("capacity-buffer-scalable-app-" + strconv.Itoa(idx)))
			Expect(p.Namespace).To(Equal("default"))
			Expect(p.Annotations[autoscalingv1beta1.FakePodAnnotationKey]).To(Equal("true"))
			Expect(p.Labels[autoscalingv1beta1.BufferNameLabel]).To(Equal("scalable-app"))
			Expect(p.Labels[autoscalingv1beta1.BufferNamespaceLabel]).To(Equal("default"))
			Expect(*p.Spec.Priority).To(Equal(autoscalingv1beta1.VirtualPodPriority))
		}
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
