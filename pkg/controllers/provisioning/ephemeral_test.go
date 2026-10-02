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

// Pure-function tests for the ephemeral (one-shot) buffer latch logic; no client needed. The
// client-dependent updateEphemeralFulfillment path is covered by ephemeral_integration_test.go.

package provisioning

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	autoscalingv1beta1 "sigs.k8s.io/karpenter/pkg/apis/autoscaling/v1beta1"
)

// --- helpers ------------------------------------------------------------------------------

func rl(cpu, mem string) corev1.ResourceList {
	out := corev1.ResourceList{}
	if cpu != "" {
		out[corev1.ResourceCPU] = resource.MustParse(cpu)
	}
	if mem != "" {
		out[corev1.ResourceMemory] = resource.MustParse(mem)
	}
	return out
}

// boundPod builds a pod with the given namespace/labels, bound to a node, requesting cpu/mem.
//
//nolint:unparam // test helper kept general; some params are constant across current callers
func boundPod(ns string, lbls map[string]string, cpu, mem string) corev1.Pod {
	return podWith(ns, lbls, cpu, mem, "node-1", nil)
}

func podWith(ns string, lbls map[string]string, cpu, mem, nodeName string, gates []corev1.PodSchedulingGate) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Labels: lbls},
		Spec: corev1.PodSpec{
			NodeName:        nodeName,
			SchedulingGates: gates,
			Containers: []corev1.Container{{
				Name:      "c",
				Resources: corev1.ResourceRequirements{Requests: rl(cpu, mem)},
			}},
		},
	}
}

//nolint:unparam // test helper kept general; some params are constant across current callers
func ephemeralBuffer(ns, name string, replicas int32, anns map[string]string) *autoscalingv1beta1.CapacityBuffer {
	refill := autoscalingv1beta1.RefillStrategyNone
	r := replicas
	return &autoscalingv1beta1.CapacityBuffer{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Annotations: anns},
		Spec:       autoscalingv1beta1.CapacityBufferSpec{RefillStrategy: &refill},
		Status:     autoscalingv1beta1.CapacityBufferStatus{Replicas: &r},
	}
}

// --- consumedChunks -----------------------------------------------------------------------

var _ = Describe("consumedChunks", func() {
	// perChunk of 1 cpu; replicas cap 4.
	perCPU := corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")}

	It("is 0 when nothing is bound", func() {
		Expect(consumedChunks(corev1.ResourceList{}, perCPU, 4)).To(Equal(int32(0)))
	})

	It("counts whole chunks by capacity (2 cpu bound / 1 cpu per = 2)", func() {
		Expect(consumedChunks(rl("2", ""), perCPU, 4)).To(Equal(int32(2)))
	})

	It("floors partial chunks (2500m bound / 1 cpu per = 2)", func() {
		Expect(consumedChunks(rl("2500m", ""), perCPU, 4)).To(Equal(int32(2)))
	})

	It("is capped at replicas (10 cpu bound but only 4 chunks)", func() {
		Expect(consumedChunks(rl("10", ""), perCPU, 4)).To(Equal(int32(4)))
	})

	It("takes the min across dimensions (cpu ok, mem limits it)", func() {
		perChunk := rl("1", "2Gi")
		// 4 cpu -> 4 chunks by cpu, but only 4Gi -> 2 chunks by mem => min = 2
		Expect(consumedChunks(rl("4", "4Gi"), perChunk, 8)).To(Equal(int32(2)))
	})

	It("counts 0 when a required dimension is absent from bound (gpu)", func() {
		perChunk := corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("1")}
		Expect(consumedChunks(rl("100", "100Gi"), perChunk, 4)).To(Equal(int32(0)))
	})

	It("ignores zero-valued per-chunk dimensions", func() {
		perChunk := corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("0")}
		Expect(consumedChunks(rl("3", ""), perChunk, 4)).To(Equal(int32(3)))
	})
})

// --- boundMatchingCapacity ----------------------------------------------------------------

