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

package options_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"sigs.k8s.io/karpenter/kwok/options"
)

func TestOptions(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "KWOK Options")
}

var _ = Describe("FromArgs", func() {
	DescribeTable("should read the instance types file path among other flags",
		func(args ...string) {
			Expect(options.FromArgs(args).InstanceTypesFilePath).To(Equal("/from/flag"))
		},
		Entry("as a flag and value", "--instance-types-file-path", "/from/flag"),
		Entry("with =", "--instance-types-file-path=/from/flag"),
		Entry("after an unknown flag and its value", "--log-level", "debug", "--instance-types-file-path", "/from/flag"),
		Entry("after an unknown boolean flag", "--disable-leader-election", "--instance-types-file-path=/from/flag"),
		Entry("after an argument with bad flag syntax", "---x", "--instance-types-file-path=/from/flag"),
		Entry("after a flag without a name", "--=x", "-instance-types-file-path=/from/flag"),
		Entry("after a single-dash flag without a name", "-=x", "--instance-types-file-path", "/from/flag"),
	)
	It("should fall back to the environment, and prefer the flag", func() {
		GinkgoT().Setenv("INSTANCE_TYPES_FILE_PATH", "/from/env")
		Expect(options.FromArgs([]string{"--log-level", "debug"}).InstanceTypesFilePath).To(Equal("/from/env"))
		Expect(options.FromArgs([]string{"--instance-types-file-path", "/from/flag"}).InstanceTypesFilePath).To(Equal("/from/flag"))
	})
	It("should be empty without the flag or the environment", func() {
		GinkgoT().Setenv("INSTANCE_TYPES_FILE_PATH", "")
		Expect(options.FromArgs([]string{"--log-level", "debug"}).InstanceTypesFilePath).To(BeEmpty())
	})
})
