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

package pod_test

import (
	"fmt"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clock "k8s.io/utils/clock/testing"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/test"
	"sigs.k8s.io/karpenter/pkg/utils/pod"
)

var (
	fakeClock *clock.FakeClock
	recorder  *test.EventRecorder
)

func TestScheduling(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Scheduling")
}

var _ = BeforeEach(func() {
	fakeClock = clock.NewFakeClock(time.Now())
	recorder = test.NewEventRecorder()
})

var _ = Describe("IsDoNotDisruptActive", func() {
	It("should return false when no annotation exists", func() {
		p := &corev1.Pod{
			Status: corev1.PodStatus{
				StartTime: &metav1.Time{Time: fakeClock.Now().Add(-10 * time.Minute)},
			},
		}
		Expect(pod.IsDoNotDisruptActive(p, fakeClock, nil)).To(BeFalse())
	})

	It("should return true for 'true' annotation", func() {
		p := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{v1.DoNotDisruptAnnotationKey: "true"},
			},
			Status: corev1.PodStatus{
				StartTime: &metav1.Time{Time: fakeClock.Now().Add(-10 * time.Minute)},
			},
		}
		Expect(pod.IsDoNotDisruptActive(p, fakeClock, nil)).To(BeTrue())
	})

	It("should return true when duration has not expired", func() {
		p := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{v1.DoNotDisruptAnnotationKey: "15m"},
			},
			Status: corev1.PodStatus{
				StartTime: &metav1.Time{Time: fakeClock.Now().Add(-10 * time.Minute)}, // Pod started 10 minutes ago
			},
		}
		Expect(pod.IsDoNotDisruptActive(p, fakeClock, nil)).To(BeTrue()) // 15m grace period not expired
	})

	It("should return false when duration has expired", func() {
		p := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{v1.DoNotDisruptAnnotationKey: "5m"},
			},
			Status: corev1.PodStatus{
				StartTime: &metav1.Time{Time: fakeClock.Now().Add(-10 * time.Minute)}, // Pod started 10 minutes ago
			},
		}
		Expect(pod.IsDoNotDisruptActive(p, fakeClock, nil)).To(BeFalse()) // 5m grace period expired
	})

	It("should fail safe when start time is nil", func() {
		p := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{v1.DoNotDisruptAnnotationKey: "5m"},
			},
			Status: corev1.PodStatus{
				StartTime: nil,
			},
		}
		Expect(pod.IsDoNotDisruptActive(p, fakeClock, nil)).To(BeTrue()) // Fail safe when start time unknown
	})

	It("should return false for invalid duration format", func() {
		p := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{v1.DoNotDisruptAnnotationKey: "invalid"},
			},
			Status: corev1.PodStatus{
				StartTime: &metav1.Time{Time: fakeClock.Now().Add(-10 * time.Minute)},
			},
		}
		Expect(pod.IsDoNotDisruptActive(p, fakeClock, nil)).To(BeFalse()) // Invalid duration treated as non-existent
	})

	It("should return false for zero duration", func() {
		p := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{v1.DoNotDisruptAnnotationKey: "0s"},
			},
			Status: corev1.PodStatus{
				StartTime: &metav1.Time{Time: fakeClock.Now().Add(-10 * time.Minute)},
			},
		}
		Expect(pod.IsDoNotDisruptActive(p, fakeClock, nil)).To(BeFalse()) // Zero duration treated as invalid
	})

	It("should return false for negative duration", func() {
		p := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{v1.DoNotDisruptAnnotationKey: "-5m"},
			},
			Status: corev1.PodStatus{
				StartTime: &metav1.Time{Time: fakeClock.Now().Add(-10 * time.Minute)},
			},
		}
		Expect(pod.IsDoNotDisruptActive(p, fakeClock, nil)).To(BeFalse()) // Negative duration treated as invalid
	})

	It("should emit event for invalid duration format", func() {
		p := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "test-pod",
				Namespace:   "default",
				Annotations: map[string]string{v1.DoNotDisruptAnnotationKey: "invalid-format"},
			},
			Status: corev1.PodStatus{
				StartTime: &metav1.Time{Time: fakeClock.Now().Add(-10 * time.Minute)},
			},
		}
		Expect(pod.IsDoNotDisruptActive(p, fakeClock, recorder)).To(BeFalse())
		Expect(recorder.DetectedEvent("Invalid karpenter.sh/do-not-disrupt annotation value \"invalid-format\", ignoring annotation. Use 'true' or a duration like '5m', '1h'")).To(BeTrue())
	})

	It("should emit event for zero duration", func() {
		p := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "test-pod",
				Namespace:   "default",
				Annotations: map[string]string{v1.DoNotDisruptAnnotationKey: "0s"},
			},
			Status: corev1.PodStatus{
				StartTime: &metav1.Time{Time: fakeClock.Now().Add(-10 * time.Minute)},
			},
		}
		Expect(pod.IsDoNotDisruptActive(p, fakeClock, recorder)).To(BeFalse())
		Expect(recorder.DetectedEvent("Invalid karpenter.sh/do-not-disrupt annotation value \"0s\", ignoring annotation. Use 'true' or a duration like '5m', '1h'")).To(BeTrue())
	})

	It("should emit event for negative duration", func() {
		p := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "test-pod",
				Namespace:   "default",
				Annotations: map[string]string{v1.DoNotDisruptAnnotationKey: "-5m"},
			},
			Status: corev1.PodStatus{
				StartTime: &metav1.Time{Time: fakeClock.Now().Add(-10 * time.Minute)},
			},
		}
		Expect(pod.IsDoNotDisruptActive(p, fakeClock, recorder)).To(BeFalse())
		Expect(recorder.DetectedEvent("Invalid karpenter.sh/do-not-disrupt annotation value \"-5m\", ignoring annotation. Use 'true' or a duration like '5m', '1h'")).To(BeTrue())
	})

	It("should emit event when duration-based protection is active", func() {
		startTime := fakeClock.Now().Add(-10 * time.Minute)
		p := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "test-pod",
				Namespace:   "default",
				Annotations: map[string]string{v1.DoNotDisruptAnnotationKey: "15m"},
			},
			Status: corev1.PodStatus{
				StartTime: &metav1.Time{Time: startTime},
			},
		}
		Expect(pod.IsDoNotDisruptActive(p, fakeClock, recorder)).To(BeTrue())
		// Calculate expected disruptable time
		expectedTime := startTime.Add(15 * time.Minute).Format(time.RFC3339)
		Expect(recorder.DetectedEvent(fmt.Sprintf("Pod will be disruptable at %s", expectedTime))).To(BeTrue())
	})

	It("should not emit disruptable-at event when duration has expired", func() {
		p := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "test-pod",
				Namespace:   "default",
				Annotations: map[string]string{v1.DoNotDisruptAnnotationKey: "5m"},
			},
			Status: corev1.PodStatus{
				StartTime: &metav1.Time{Time: fakeClock.Now().Add(-10 * time.Minute)},
			},
		}
		Expect(pod.IsDoNotDisruptActive(p, fakeClock, recorder)).To(BeFalse())
		// Should not emit the disruptable-at event since duration has expired
		Expect(recorder.DetectedEvent("Pod will be disruptable at")).To(BeFalse())
		// Should emit the grace period elapsed event instead
		Expect(recorder.DetectedEvent("The karpenter.sh/do-not-disrupt grace period has elapsed, pod is now disruptable")).To(BeTrue())
	})

	It("should not emit disruptable-at event for indefinite protection", func() {
		p := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "test-pod",
				Namespace:   "default",
				Annotations: map[string]string{v1.DoNotDisruptAnnotationKey: "true"},
			},
			Status: corev1.PodStatus{
				StartTime: &metav1.Time{Time: fakeClock.Now().Add(-10 * time.Minute)},
			},
		}
		Expect(pod.IsDoNotDisruptActive(p, fakeClock, recorder)).To(BeTrue())
		// Should not emit the disruptable-at event for indefinite protection
		Expect(recorder.DetectedEvent("Pod will be disruptable at")).To(BeFalse())
	})

	It("should emit grace period elapsed event when duration has expired", func() {
		p := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "test-pod",
				Namespace:   "default",
				Annotations: map[string]string{v1.DoNotDisruptAnnotationKey: "5m"},
			},
			Status: corev1.PodStatus{
				StartTime: &metav1.Time{Time: fakeClock.Now().Add(-10 * time.Minute)},
			},
		}
		Expect(pod.IsDoNotDisruptActive(p, fakeClock, recorder)).To(BeFalse())
		// Should emit the grace period elapsed event
		Expect(recorder.DetectedEvent("The karpenter.sh/do-not-disrupt grace period has elapsed, pod is now disruptable")).To(BeTrue())
	})
})

