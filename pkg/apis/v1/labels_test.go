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

package v1_test

import (
	"github.com/awslabs/operatorpkg/docs"
	"github.com/awslabs/operatorpkg/wellknown"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

var _ = Describe("WellKnownAnnotations", func() {
	annotations := v1.KarpenterAnnotations

	It("should document every annotation", func() {
		for _, annotation := range annotations {
			Expect(annotation.Name).ToNot(BeEmpty())
			Expect(annotation.Example).ToNot(BeEmpty(), annotation.Name)
			Expect(annotation.Help).ToNot(BeEmpty(), annotation.Name)
			Expect(annotation.UsedOn).ToNot(BeEmpty(), annotation.Name)
			Expect(annotation.Stage).To(BeElementOf(docs.Alpha, docs.Beta, docs.GA), annotation.Name)
		}
	})
	It("should mark every internal only annotation as alpha", func() {
		for _, annotation := range annotations {
			if annotation.InternalOnly {
				Expect(annotation.Stage).To(Equal(docs.Alpha), annotation.Name)
			}
		}
	})
	It("should not document an annotation twice", func() {
		Expect(lo.FindDuplicatesBy(annotations, func(a wellknown.Annotation) string { return a.Name })).To(BeEmpty())
	})
})
