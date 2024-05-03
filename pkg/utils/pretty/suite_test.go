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
		Context("when the actual state matches the goal state", func() {
			It("should reset the lastResolved time and not alarm", func() {
				Expect(monitor.Alarm("synced")).To(BeFalse())
				Expect(monitor.LastResolved()).To(Equal(mockClock.Now()))
			})
		})

		Context("when the actual state does not match the goal state", func() {
			It("should alarm after threshold and interval have passed", func() {
				mockClock.SetTime(mockClock.Now().Add(61 * time.Second))
				Expect(monitor.Alarm("unsynced")).To(BeFalse())
				currentTime := mockClock.Now()
				mockClock.SetTime(currentTime.Add(5 * time.Minute))
				Expect(monitor.Alarm("unsynced")).To(BeTrue())
				Expect(monitor.LastAlarmed()).To(Equal(mockClock.Now()))
			})
		})
	})

})
