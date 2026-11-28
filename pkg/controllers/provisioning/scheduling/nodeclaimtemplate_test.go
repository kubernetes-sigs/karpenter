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

package scheduling_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
	"sigs.k8s.io/karpenter/pkg/controllers/provisioning/scheduling"
	"sigs.k8s.io/karpenter/pkg/labels"
	"sigs.k8s.io/karpenter/pkg/labels/labelstest"
	"sigs.k8s.io/karpenter/pkg/test"
)

var _ = Describe("NodeClaimTemplate", func() {
	It("should stamp custom labels from requirements, skipping restricted keys", func() {
		labelstest.Register(GinkgoTB(), labelstest.With(fake.LabelOptions(), labels.Option{Key: "example.com/restricted", DenyOn: labels.All()})...)
		nodePool := test.NodePool()
		nodePool.Spec.Template.Spec.Requirements = []v1.NodeSelectorRequirementWithMinValues{
			{Key: "example.com/restricted", Operator: corev1.NodeSelectorOpIn, Values: []string{"a"}},
			{Key: "example.com/custom", Operator: corev1.NodeSelectorOpIn, Values: []string{"b"}},
		}
		nodeClaim := scheduling.NewNodeClaimTemplate(nodePool).ToNodeClaim()
		Expect(nodeClaim.Labels).To(HaveKeyWithValue("example.com/custom", "b"))
		Expect(nodeClaim.Labels).ToNot(HaveKey("example.com/restricted"))
	})
})
