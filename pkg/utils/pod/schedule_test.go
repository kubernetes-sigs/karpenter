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

func TestSchedule(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Pod Schedule")
}

var _ = Describe("IsDisruptable", func() {
	var fakeClock *clock.FakeClock
	var testPod *corev1.Pod

	BeforeEach(func() {
		fakeClock = clock.NewFakeClock(time.Date(2000, time.June, 17, 2, 30, 0, 0, time.UTC))
		testPod = &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-pod",
				Namespace: "default",
			},
			Status: corev1.PodStatus{
				Phase: corev1.PodRunning,
			},
		}
	})

	It("should return true for pods without any disruption annotations", func() {
		Expect(pod.IsDisruptable(testPod, fakeClock)).To(BeTrue())
	})

	It("should return false for pods with do-not-disrupt annotation", func() {
		testPod.Annotations = map[string]string{
			v1.DoNotDisruptAnnotationKey: "true",
		}
		Expect(pod.IsDisruptable(testPod, fakeClock)).To(BeFalse())
	})

	It("should return true for terminal pods even with do-not-disrupt annotation", func() {
		testPod.Annotations = map[string]string{
			v1.DoNotDisruptAnnotationKey: "true",
		}
		testPod.Status.Phase = corev1.PodSucceeded
		Expect(pod.IsDisruptable(testPod, fakeClock)).To(BeTrue())
	})

	It("should return false when outside disruption schedule window", func() {
		// Set clock to a time outside the Saturday 2 AM window
		fakeClock = clock.NewFakeClock(time.Date(2000, time.June, 17, 10, 0, 0, 0, time.UTC))
		testPod.Annotations = map[string]string{
			v1.DisruptionScheduleAnnotationKey:         "0 2 * * 6",
			v1.DisruptionScheduleDurationAnnotationKey: "4h",
		}
		Expect(pod.IsDisruptable(testPod, fakeClock)).To(BeFalse())
	})

	It("should return true when inside disruption schedule window", func() {
		testPod.Annotations = map[string]string{
			v1.DisruptionScheduleAnnotationKey:         "0 2 * * 6",
			v1.DisruptionScheduleDurationAnnotationKey: "4h",
		}
		Expect(pod.IsDisruptable(testPod, fakeClock)).To(BeTrue())
	})

	It("do-not-disrupt should take precedence over schedule", func() {
		// Even with an active schedule, do-not-disrupt blocks disruption
		testPod.Annotations = map[string]string{
			v1.DoNotDisruptAnnotationKey:               "true",
			v1.DisruptionScheduleAnnotationKey:         "0 2 * * 6",
			v1.DisruptionScheduleDurationAnnotationKey: "4h",
		}
		Expect(pod.IsDisruptable(testPod, fakeClock)).To(BeFalse())
	})
})

