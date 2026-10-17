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
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("InstanceTypeFilterError is infeasible", func() {
	It("should be infeasible when no instance type fits", func() {
		e := InstanceTypeFilterError{fits: false, requirementsMet: true, hasOffering: true}
		Expect(e.IsInfeasible()).To(BeTrue())
	})

	It("should be infeasible when no instance type meets requirements", func() {
		e := InstanceTypeFilterError{fits: true, requirementsMet: false, hasOffering: true}
		Expect(e.IsInfeasible()).To(BeTrue())
	})

	It("should be infeasible when minValues are incompatible", func() {
		e := InstanceTypeFilterError{
			requirementsAndFits:      true,
			minValuesIncompatibleErr: fmt.Errorf("minValues incompatible"),
		}
		Expect(e.IsInfeasible()).To(BeTrue())
	})

	It("should not be infeasible when requirements and fits are met but offerings are missing", func() {
		e := InstanceTypeFilterError{fits: true, requirementsMet: true, hasOffering: false, requirementsAndFits: true}
		Expect(e.IsInfeasible()).To(BeFalse())
	})

	It("should be infeasible when fits and offering match but requirements do not", func() {
		e := InstanceTypeFilterError{fits: true, requirementsMet: false, hasOffering: true, fitsAndOffering: true}
		Expect(e.IsInfeasible()).To(BeTrue())
	})

	It("should not be infeasible when requirements and offering match with requirementsAndFits", func() {
		e := InstanceTypeFilterError{requirementsMet: true, hasOffering: true, requirementsAndOffering: true, requirementsAndFits: true}
		Expect(e.IsInfeasible()).To(BeFalse())
	})

	It("should be infeasible when all individual criteria fail", func() {
		e := InstanceTypeFilterError{fits: false, requirementsMet: false, hasOffering: false}
		Expect(e.IsInfeasible()).To(BeTrue())
	})

	It("should be infeasible when all individual criteria pass but no single instance meets requirements and fits", func() {
		e := InstanceTypeFilterError{fits: true, requirementsMet: true, hasOffering: true, requirementsAndFits: false}
		Expect(e.IsInfeasible()).To(BeTrue())
	})
})
