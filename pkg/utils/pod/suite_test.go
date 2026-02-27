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
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clock "k8s.io/utils/clock/testing"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/utils/pod"
)

var fakeClock *clock.FakeClock

func TestScheduling(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Scheduling")
}

var _ = BeforeEach(func() {
	fakeClock = clock.NewFakeClock(time.Now())
})

var _ = Describe("parseDoNotDisrupt", func() {
	DescribeTable("should parse do-not-disrupt annotation values",
		func(value string, expectInf bool, expectDur time.Duration) {
			indefinite, duration := pod.ParseDoNotDisrupt(value)
			Expect(indefinite).To(Equal(expectInf))
			Expect(duration).To(Equal(expectDur))
		},
		Entry("true value", "true", true, time.Duration(0)),
		Entry("valid duration minutes", "5m", false, 5*time.Minute),
		Entry("valid duration hours", "2h", false, 2*time.Hour),
		Entry("invalid format", "invalid", true, time.Duration(0)),
		Entry("zero duration", "0s", true, time.Duration(0)),
		Entry("negative duration", "-5m", true, time.Duration(0)),
	)
})

var _ = Describe("IsDoNotDisruptActive", func() {
	It("should return false when no annotation exists", func() {
		p := &corev1.Pod{
			Status: corev1.PodStatus{
				StartTime: &metav1.Time{Time: fakeClock.Now().Add(-10 * time.Minute)},
			},
		}
		Expect(pod.IsDoNotDisruptActive(p, fakeClock)).To(BeFalse())
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
		Expect(pod.IsDoNotDisruptActive(p, fakeClock)).To(BeTrue())
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
		Expect(pod.IsDoNotDisruptActive(p, fakeClock)).To(BeTrue()) // 15m grace period not expired
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
		Expect(pod.IsDoNotDisruptActive(p, fakeClock)).To(BeFalse()) // 5m grace period expired
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
		Expect(pod.IsDoNotDisruptActive(p, fakeClock)).To(BeTrue()) // Fail safe when start time unknown
	})

	It("should fail safe for invalid duration format", func() {
		p := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{v1.DoNotDisruptAnnotationKey: "invalid"},
			},
			Status: corev1.PodStatus{
				StartTime: &metav1.Time{Time: fakeClock.Now().Add(-10 * time.Minute)},
			},
		}
		Expect(pod.IsDoNotDisruptActive(p, fakeClock)).To(BeTrue()) // Invalid duration treated as indefinite
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
		Expect(pod.IsDisruptable(p, fakeClock)).To(BeTrue())
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
		Expect(pod.IsDisruptable(p, fakeClock)).To(BeFalse())
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
		Expect(pod.IsDisruptable(p, fakeClock)).To(BeTrue()) // Grace period expired
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
		Expect(pod.IsDisruptable(p, fakeClock)).To(BeFalse()) // Grace period not expired
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
		Expect(pod.IsDisruptable(p, fakeClock)).To(BeTrue()) // Not active
	})
})
