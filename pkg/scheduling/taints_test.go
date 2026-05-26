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
	corev1 "k8s.io/api/core/v1"
)

var _ = Describe("IsKnownEphemeralTaint", func() {
	It("matches an exact entry in KnownEphemeralTaints", func() {
		Expect(IsKnownEphemeralTaint(&corev1.Taint{
			Key:    corev1.TaintNodeNotReady,
			Effect: corev1.TaintEffectNoSchedule,
		})).To(BeTrue())
	})

	It("matches a NodeReadinessController taint by prefix", func() {
		Expect(IsKnownEphemeralTaint(&corev1.Taint{
			Key:    "readiness.k8s.io/my-rule",
			Effect: corev1.TaintEffectNoSchedule,
		})).To(BeTrue())
	})

	It("matches any effect for a prefixed taint", func() {
		Expect(IsKnownEphemeralTaint(&corev1.Taint{
			Key:    "readiness.k8s.io/another",
			Effect: corev1.TaintEffectNoExecute,
		})).To(BeTrue())
	})

	It("does not match an unrelated taint", func() {
		Expect(IsKnownEphemeralTaint(&corev1.Taint{
			Key:    "example.com/some-taint",
			Effect: corev1.TaintEffectNoSchedule,
		})).To(BeFalse())
	})

	It("does not match a taint whose key only contains the prefix in the middle", func() {
		Expect(IsKnownEphemeralTaint(&corev1.Taint{
			Key:    "foo/readiness.k8s.io/bar",
			Effect: corev1.TaintEffectNoSchedule,
		})).To(BeFalse())
	})

	It("returns false for a nil taint", func() {
		Expect(IsKnownEphemeralTaint(nil)).To(BeFalse())
	})
})