var _ = Describe("DisruptionSchedule", func() {
	var fakeClock *clock.FakeClock
	var testPod *corev1.Pod

	BeforeEach(func() {
		// Set the time to Saturday 2 AM UTC (within typical maintenance window)
		// June 17, 2000 is a Saturday
		fakeClock = clock.NewFakeClock(time.Date(2000, time.June, 17, 2, 30, 0, 0, time.UTC))
		testPod = &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-pod",
				Namespace: "default",
			},
		}
	})

	Context("EvaluateDisruptionSchedule", func() {
		It("should return HasSchedule=false and IsActive=true when no annotations exist", func() {
			result := pod.EvaluateDisruptionSchedule(testPod, fakeClock)
			Expect(result.HasSchedule).To(BeFalse())
			Expect(result.IsActive).To(BeTrue())
		})

		It("should return HasSchedule=false and IsActive=true when schedule annotation is empty", func() {
			testPod.Annotations = map[string]string{
				v1.DisruptionScheduleAnnotationKey: "",
			}
			result := pod.EvaluateDisruptionSchedule(testPod, fakeClock)
			Expect(result.HasSchedule).To(BeFalse())
			Expect(result.IsActive).To(BeTrue())
		})

		It("should return IsActive=true when current time is within the schedule window", func() {
			// Schedule: 2 AM on Saturdays, duration: 4h
			// Current time: Saturday 2:30 AM (within window)
			testPod.Annotations = map[string]string{
				v1.DisruptionScheduleAnnotationKey:         "0 2 * * 6",
				v1.DisruptionScheduleDurationAnnotationKey: "4h",
			}
			result := pod.EvaluateDisruptionSchedule(testPod, fakeClock)
			Expect(result.HasSchedule).To(BeTrue())
			Expect(result.IsActive).To(BeTrue())
			Expect(result.InvalidSchedule).To(BeFalse())
			Expect(result.InvalidDuration).To(BeFalse())
		})

		It("should return IsActive=false when current time is outside the schedule window", func() {
			// Schedule: 2 AM on Saturdays, duration: 1h
			// Current time: Saturday 2:30 AM (outside 1h window that ended at 3 AM)
			// Actually wait, 2:30 AM is still within the window from 2 AM to 3 AM
			// Let's set the clock to 4 AM which is definitely outside
			fakeClock = clock.NewFakeClock(time.Date(2000, time.June, 17, 4, 0, 0, 0, time.UTC))
			testPod.Annotations = map[string]string{
				v1.DisruptionScheduleAnnotationKey:         "0 2 * * 6",
				v1.DisruptionScheduleDurationAnnotationKey: "1h",
			}
			result := pod.EvaluateDisruptionSchedule(testPod, fakeClock)
			Expect(result.HasSchedule).To(BeTrue())
			Expect(result.IsActive).To(BeFalse())
		})

		It("should return IsActive=true and InvalidSchedule=true for invalid cron expression (fail-open)", func() {
			testPod.Annotations = map[string]string{
				v1.DisruptionScheduleAnnotationKey:         "invalid-cron",
				v1.DisruptionScheduleDurationAnnotationKey: "1h",
			}
			result := pod.EvaluateDisruptionSchedule(testPod, fakeClock)
			Expect(result.HasSchedule).To(BeTrue())
			Expect(result.IsActive).To(BeTrue()) // fail-open
			Expect(result.InvalidSchedule).To(BeTrue())
			Expect(result.ErrorMessage).To(ContainSubstring("invalid cron schedule"))
		})

		It("should return IsActive=true and InvalidDuration=true for invalid duration (fail-open)", func() {
			testPod.Annotations = map[string]string{
				v1.DisruptionScheduleAnnotationKey:         "0 2 * * 6",
				v1.DisruptionScheduleDurationAnnotationKey: "not-a-duration",
			}
			result := pod.EvaluateDisruptionSchedule(testPod, fakeClock)
			Expect(result.HasSchedule).To(BeTrue())
			Expect(result.IsActive).To(BeTrue()) // fail-open
			Expect(result.InvalidDuration).To(BeTrue())
			Expect(result.ErrorMessage).To(ContainSubstring("invalid duration"))
		})

		It("should return IsActive=true and InvalidDuration=true for negative duration (fail-open)", func() {
			testPod.Annotations = map[string]string{
				v1.DisruptionScheduleAnnotationKey:         "0 2 * * 6",
				v1.DisruptionScheduleDurationAnnotationKey: "-1h",
			}
			result := pod.EvaluateDisruptionSchedule(testPod, fakeClock)
			Expect(result.HasSchedule).To(BeTrue())
			Expect(result.IsActive).To(BeTrue()) // fail-open
			Expect(result.InvalidDuration).To(BeTrue())
		})

		It("should use default duration of 1h when duration annotation is not specified", func() {
			// Schedule: 2 AM on Saturdays (default duration: 1h)
			// Current time: Saturday 2:30 AM (within 1h window)
			testPod.Annotations = map[string]string{
				v1.DisruptionScheduleAnnotationKey: "0 2 * * 6",
			}
			result := pod.EvaluateDisruptionSchedule(testPod, fakeClock)
			Expect(result.HasSchedule).To(BeTrue())
			Expect(result.IsActive).To(BeTrue())

			// Move clock past 1h window
			fakeClock = clock.NewFakeClock(time.Date(2000, time.June, 17, 3, 30, 0, 0, time.UTC))
			result = pod.EvaluateDisruptionSchedule(testPod, fakeClock)
			Expect(result.IsActive).To(BeFalse())
		})

		It("should handle @daily schedule shorthand", func() {
			// @daily means 0 0 * * * (midnight every day)
			// Set clock to just after midnight
			fakeClock = clock.NewFakeClock(time.Date(2000, time.June, 17, 0, 30, 0, 0, time.UTC))
			testPod.Annotations = map[string]string{
				v1.DisruptionScheduleAnnotationKey:         "@daily",
				v1.DisruptionScheduleDurationAnnotationKey: "1h",
			}
			result := pod.EvaluateDisruptionSchedule(testPod, fakeClock)
			Expect(result.HasSchedule).To(BeTrue())
			Expect(result.IsActive).To(BeTrue())
		})

		It("should handle @weekly schedule shorthand", func() {
			// @weekly means 0 0 * * 0 (midnight on Sunday)
			// Set clock to Sunday midnight + 30 min
			// June 18, 2000 is a Sunday
			fakeClock = clock.NewFakeClock(time.Date(2000, time.June, 18, 0, 30, 0, 0, time.UTC))
			testPod.Annotations = map[string]string{
				v1.DisruptionScheduleAnnotationKey:         "@weekly",
				v1.DisruptionScheduleDurationAnnotationKey: "2h",
			}
			result := pod.EvaluateDisruptionSchedule(testPod, fakeClock)
			Expect(result.HasSchedule).To(BeTrue())
			Expect(result.IsActive).To(BeTrue())
		})

		It("should always evaluate schedule in UTC", func() {
			// The clock is in a non-UTC timezone, but schedule should be evaluated in UTC
			fakeClock = clock.NewFakeClock(time.Date(2000, time.June, 17, 2, 30, 0, 0, time.FixedZone("UTC+5", 5*3600)))
			testPod.Annotations = map[string]string{
				v1.DisruptionScheduleAnnotationKey:         "0 2 * * 6",
				v1.DisruptionScheduleDurationAnnotationKey: "4h",
			}
			result := pod.EvaluateDisruptionSchedule(testPod, fakeClock)
			// The schedule is evaluated in UTC. At 2:30 AM in UTC+5, it's 9:30 PM UTC on Friday
			// which is outside the Saturday 2 AM window
			Expect(result.HasSchedule).To(BeTrue())
			Expect(result.IsActive).To(BeFalse())
		})

		It("should handle overlapping schedule windows correctly", func() {
			// Set up a schedule that runs every hour for 2 hours (always active)
			testPod.Annotations = map[string]string{
				v1.DisruptionScheduleAnnotationKey:         "0 * * * *",
				v1.DisruptionScheduleDurationAnnotationKey: "2h",
			}
			result := pod.EvaluateDisruptionSchedule(testPod, fakeClock)
			Expect(result.IsActive).To(BeTrue())
		})
	})

	Context("IsWithinDisruptionSchedule", func() {
		It("should return true when no schedule is set", func() {
			Expect(pod.IsWithinDisruptionSchedule(testPod, fakeClock)).To(BeTrue())
		})

		It("should return true when within schedule window", func() {
			testPod.Annotations = map[string]string{
				v1.DisruptionScheduleAnnotationKey:         "0 2 * * 6",
				v1.DisruptionScheduleDurationAnnotationKey: "4h",
			}
			Expect(pod.IsWithinDisruptionSchedule(testPod, fakeClock)).To(BeTrue())
		})

		It("should return false when outside schedule window", func() {
			fakeClock = clock.NewFakeClock(time.Date(2000, time.June, 17, 10, 0, 0, 0, time.UTC))
			testPod.Annotations = map[string]string{
				v1.DisruptionScheduleAnnotationKey:         "0 2 * * 6",
				v1.DisruptionScheduleDurationAnnotationKey: "4h",
			}
			Expect(pod.IsWithinDisruptionSchedule(testPod, fakeClock)).To(BeFalse())
		})

		It("should return true for invalid schedule (fail-open)", func() {
			testPod.Annotations = map[string]string{
				v1.DisruptionScheduleAnnotationKey: "invalid",
			}
			Expect(pod.IsWithinDisruptionSchedule(testPod, fakeClock)).To(BeTrue())
		})
	})
})
