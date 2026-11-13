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
)

var _ = Describe("externalControllerFromEnv", func() {
	BeforeEach(func() {
		GinkgoT().Setenv("KARPENTER_EXTERNAL_CONTROLLER", "test-controller")
	})
	It("parses KARPENTER_EXTERNAL_SETTINGS", func() {
		GinkgoT().Setenv("KARPENTER_EXTERNAL_SETTINGS", `{"MIN_VALUES_POLICY":"Strict","FEATURE_GATES":"NodeRepair=true,StaticCapacity=true"}`)
		Expect(externalControllerFromEnv().Settings).To(Equal(map[string]string{"MIN_VALUES_POLICY": "Strict", "FEATURE_GATES": "NodeRepair=true,StaticCapacity=true"}))
	})
	DescribeTable("panics on malformed KARPENTER_EXTERNAL_SETTINGS",
		func(value string) {
			GinkgoT().Setenv("KARPENTER_EXTERNAL_SETTINGS", value)
			Expect(func() { externalControllerFromEnv() }).To(Panic())
		},
		Entry("not JSON", "MIN_VALUES_POLICY=Strict"),
		Entry("non-string value", `{"FEATURE_GATES":true}`),
	)
})

var _ = Describe("unmetSetting", func() {
	const gates = "SpotToSpotConsolidation=true,NodeRepair=true,ReservedCapacity=true,NodeOverlay=false"
	DescribeTable("returns the wanted settings that aren't met",
		func(have, want string, unmet []string) {
			Expect(unmetSetting(have, want)).To(Equal(unmet))
		},
		Entry("same policy", "Strict", "Strict", []string(nil)),
		Entry("different policy", "Strict", "BestEffort", []string{`"BestEffort"`}),
		Entry("one gate met", gates, "NodeRepair=true", []string(nil)),
		Entry("gate values are case insensitive", gates, "NodeRepair=True,NodeOverlay=false", []string(nil)),
		Entry("all gates met", gates, "ReservedCapacity=true,SpotToSpotConsolidation=true,NodeRepair=true,NodeOverlay=false", []string(nil)),
		Entry("gate value differs", gates, "NodeOverlay=true", []string{"NodeOverlay=true"}),
		Entry("missing gates", gates, "TerminateFirstDrift=true,NodeRepair=true,Drift=true", []string{"Drift=true", "TerminateFirstDrift=true"}),
		Entry("policy against gates", gates, "Strict", []string{`"Strict"`}),
	)
})
