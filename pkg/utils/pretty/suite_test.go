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

package pretty_test

import (
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	clocktest "k8s.io/utils/clock/testing"

	. "sigs.k8s.io/karpenter/pkg/utils/pretty"
)

func TestPretty(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Pretty Suite")
}

var _ = Describe("ErrorStateMonitor", func() {
	var (
		mockClock *clocktest.FakeClock
		monitor   *ErrorStateMonitor
	)

	BeforeEach(func() {
		mockClock = clocktest.NewFakeClock(time.Now())
		monitor = NewErrorStateMonitor("synced", 1*time.Minute, 5*time.Minute, mockClock)
	})

	Describe("Alarm", func() {
		Context("when the actual state does not match the goal state and the error threshold is exceeded", func() {
			It("should trigger an alarm as soon as the error threshold is exceeded", func() {
				// Move time to exceed the error threshold but not the interval
				mockClock.SetTime(mockClock.Now().Add(1*time.Minute + 1*time.Second))
				Expect(monitor.Alarm("unsynced")).To(BeTrue())
			})
		})

		Context("subsequent alarms after the first", func() {
			BeforeEach(func() {
				// Set up initial condition: First alarm triggered by exceeding error threshold
				mockClock.SetTime(mockClock.Now().Add(1*time.Minute + 1*time.Second))
				monitor.Alarm("unsynced")
			})

			It("should not trigger a subsequent alarm until the interval has passed since the last alarm", func() {

				mockClock.SetTime(mockClock.Now().Add(3 * time.Minute))
				Expect(monitor.Alarm("unsynced")).To(BeFalse())
				mockClock.SetTime(mockClock.Now().Add(1*time.Minute + 59*time.Second))
				Expect(monitor.Alarm("unsynced")).To(BeFalse())
				// We exceed the 5 minute interval, we should fire an alarm
				mockClock.SetTime(mockClock.Now().Add(2 * time.Second))
				Expect(monitor.Alarm("unsynced")).To(BeTrue())
			})

			It("should alarm when reaching errorThreshold again after we have synced", func() {
				// If state reaches the goal, future uses of the same monitor getting out of state
				// should fire right as errorThreshold is reached
				Expect(monitor.Alarm("synced")).To(BeFalse())

				mockClock.SetTime(mockClock.Now().Add(1*time.Minute + 1*time.Second))
				Expect(monitor.Alarm("unsynced")).To(BeTrue())
			})
		})
	})
})
