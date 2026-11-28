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

	It("should register core's well-known labels", func() {
		Expect(labels.WellKnown()).To(Equal(sets.New(
			v1.NodePoolLabelKey,
			corev1.LabelTopologyZone,
			corev1.LabelTopologyRegion,
			corev1.LabelInstanceTypeStable,
			corev1.LabelArchStable,
			corev1.LabelOSStable,
			v1.CapacityTypeLabelKey,
			corev1.LabelWindowsBuild,
		)))
	})
	It("should restrict the karpenter.sh domain", func() {
		Expect(labels.Domains()).To(Equal([]string{"karpenter.sh"}))
	})
	It("should deny only kubernetes.io/hostname on every resource", func() {
		deniedEverywhere := sets.New(labels.Denied(labels.All()[0])...)
		for _, r := range labels.All()[1:] {
			deniedEverywhere = deniedEverywhere.Intersection(sets.New(labels.Denied(r)...))
		}
		Expect(deniedEverywhere).To(Equal(sets.New(corev1.LabelHostname)))
	})
	It("should alias the beta and legacy labels to their stable keys", func() {
		Expect(labels.Aliases()).To(Equal(map[string]string{
			corev1.LabelFailureDomainBetaZone:   corev1.LabelTopologyZone,
			"beta.kubernetes.io/arch":           corev1.LabelArchStable,
			"beta.kubernetes.io/os":             corev1.LabelOSStable,
			corev1.LabelInstanceType:            corev1.LabelInstanceTypeStable,
			corev1.LabelFailureDomainBetaRegion: corev1.LabelTopologyRegion,
		}))
	})
	It("should register known values only for the capacity type", func() {
		keys := labels.WellKnown().Clone()
		for _, r := range labels.All() {
			keys.Insert(labels.Denied(r)...)
		}
		for key := range keys {
			values, ok := labels.KnownValues(key)
			if key == v1.CapacityTypeLabelKey {
				Expect(ok).To(BeTrue(), key)
				Expect(values).To(Equal(sets.New(v1.CapacityTypeOnDemand, v1.CapacityTypeSpot, v1.CapacityTypeReserved)), key)
				continue
			}
			Expect(ok).To(BeFalse(), key)
		}
	})
	It("should restrict exactly the karpenter.sh domain's unregistered keys and kubernetes.io/hostname", func() {
		restricted := sets.New("karpenter.sh/unregistered", "sub.karpenter.sh/unregistered", corev1.LabelHostname)
		probes := restricted.Union(sets.New("not-karpenter.sh/unregistered", "karpenter.sh", "example.com/unregistered"))
		probes.Insert(labels.WellKnown().UnsortedList()...)
		probes.Insert(lo.Keys(labels.Aliases())...)
		for key := range probes {
			for _, r := range []labels.Resource{labels.NodeClaim, labels.NodeOverlay, labels.Pod} {
				Expect(labels.Validate(key, r) != nil).To(Equal(restricted.Has(key)), "%s on %s", key, r)
			}
			// NodePool validation also rejects the nodepool label.
			Expect(labels.Validate(key, labels.NodePool) != nil).To(Equal(restricted.Has(key) || key == v1.NodePoolLabelKey), "%s on NodePool", key)
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
