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

package cel_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"sigs.k8s.io/karpenter/pkg/apis/v1alpha1/cel"
)

func TestCEL(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "CEL Suite")
}

var _ = Describe("PriceExpression", func() {
	Describe("Compile", func() {
		DescribeTable("valid expressions",
			func(expr string) {
				_, err := cel.Compile(expr)
				Expect(err).NotTo(HaveOccurred())
			},
			Entry("identity", "self.price"),
			Entry("percentage discount", "self.price * 0.9"),
			Entry("compound adjustment", "(self.price * 0.9 + 0.05) * 1.03"),
			Entry("flat addition", "self.price + 1.5"),
			Entry("flat subtraction", "self.price - 0.5"),
			Entry("integer constant", "1.0"),
		)

		DescribeTable("invalid expressions",
			func(expr string) {
				_, err := cel.Compile(expr)
				Expect(err).To(HaveOccurred())
			},
			Entry("syntax error", "self.price *"),
			Entry("unknown variable", "other.price * 0.9"),
			Entry("string return type", `"not a number"`),
			Entry("boolean return type", "self.price > 0.0"),
		)
	})

	Describe("Evaluate", func() {
		DescribeTable("correct results",
			func(expr string, basePrice float64, expected float64) {
				p, err := cel.Compile(expr)
				Expect(err).NotTo(HaveOccurred())
				result, err := p.Evaluate(basePrice)
				Expect(err).NotTo(HaveOccurred())
				Expect(result).To(BeNumerically("~", expected, 1e-9))
			},
			Entry("identity", "self.price", 1.0, 1.0),
			Entry("10% discount", "self.price * 0.9", 2.0, 1.8),
			Entry("compound jmdeal example", "(self.price * 0.9 + 0.05) * 1.03", 1.0, 0.9785),
			Entry("flat +1", "self.price + 1.0", 0.5, 1.5),
			Entry("zero base price", "self.price * 0.5", 0.0, 0.0),
		)

		It("returns an error for negative results", func() {
			p, err := cel.Compile("self.price - 999.0")
			Expect(err).NotTo(HaveOccurred())
			_, err = p.Evaluate(1.0)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("negative"))
		})
	})
})
