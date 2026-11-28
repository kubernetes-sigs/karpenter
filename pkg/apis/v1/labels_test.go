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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/labels"
	"sigs.k8s.io/karpenter/pkg/labels/labelstest"
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

var _ = Describe("LabelOptions", func() {
	BeforeEach(func() {
		labelstest.Register(GinkgoTB(), v1.LabelOptions()...)
	})

	It("should register the legacy well-known labels", func() {
		Expect(labels.WellKnown()).To(Equal(v1.WellKnownLabels))
	})
	It("should register the legacy restricted domains", func() {
		Expect(labels.Domains()).To(Equal(sets.List(v1.RestrictedLabelDomains)))
	})
	It("should deny exactly the legacy restricted labels on every resource", func() {
		deniedEverywhere := sets.New(labels.Denied(labels.All()[0])...)
		for _, r := range labels.All()[1:] {
			deniedEverywhere = deniedEverywhere.Intersection(sets.New(labels.Denied(r)...))
		}
		Expect(deniedEverywhere).To(Equal(v1.RestrictedLabels))
	})
	It("should register the legacy normalized labels as aliases", func() {
		Expect(labels.Aliases()).To(Equal(v1.NormalizedLabels))
	})
	It("should register the legacy known values, and no others", func() {
		keys := labels.WellKnown().Clone()
		for _, r := range labels.All() {
			keys.Insert(labels.Denied(r)...)
		}
		keys.Insert(lo.Keys(v1.WellKnownValuesForRequirements)...)
		for key := range keys {
			values, ok := labels.KnownValues(key)
			legacy, legacyOK := v1.WellKnownValuesForRequirements[key]
			Expect(ok).To(Equal(legacyOK), key)
			Expect(values).To(Equal(legacy), key)
		}
	})
	It("should restrict labels like the legacy checks", func() {
		probes := sets.New("karpenter.sh/unregistered", "sub.karpenter.sh/unregistered", "not-karpenter.sh/unregistered", "karpenter.sh", "example.com/unregistered", corev1.LabelHostname)
		probes.Insert(v1.WellKnownLabels.UnsortedList()...)
		probes.Insert(lo.Keys(v1.NormalizedLabels)...)
		for key := range probes {
			legacy := v1.IsRestrictedLabel(key) != nil
			for _, r := range []labels.Resource{labels.NodeClaim, labels.NodeOverlay, labels.Pod} {
				Expect(labels.Validate(key, r) != nil).To(Equal(legacy), "%s on %s", key, r)
			}
			// NodePool validation also rejects the nodepool label.
			Expect(labels.Validate(key, labels.NodePool) != nil).To(Equal(legacy || key == v1.NodePoolLabelKey), "%s on NodePool", key)
		}
	})
	It("should return a fresh list on every call", func() {
		first := v1.LabelOptions()
		for i := range first {
			first[i].Key = "edited"
			for j := range first[i].Aliases {
				first[i].Aliases[j] = "edited"
			}
			for j := range first[i].DenyOn {
				first[i].DenyOn[j] = "edited"
			}
			for j := range first[i].KnownValues {
				first[i].KnownValues[j] = "edited"
			}
		}
		Expect(v1.LabelOptions()).ToNot(ContainElement(HaveField("Key", "edited")))
		for _, o := range v1.LabelOptions() {
			Expect(o.Aliases).ToNot(ContainElement("edited"))
			Expect(o.DenyOn).ToNot(ContainElement(labels.Resource("edited")))
			Expect(o.KnownValues).ToNot(ContainElement("edited"))
		}
	})
})