var _ = Describe("boundMatchingCapacity", func() {
	sel := labels.SelectorFromSet(labels.Set{"app": "trainer"})
	match := map[string]string{"app": "trainer"}
	other := map[string]string{"app": "other"}

	It("sums only bound, matching, ungated pods in the namespace", func() {
		pods := []corev1.Pod{
			boundPod("ns", match, "2", "4Gi"),         // counts
			boundPod("ns", match, "2", "4Gi"),         // counts
			podWith("ns", match, "2", "4Gi", "", nil), // unbound -> excluded
			podWith("ns", match, "2", "4Gi", "node-1", []corev1.PodSchedulingGate{{Name: "kueue.x-k8s.io/admission"}}), // gated -> excluded
			boundPod("ns", other, "2", "4Gi"),       // non-matching -> excluded
			boundPod("other-ns", match, "2", "4Gi"), // wrong namespace -> excluded
		}
		got := boundMatchingCapacity("ns", sel, pods, time.Time{})
		cpu := got[corev1.ResourceCPU]
		mem := got[corev1.ResourceMemory]
		wantCPU := resource.MustParse("4")
		wantMem := resource.MustParse("8Gi")
		// Compare numeric values, not Quantity structs: equal quantities can differ in cached
		// string/format fields, so gomega's Equal on the struct is brittle.
		Expect(cpu.MilliValue()).To(Equal(wantCPU.MilliValue()))
		Expect(mem.Value()).To(Equal(wantMem.Value()))
	})

	It("does not include the injected v1.ResourcePods count", func() {
		pods := []corev1.Pod{boundPod("ns", match, "2", "4Gi")}
		got := boundMatchingCapacity("ns", sel, pods, time.Time{})
		_, hasPods := got[corev1.ResourcePods]
		Expect(hasPods).To(BeFalse())
	})

	It("returns an empty-ish list when nothing matches", func() {
		pods := []corev1.Pod{boundPod("ns", other, "2", "4Gi")}
		got := boundMatchingCapacity("ns", sel, pods, time.Time{})
		cpu := got[corev1.ResourceCPU]
		Expect(cpu.IsZero()).To(BeTrue())
	})

	It("excludes a pod carrying the Kueue topology gate even if admission gate is gone", func() {
		pods := []corev1.Pod{
			podWith("ns", match, "2", "4Gi", "node-1", []corev1.PodSchedulingGate{{Name: "kueue.x-k8s.io/topology"}}),
		}
		got := boundMatchingCapacity("ns", sel, pods, time.Time{})
		cpu := got[corev1.ResourceCPU]
		Expect(cpu.IsZero()).To(BeTrue())
	})
})

// --- isEphemeral --------------------------------------------------------------------------

var _ = Describe("IsEphemeral", func() {
	It("is true when refillStrategy is none (one-shot)", func() {
		Expect(ephemeralBuffer("ns", "b", 1, nil).IsEphemeral()).To(BeTrue())
	})

	It("is false when refillStrategy is nil (defaulted recreate)", func() {
		cb := &autoscalingv1beta1.CapacityBuffer{}
		Expect(cb.IsEphemeral()).To(BeFalse())
	})

	It("is false for refillStrategy=recreate", func() {
		recreate := autoscalingv1beta1.RefillStrategyRecreate
		cb := &autoscalingv1beta1.CapacityBuffer{Spec: autoscalingv1beta1.CapacityBufferSpec{RefillStrategy: &recreate}}
		Expect(cb.IsEphemeral()).To(BeFalse())
	})
})

// --- RemainingReplicas --------------------------------------------------------------------

var _ = Describe("RemainingReplicas", func() {
	It("is replicas minus consumed for a one-shot buffer", func() {
		cb := ephemeralBuffer("ns", "b", 4, nil)
		cb.Status.ConsumedReplicas = lo.ToPtr(int32(3))
		Expect(cb.RemainingReplicas()).To(Equal(int32(1)))
	})

	It("never goes negative", func() {
		cb := ephemeralBuffer("ns", "b", 2, nil)
		cb.Status.ConsumedReplicas = lo.ToPtr(int32(5))
		Expect(cb.RemainingReplicas()).To(Equal(int32(0)))
	})

	It("ignores consumed for a recreate buffer", func() {
		recreate := autoscalingv1beta1.RefillStrategyRecreate
		cb := ephemeralBuffer("ns", "b", 4, nil)
		cb.Spec.RefillStrategy = &recreate
		cb.Status.ConsumedReplicas = lo.ToPtr(int32(3))
		Expect(cb.RemainingReplicas()).To(Equal(int32(4)))
	})

	It("is zero when status.replicas is unset", func() {
		cb := &autoscalingv1beta1.CapacityBuffer{}
		Expect(cb.RemainingReplicas()).To(Equal(int32(0)))
	})
})

