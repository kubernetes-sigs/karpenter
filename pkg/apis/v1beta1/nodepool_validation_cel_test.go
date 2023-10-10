/*
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

package v1beta1_test

import (
	"strings"
	"time"

	"github.com/Pallinder/go-randomdata"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	. "github.com/aws/karpenter-core/pkg/apis/v1beta1"
)

var _ = Describe("CEL/Validation", func() {
	var nodePool *NodePool

	BeforeEach(func() {
		if env.Version.Minor() < 25 {
			Skip("CEL Validation is for 1.25>")
		}
		nodePool = &NodePool{
			ObjectMeta: metav1.ObjectMeta{Name: strings.ToLower(randomdata.SillyName())},
			Spec: NodePoolSpec{
				Template: NodeClaimTemplate{
					Spec: NodeClaimSpec{
						NodeClassRef: &NodeClassReference{
							Kind: "NodeClaim",
							Name: "default",
						},
						Requirements: []v1.NodeSelectorRequirement{
							{
								Key:      CapacityTypeLabelKey,
								Operator: v1.NodeSelectorOpExists,
							},
						},
					},
				},
			},
		}
	})
	Context("Disruption", func() {
		It("should fail on negative expireAfter", func() {
			nodePool.Spec.Disruption.ExpireAfter.Duration = lo.ToPtr(lo.Must(time.ParseDuration("-1s")))
			Expect(env.Client.Create(ctx, nodePool)).ToNot(Succeed())
		})
		It("should succeed on a disabled expireAfter", func() {
			nodePool.Spec.Disruption.ExpireAfter.Duration = nil
			Expect(env.Client.Create(ctx, nodePool)).To(Succeed())
		})
		It("should succeed on a valid expireAfter", func() {
			nodePool.Spec.Disruption.ExpireAfter.Duration = lo.ToPtr(lo.Must(time.ParseDuration("30s")))
			Expect(env.Client.Create(ctx, nodePool)).To(Succeed())
		})
		It("should fail on negative consolidateAfter", func() {
			nodePool.Spec.Disruption.ConsolidateAfter = &NillableDuration{Duration: lo.ToPtr(lo.Must(time.ParseDuration("-1s")))}
			Expect(env.Client.Create(ctx, nodePool)).ToNot(Succeed())
		})
		It("should succeed on a disabled consolidateAfter", func() {
			nodePool.Spec.Disruption.ConsolidateAfter = &NillableDuration{Duration: nil}
			Expect(env.Client.Create(ctx, nodePool)).To(Succeed())
		})
		It("should succeed on a valid consolidateAfter", func() {
			nodePool.Spec.Disruption.ConsolidateAfter = &NillableDuration{Duration: lo.ToPtr(lo.Must(time.ParseDuration("30s")))}
			nodePool.Spec.Disruption.ConsolidationPolicy = ConsolidationPolicyWhenEmpty
			Expect(env.Client.Create(ctx, nodePool)).To(Succeed())
		})
		It("should succeed when setting consolidateAfter with consolidationPolicy=WhenEmpty", func() {
			nodePool.Spec.Disruption.ConsolidateAfter = &NillableDuration{Duration: lo.ToPtr(lo.Must(time.ParseDuration("30s")))}
			nodePool.Spec.Disruption.ConsolidationPolicy = ConsolidationPolicyWhenEmpty
			Expect(env.Client.Create(ctx, nodePool)).To(Succeed())
		})
		It("should fail when setting consolidateAfter with consolidationPolicy=WhenUnderutilized", func() {
			nodePool.Spec.Disruption.ConsolidateAfter = &NillableDuration{Duration: lo.ToPtr(lo.Must(time.ParseDuration("30s")))}
			nodePool.Spec.Disruption.ConsolidationPolicy = ConsolidationPolicyWhenUnderutilized
			Expect(env.Client.Create(ctx, nodePool)).ToNot(Succeed())
		})
		It("should succeed when not setting consolidateAfter to 'Never' with consolidationPolicy=WhenUnderutilized", func() {
			nodePool.Spec.Disruption.ConsolidateAfter = &NillableDuration{Duration: nil}
			nodePool.Spec.Disruption.ConsolidationPolicy = ConsolidationPolicyWhenUnderutilized
			Expect(env.Client.Create(ctx, nodePool)).To(Succeed())
		})
	})
})
