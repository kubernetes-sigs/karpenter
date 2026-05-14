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
	"testing"

	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	autoscalingv1alpha1 "sigs.k8s.io/karpenter/pkg/apis/autoscaling/v1alpha1"
	scheduler "sigs.k8s.io/karpenter/pkg/controllers/provisioning/scheduling"
)

func readyBuffer(name string, replicas int32) *autoscalingv1alpha1.CapacityBuffer {
	cb := &autoscalingv1alpha1.CapacityBuffer{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			UID:       types.UID("uid-" + name),
		},
		Status: autoscalingv1alpha1.CapacityBufferStatus{
			Replicas: lo.ToPtr(replicas),
			PodTemplateRef: &autoscalingv1alpha1.LocalObjectRef{
				Name: name + "-template",
			},
			Conditions: []metav1.Condition{{
				Type:   autoscalingv1alpha1.ReadyForProvisioningCondition,
				Status: metav1.ConditionTrue,
			}},
		},
	}
	return cb
}

func TestBuildVirtualPods(t *testing.T) {
	cb := readyBuffer("web", 3)
	spec := corev1.PodSpec{
		Containers: []corev1.Container{{Name: "app", Image: "pause:v1"}},
	}

	pods := buildVirtualPods(cb, spec)
	if len(pods) != 3 {
		t.Fatalf("expected 3 pods, got %d", len(pods))
	}
	for i, p := range pods {
		if p.Name != "capacity-buffer-web-"+itoa(i+1) {
			t.Errorf("pod[%d] name = %q, want capacity-buffer-web-%d", i, p.Name, i+1)
		}
		if p.Namespace != "default" {
			t.Errorf("pod[%d] namespace = %q, want default", i, p.Namespace)
		}
		if string(p.UID) != "uid-web-"+itoa(i+1) {
			t.Errorf("pod[%d] UID = %q, want uid-web-%d", i, p.UID, i+1)
		}
		if p.Annotations[autoscalingv1alpha1.FakePodAnnotationKey] != "true" {
			t.Errorf("pod[%d] missing fake-pod annotation", i)
		}
		if p.Labels[autoscalingv1alpha1.BufferNameLabel] != "web" {
			t.Errorf("pod[%d] missing buffer-name label", i)
		}
		if p.Spec.Priority == nil || *p.Spec.Priority != autoscalingv1alpha1.VirtualPodPriority {
			t.Errorf("pod[%d] priority = %v, want %d", i, p.Spec.Priority, autoscalingv1alpha1.VirtualPodPriority)
		}
		if p.Spec.NodeName != "" {
			t.Errorf("pod[%d] nodeName should be empty, got %q", i, p.Spec.NodeName)
		}
		var found bool
		for _, c := range p.Status.Conditions {
			if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse && c.Reason == corev1.PodReasonUnschedulable {
				found = true
			}
		}
		if !found {
			t.Errorf("pod[%d] missing PodScheduled=False/Unschedulable condition", i)
		}
	}
}

func TestBuildVirtualPodsDeterministicUID(t *testing.T) {
	cb := readyBuffer("web", 2)
	spec := corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}}
	a := buildVirtualPods(cb, spec)
	b := buildVirtualPods(cb, spec)
	if len(a) != len(b) {
		t.Fatalf("mismatched lengths: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i].UID != b[i].UID {
			t.Errorf("UID[%d] not deterministic: %q vs %q", i, a[i].UID, b[i].UID)
		}
	}
}

func TestBuildVirtualPodsZeroReplicas(t *testing.T) {
	cb := readyBuffer("web", 0)
	spec := corev1.PodSpec{}
	if pods := buildVirtualPods(cb, spec); len(pods) != 0 {
		t.Errorf("expected 0 pods for zero replicas, got %d", len(pods))
	}
	cb.Status.Replicas = nil
	if pods := buildVirtualPods(cb, spec); len(pods) != 0 {
		t.Errorf("expected 0 pods for nil replicas, got %d", len(pods))
	}
}

func TestSanitizeVirtualPodSpecDropsPVCs(t *testing.T) {
	spec := corev1.PodSpec{
		NodeName: "should-be-cleared",
		Volumes: []corev1.Volume{
			{Name: "config", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{}}},
			{Name: "data", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "pvc1"}}},
		},
		Containers: []corev1.Container{{
			Name: "app",
			VolumeMounts: []corev1.VolumeMount{
				{Name: "config", MountPath: "/config"},
				{Name: "data", MountPath: "/data"},
			},
		}},
	}
	out := sanitizeVirtualPodSpec(spec)
	if out.NodeName != "" {
		t.Errorf("NodeName should be cleared")
	}
	if len(out.Volumes) != 1 || out.Volumes[0].Name != "config" {
		t.Errorf("PVC volume should be dropped, got %+v", out.Volumes)
	}
	if len(out.Containers[0].VolumeMounts) != 1 || out.Containers[0].VolumeMounts[0].Name != "config" {
		t.Errorf("PVC volumeMount should be dropped, got %+v", out.Containers[0].VolumeMounts)
	}
}