// --- matchSelector ------------------------------------------------------------------------

var _ = Describe("matchSelector", func() {
	It("returns (nil,false,nil) when the annotation is absent", func() {
		sel, ok, err := matchSelector(ephemeralBuffer("ns", "b", 1, nil))
		Expect(err).ToNot(HaveOccurred())
		Expect(ok).To(BeFalse())
		Expect(sel).To(BeNil())
	})

	It("returns (nil,false,nil) when the annotation is empty", func() {
		cb := ephemeralBuffer("ns", "b", 1, map[string]string{autoscalingv1beta1.BufferMatchSelectorAnnotation: ""})
		_, ok, err := matchSelector(cb)
		Expect(err).ToNot(HaveOccurred())
		Expect(ok).To(BeFalse())
	})

	It("parses a kubectl-style selector", func() {
		cb := ephemeralBuffer("ns", "b", 1, map[string]string{autoscalingv1beta1.BufferMatchSelectorAnnotation: "app=trainer,role in (worker)"})
		sel, ok, err := matchSelector(cb)
		Expect(err).ToNot(HaveOccurred())
		Expect(ok).To(BeTrue())
		Expect(sel.Matches(labels.Set{"app": "trainer", "role": "worker"})).To(BeTrue())
		Expect(sel.Matches(labels.Set{"app": "trainer", "role": "ps"})).To(BeFalse())
	})

	It("returns an error for an invalid selector", func() {
		// "=bad" is genuinely unparseable (leading operator). Note "app==" is NOT invalid — it
		// parses as app equal to the empty string.
		cb := ephemeralBuffer("ns", "b", 1, map[string]string{autoscalingv1beta1.BufferMatchSelectorAnnotation: "=bad"})
		_, ok, err := matchSelector(cb)
		Expect(err).To(HaveOccurred())
		Expect(ok).To(BeFalse())
	})
})

// --- fillDeadline -------------------------------------------------------------------------

var _ = Describe("fillDeadline", func() {
	It("is absent when unset", func() {
		_, ok := fillDeadline(ephemeralBuffer("ns", "b", 1, nil))
		Expect(ok).To(BeFalse())
	})

	It("returns the spec.fillDeadline duration when set", func() {
		cb := ephemeralBuffer("ns", "b", 1, nil)
		cb.Spec.FillDeadlineSeconds = lo.ToPtr(int32(30 * 60))
		d, ok := fillDeadline(cb)
		Expect(ok).To(BeTrue())
		Expect(d).To(Equal(30 * time.Minute))
	})

	It("treats a non-positive value as unset", func() {
		cb := ephemeralBuffer("ns", "b", 1, nil)
		cb.Spec.FillDeadlineSeconds = lo.ToPtr(int32(0))
		_, ok := fillDeadline(cb)
		Expect(ok).To(BeFalse())
	})
})

// --- readyForProvisioningSince ------------------------------------------------------------

var _ = Describe("readyForProvisioningSince", func() {
	It("is absent when the ReadyForProvisioning condition is missing", func() {
		_, ok := readyForProvisioningSince(ephemeralBuffer("ns", "b", 1, nil))
		Expect(ok).To(BeFalse())
	})

	It("is absent when ReadyForProvisioning is False", func() {
		cb := ephemeralBuffer("ns", "b", 1, nil)
		cb.SetCondition(autoscalingv1beta1.ReadyForProvisioningCondition, metav1.ConditionFalse, "x", "x")
		_, ok := readyForProvisioningSince(cb)
		Expect(ok).To(BeFalse())
	})

	It("returns the transition time when ReadyForProvisioning is True", func() {
		cb := ephemeralBuffer("ns", "b", 1, nil)
		cb.SetCondition(autoscalingv1beta1.ReadyForProvisioningCondition, metav1.ConditionTrue, "ok", "ok")
		ts, ok := readyForProvisioningSince(cb)
		Expect(ok).To(BeTrue())
		Expect(ts.IsZero()).To(BeFalse())
	})
})
