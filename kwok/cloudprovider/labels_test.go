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

package kwok_test

import (
	"context"
	"os"
	"path/filepath"

	"github.com/awslabs/operatorpkg/object"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	"sigs.k8s.io/karpenter/kwok/apis/v1alpha1"
	kwok "sigs.k8s.io/karpenter/kwok/cloudprovider"
	"sigs.k8s.io/karpenter/kwok/options"
	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	pscheduling "sigs.k8s.io/karpenter/pkg/controllers/provisioning/scheduling"
	"sigs.k8s.io/karpenter/pkg/labels"
	"sigs.k8s.io/karpenter/pkg/labels/labelstest"
	"sigs.k8s.io/karpenter/pkg/scheduling"
	"sigs.k8s.io/karpenter/pkg/test"
)

var _ = Describe("LabelOptions", func() {
	// coreAndKWOK is the kwok binary's list without offering keys: core's options, with on-demand-alt as a known
	// capacity type, then kwok's own.
	coreAndKWOK := func() []labels.Option {
		opts := v1.LabelOptions()
		for i := range opts {
			if opts[i].Key == v1.CapacityTypeLabelKey {
				opts[i].KnownValues = []string{v1.CapacityTypeOnDemand, kwok.CapacityTypeOnDemandAlt, v1.CapacityTypeReserved, v1.CapacityTypeSpot}
			}
		}
		return append(opts, v1alpha1.LabelOptions()...)
	}

	It("should add no offering keys for the embedded instance types, whose offering keys core registers", func() {
		opts := kwok.LabelOptions("")
		Expect(opts).To(Equal(coreAndKWOK()))
		labelstest.Register(GinkgoTB(), opts...)
	})
	It("should register each new offering key in a custom instance types file once", func() {
		path := filepath.Join(GinkgoT().TempDir(), "instance-types.json")
		Expect(os.WriteFile(path, []byte(`[{"name": "it", "offerings": [
			{"Price": 1, "Requirements": [{"key": "example.com/offering", "operator": "In", "values": ["a"]}, {"key": "topology.kubernetes.io/zone", "operator": "In", "values": ["z"]}]},
			{"Price": 2, "Requirements": [{"key": "example.com/offering", "operator": "In", "values": ["b"]}, {"key": "failure-domain.beta.kubernetes.io/zone", "operator": "In", "values": ["z"]}]}
		]}]`), 0o600)).To(Succeed())
		opts := kwok.LabelOptions(path)
		Expect(opts).To(Equal(append(coreAndKWOK(), labels.Option{Key: "example.com/offering"})))
		labelstest.Register(GinkgoTB(), opts...)
		Expect(labels.IsWellKnown("example.com/offering")).To(BeTrue())
	})
	It("should add no offering keys when the instance types file can't be read", func() {
		Expect(kwok.LabelOptions(filepath.Join(GinkgoT().TempDir(), "missing.json"))).To(Equal(coreAndKWOK()))
	})
	It("should skip offering keys that aren't qualified names, which instance type construction still accepts", func() {
		path := filepath.Join(GinkgoT().TempDir(), "instance-types.json")
		Expect(os.WriteFile(path, []byte(`[{"name": "it", "offerings": [
			{"Price": 1, "Requirements": [{"key": "not a key", "operator": "In", "values": ["a"]}, {"key": "example.com/offering", "operator": "In", "values": ["a"]}]}
		]}]`), 0o600)).To(Succeed())
		opts := kwok.LabelOptions(path)
		Expect(opts).To(Equal(append(coreAndKWOK(), labels.Option{Key: "example.com/offering"})))
		labelstest.Register(GinkgoTB(), opts...)
		instanceTypes, err := kwok.ConstructInstanceTypes(options.ToContext(context.Background(), &options.Options{InstanceTypesFilePath: path}))
		Expect(err).ToNot(HaveOccurred())
		Expect(instanceTypes[0].Offerings[0].Requirements.Has("not a key")).To(BeTrue())
	})

	Context("KWOKNodeClass label", func() {
		BeforeEach(func() {
			labelstest.Register(GinkgoTB(), kwok.LabelOptions("")...)
		})
		It("should be well-known and denied only on NodePools", func() {
			Expect(labels.IsWellKnown(v1alpha1.NodeClassLabelKey)).To(BeTrue())
			Expect(labels.Validate(v1alpha1.NodeClassLabelKey, labels.NodePool)).To(MatchError(`label "karpenter.kwok.sh/kwoknodeclass" is restricted on NodePool`))
			for _, r := range []labels.Resource{labels.NodeClaim, labels.NodeOverlay, labels.Pod} {
				Expect(labels.Validate(v1alpha1.NodeClassLabelKey, r)).To(Succeed(), string(r))
			}
		})
		It("should match pods selecting a NodePool's KWOKNodeClass to that NodePool, and not to another", func() {
			pod := test.Pod(test.PodOptions{NodeSelector: map[string]string{v1alpha1.NodeClassLabelKey: "a"}})
			nodePoolFor := func(nodeClass string) *v1.NodePool {
				return test.NodePool(v1.NodePool{Spec: v1.NodePoolSpec{Template: v1.NodeClaimTemplate{Spec: v1.NodeClaimTemplateSpec{
					NodeClassRef: &v1.NodeClassReference{Group: object.GVK(&v1alpha1.KWOKNodeClass{}).Group, Kind: object.GVK(&v1alpha1.KWOKNodeClass{}).Kind, Name: nodeClass},
				}}}})
			}
			podRequirements := scheduling.NewPodRequirements(pod)
			Expect(pscheduling.NewNodeClaimTemplate(nodePoolFor("a")).Requirements.Compatible(podRequirements, scheduling.AllowUndefinedWellKnownLabels)).To(Succeed())
			Expect(pscheduling.NewNodeClaimTemplate(nodePoolFor("b")).Requirements.Compatible(podRequirements, scheduling.AllowUndefinedWellKnownLabels)).ToNot(Succeed())
		})
		It("should be rejected on NodePools by kwok's generated CRDs", func() {
			nodePoolCRD := object.Unmarshal[apiextensionsv1.CustomResourceDefinition](lo.Must(os.ReadFile(filepath.Join("..", "charts", "crds", "karpenter.sh_nodepools.yaml"))))
			env := test.NewEnvironment(test.WithCRDs(nodePoolCRD))
			DeferCleanup(func() { Expect(env.Stop()).To(Succeed()) })
			ctx := context.Background()

			withLabel := test.NodePool()
			withLabel.Spec.Template.Labels = map[string]string{v1alpha1.NodeClassLabelKey: "default"}
			Expect(env.Client.Create(ctx, withLabel)).To(MatchError(ContainSubstring(`label "karpenter.kwok.sh/kwoknodeclass" is restricted on NodePool`)))
			withRequirement := test.NodePool()
			withRequirement.Spec.Template.Spec.Requirements = []v1.NodeSelectorRequirementWithMinValues{{Key: v1alpha1.NodeClassLabelKey, Operator: corev1.NodeSelectorOpIn, Values: []string{"default"}}}
			Expect(env.Client.Create(ctx, withRequirement)).To(MatchError(ContainSubstring(`label "karpenter.kwok.sh/kwoknodeclass" is restricted on NodePool`)))
			// Another kwok well-known key is admitted.
			control := test.NodePool()
			control.Spec.Template.Labels = map[string]string{v1alpha1.InstanceSizeLabelKey: "large"}
			control.Spec.Template.Spec.Requirements = []v1.NodeSelectorRequirementWithMinValues{{Key: v1alpha1.InstanceSizeLabelKey, Operator: corev1.NodeSelectorOpIn, Values: []string{"large"}}}
			Expect(env.Client.Create(ctx, control)).To(Succeed())
		})
		It("should reject NodePools that use it", func() {
			nodePool := test.NodePool()
			nodePool.Spec.Template.Labels = map[string]string{v1alpha1.NodeClassLabelKey: "default"}
			nodePool.Spec.Template.Spec.Requirements = []v1.NodeSelectorRequirementWithMinValues{{Key: v1alpha1.NodeClassLabelKey, Operator: corev1.NodeSelectorOpIn, Values: []string{"default"}}}
			Expect(nodePool.RuntimeValidate(context.Background())).To(MatchError(And(
				ContainSubstring(`invalid key name "karpenter.kwok.sh/kwoknodeclass" in labels`),
				ContainSubstring(`label "karpenter.kwok.sh/kwoknodeclass" is restricted on NodePool in requirements`),
			)))
		})
	})
})
