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

package scheduling

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/util/sets"
)

var _ = Describe("VolumeUsage", func() {
	It("should only use fallback limits until the driver is published", func() {
		const driver = "fake.csi.provider"
		usage := NewVolumeUsage()
		volumes := Volumes{driver: sets.New("volume-a", "volume-b")}

		usage.AddFallbackLimit(driver, 1)
		Expect(usage.ExceedsLimits(volumes)).To(HaveOccurred())

		usage.AddLimit(driver, 2)
		usage.AddFallbackLimit(driver, 1)
		Expect(usage.ExceedsLimits(volumes)).To(Succeed())

		usage.AddUnbounded(driver)
		usage.AddFallbackLimit(driver, 1)
		Expect(usage.ExceedsLimits(Volumes{driver: sets.New("volume-a", "volume-b", "volume-c")})).To(Succeed())
	})
})