var _ = Describe("IsDisruptable", func() {
	It("should be disruptable for active pod without annotation", func() {
		p := &corev1.Pod{
			Status: corev1.PodStatus{
				Phase:     corev1.PodRunning,
				StartTime: &metav1.Time{Time: fakeClock.Now().Add(-10 * time.Minute)},
			},
		}
		Expect(pod.IsDisruptable(p, fakeClock, nil)).To(BeTrue())
	})

	It("should not be disruptable for active pod with 'true' annotation", func() {
		p := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{v1.DoNotDisruptAnnotationKey: "true"},
			},
			Status: corev1.PodStatus{
				Phase:     corev1.PodRunning,
				StartTime: &metav1.Time{Time: fakeClock.Now().Add(-10 * time.Minute)},
			},
		}
		Expect(pod.IsDisruptable(p, fakeClock, nil)).To(BeFalse())
	})

	It("should be disruptable for active pod with expired duration", func() {
		p := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{v1.DoNotDisruptAnnotationKey: "5m"},
			},
			Status: corev1.PodStatus{
				Phase:     corev1.PodRunning,
				StartTime: &metav1.Time{Time: fakeClock.Now().Add(-10 * time.Minute)}, // Started 10 minutes ago
			},
		}
		Expect(pod.IsDisruptable(p, fakeClock, nil)).To(BeTrue()) // Grace period expired
	})

	It("should not be disruptable for active pod with non-expired duration", func() {
		p := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{v1.DoNotDisruptAnnotationKey: "15m"},
			},
			Status: corev1.PodStatus{
				Phase:     corev1.PodRunning,
				StartTime: &metav1.Time{Time: fakeClock.Now().Add(-10 * time.Minute)}, // Started 10 minutes ago
			},
		}
		Expect(pod.IsDisruptable(p, fakeClock, nil)).To(BeFalse()) // Grace period not expired
	})

	It("should be disruptable for terminal pod even with annotation", func() {
		p := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{v1.DoNotDisruptAnnotationKey: "true"},
			},
			Status: corev1.PodStatus{
				Phase:     corev1.PodSucceeded,
				StartTime: &metav1.Time{Time: fakeClock.Now().Add(-10 * time.Minute)},
			},
		}
		Expect(pod.IsDisruptable(p, fakeClock, nil)).To(BeTrue()) // Not active
	})

	It("should emit event for invalid do-not-disrupt annotation format", func() {
		p := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "test-pod",
				Namespace:   "default",
				Annotations: map[string]string{v1.DoNotDisruptAnnotationKey: "invalid-format"},
			},
			Status: corev1.PodStatus{
				Phase:     corev1.PodRunning,
				StartTime: &metav1.Time{Time: fakeClock.Now().Add(-10 * time.Minute)},
			},
		}
		// Call IsDisruptable with the recorder
		Expect(pod.IsDisruptable(p, fakeClock, recorder)).To(BeTrue()) // Invalid annotation treated as non-existent, so disruptable
		// Verify the event was emitted
		Expect(recorder.DetectedEvent("Invalid karpenter.sh/do-not-disrupt annotation value \"invalid-format\", ignoring annotation. Use 'true' or a duration like '5m', '1h'")).To(BeTrue())
	})

	It("should emit event for zero duration do-not-disrupt annotation", func() {
		p := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "test-pod",
				Namespace:   "default",
				Annotations: map[string]string{v1.DoNotDisruptAnnotationKey: "0s"},
			},
			Status: corev1.PodStatus{
				Phase:     corev1.PodRunning,
				StartTime: &metav1.Time{Time: fakeClock.Now().Add(-10 * time.Minute)},
			},
		}
		Expect(pod.IsDisruptable(p, fakeClock, recorder)).To(BeTrue()) // Invalid annotation treated as non-existent, so disruptable
		Expect(recorder.DetectedEvent("Invalid karpenter.sh/do-not-disrupt annotation value \"0s\", ignoring annotation. Use 'true' or a duration like '5m', '1h'")).To(BeTrue())
	})

	It("should emit event for negative duration do-not-disrupt annotation", func() {
		p := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "test-pod",
				Namespace:   "default",
				Annotations: map[string]string{v1.DoNotDisruptAnnotationKey: "-5m"},
			},
			Status: corev1.PodStatus{
				Phase:     corev1.PodRunning,
				StartTime: &metav1.Time{Time: fakeClock.Now().Add(-10 * time.Minute)},
			},
		}
		Expect(pod.IsDisruptable(p, fakeClock, recorder)).To(BeTrue()) // Invalid annotation treated as non-existent, so disruptable
		Expect(recorder.DetectedEvent("Invalid karpenter.sh/do-not-disrupt annotation value \"-5m\", ignoring annotation. Use 'true' or a duration like '5m', '1h'")).To(BeTrue())
	})

	It("should not emit event for valid 'true' annotation", func() {
		p := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "test-pod",
				Namespace:   "default",
				Annotations: map[string]string{v1.DoNotDisruptAnnotationKey: "true"},
			},
			Status: corev1.PodStatus{
				Phase:     corev1.PodRunning,
				StartTime: &metav1.Time{Time: fakeClock.Now().Add(-10 * time.Minute)},
			},
		}
		Expect(pod.IsDisruptable(p, fakeClock, recorder)).To(BeFalse())
		// Should not emit any event for valid annotation
		Expect(recorder.DetectedEvent("Invalid karpenter.sh/do-not-disrupt annotation value")).To(BeFalse())
	})

	It("should not emit event for valid duration annotation", func() {
		p := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "test-pod",
				Namespace:   "default",
				Annotations: map[string]string{v1.DoNotDisruptAnnotationKey: "5m"},
			},
			Status: corev1.PodStatus{
				Phase:     corev1.PodRunning,
				StartTime: &metav1.Time{Time: fakeClock.Now().Add(-10 * time.Minute)},
			},
		}
		Expect(pod.IsDisruptable(p, fakeClock, recorder)).To(BeTrue()) // Duration expired
		// Should not emit any event for valid annotation
		Expect(recorder.DetectedEvent("Invalid karpenter.sh/do-not-disrupt annotation value")).To(BeFalse())
	})
})