func TestIsVirtualPod(t *testing.T) {
	cases := []struct {
		name string
		pod  *corev1.Pod
		want bool
	}{
		{"nil pod", nil, false},
		{"no annotations", &corev1.Pod{}, false},
		{
			"virtual pod",
			&corev1.Pod{ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{autoscalingv1alpha1.FakePodAnnotationKey: "true"},
			}},
			true,
		},
		{
			"other annotations only",
			&corev1.Pod{ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{"something-else": "value"},
			}},
			false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsVirtualPod(tc.pod); got != tc.want {
				t.Errorf("IsVirtualPod(%+v) = %v, want %v", tc.pod, got, tc.want)
			}
		})
	}
}

func TestClassifyBufferPods(t *testing.T) {
	cbA := readyBuffer("a", 3)
	cbB := readyBuffer("b", 2)
	buffers := map[string]*autoscalingv1alpha1.CapacityBuffer{
		"a": cbA, "b": cbB,
	}
	spec := corev1.PodSpec{}

	// Build 3 virtual pods for A and 2 for B. Put 2 of A on existing capacity,
	// 1 of A on a new NodeClaim. All 2 of B on new NodeClaim. No failures.
	aPods := buildVirtualPods(cbA, spec)
	bPods := buildVirtualPods(cbB, spec)

	results := scheduler.Results{
		ExistingNodes: []*scheduler.ExistingNode{{Pods: []*corev1.Pod{aPods[0], aPods[1]}}},
		NewNodeClaims: []*scheduler.NodeClaim{{Pods: []*corev1.Pod{aPods[2], bPods[0], bPods[1]}}},
		PodErrors:     map[*corev1.Pod]error{},
	}

	summary := classifyBufferPods(results, buffers)
	if summary["a"].existing != 2 {
		t.Errorf("buffer a existing = %d, want 2", summary["a"].existing)
	}
	if summary["a"].requiresNewClaim != 1 {
		t.Errorf("buffer a requiresNewClaim = %d, want 1", summary["a"].requiresNewClaim)
	}
	if summary["b"].existing != 0 {
		t.Errorf("buffer b existing = %d, want 0", summary["b"].existing)
	}
	if summary["b"].requiresNewClaim != 2 {
		t.Errorf("buffer b requiresNewClaim = %d, want 2", summary["b"].requiresNewClaim)
	}
	if summary["a"].desiredReplicas != 3 {
		t.Errorf("buffer a desiredReplicas = %d, want 3", summary["a"].desiredReplicas)
	}
}

func TestClassifyBufferPodsIgnoresRealPods(t *testing.T) {
	realPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "real", Namespace: "default"}}
	results := scheduler.Results{
		ExistingNodes: []*scheduler.ExistingNode{{Pods: []*corev1.Pod{realPod}}},
	}
	summary := classifyBufferPods(results, map[string]*autoscalingv1alpha1.CapacityBuffer{})
	if len(summary) != 0 {
		t.Errorf("real pods should be ignored, got %d buckets", len(summary))
	}
}

func TestComputeProvisioningCondition(t *testing.T) {
	cb := readyBuffer("web", 3)

	t.Run("fits existing capacity", func(t *testing.T) {
		cond := computeProvisioningCondition(cb, &bufferProvisioningStatus{existing: 3, desiredReplicas: 3})
		if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != "FitsExistingCapacity" {
			t.Errorf("got %+v, want Provisioning=True/FitsExistingCapacity", cond)
		}
	})

	t.Run("requires new capacity", func(t *testing.T) {
		cond := computeProvisioningCondition(cb, &bufferProvisioningStatus{existing: 1, requiresNewClaim: 2, desiredReplicas: 3})
		if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != "RequiresNewCapacity" {
			t.Errorf("got %+v, want Provisioning=False/RequiresNewCapacity", cond)
		}
	})

	t.Run("not ready for provisioning", func(t *testing.T) {
		notReady := readyBuffer("web", 3)
		notReady.Status.Conditions[0].Status = metav1.ConditionFalse
		cond := computeProvisioningCondition(notReady, nil)
		if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != "NotReadyForProvisioning" {
			t.Errorf("got %+v, want Provisioning=False/NotReadyForProvisioning", cond)
		}
	})

	t.Run("buffer empty", func(t *testing.T) {
		empty := readyBuffer("web", 0)
		cond := computeProvisioningCondition(empty, nil)
		if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != "BufferEmpty" {
			t.Errorf("got %+v, want Provisioning=False/BufferEmpty", cond)
		}
	})

	t.Run("ready but no results", func(t *testing.T) {
		cond := computeProvisioningCondition(cb, nil)
		if cond != nil {
			t.Errorf("got %+v, want nil (leave condition unchanged)", cond)
		}
	})
}

// itoa is a tiny local integer-to-string used to avoid importing strconv.
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
