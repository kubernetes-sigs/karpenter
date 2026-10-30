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

package common

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
)

var _ = Describe("parseCondition", func() {
	DescribeTable("parses type=status[/reason]",
		func(value string, want corev1.NodeCondition) {
			Expect(parseCondition("test", value)).To(Equal(want))
		},
		Entry("type and status", "KernelReady=False", corev1.NodeCondition{Type: "KernelReady", Status: corev1.ConditionFalse}),
		Entry("type, status and reason", "AcceleratedHardwareReady=False/NvidiaXID46Error", corev1.NodeCondition{Type: "AcceleratedHardwareReady", Status: corev1.ConditionFalse, Reason: "NvidiaXID46Error"}),
	)
	DescribeTable("panics on malformed values",
		func(value string) {
			Expect(func() { parseCondition("test", value) }).To(Panic())
		},
		Entry("no status", "KernelReady"),
		Entry("no type", "=False"),
		Entry("empty status", "KernelReady="),
		Entry("empty status with reason", "KernelReady=/Reason"),
	)
})
