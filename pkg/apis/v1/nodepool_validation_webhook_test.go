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
	"fmt"
	"strings"
	"time"

	"github.com/Pallinder/go-randomdata"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"knative.dev/pkg/ptr"

	. "sigs.k8s.io/karpenter/pkg/apis/v1"
)

var _ = Describe("Webhook/Validation", func() {
	var nodePool *NodePool

	BeforeEach(func() {
		nodePool = &NodePool{
			ObjectMeta: metav1.ObjectMeta{Name: strings.ToLower(randomdata.SillyName())},
			Spec: NodePoolSpec{
				Template: NodeClaimTemplate{
					Spec: NodeClaimSpec{
						NodeClassRef: &NodeClassReference{
							Kind: "NodeClaim",
							Name: "default",
						},
					},
				},
			},
		}
	})
	Context("Disruption", func() {
		It("should succeed on a disabled expireAfter", func() {
			nodePool.Spec.Disruption.ExpireAfter.Duration = nil
			Expect(nodePool.Validate(ctx)).To(Succeed())
		})
		It("should succeed on a valid expireAfter", func() {
			nodePool.Spec.Disruption.ExpireAfter.Duration = lo.ToPtr(lo.Must(time.ParseDuration("30s")))
			Expect(nodePool.Validate(ctx)).To(Succeed())
		})
		It("should succeed on a disabled consolidateAfter", func() {
			nodePool.Spec.Disruption.ConsolidateAfter = &NillableDuration{Duration: nil}
			Expect(nodePool.Validate(ctx)).To(Succeed())
		})
		It("should succeed on a valid consolidateAfter", func() {
			nodePool.Spec.Disruption.ConsolidateAfter = &NillableDuration{Duration: lo.ToPtr(lo.Must(time.ParseDuration("30s")))}
			Expect(nodePool.Validate(ctx)).To(Succeed())
		})
		It("should succeed when setting consolidateAfter with consolidationPolicy=WhenEmpty", func() {
			nodePool.Spec.Disruption.ConsolidateAfter = &NillableDuration{Duration: lo.ToPtr(lo.Must(time.ParseDuration("30s")))}
			nodePool.Spec.Disruption.ConsolidationPolicy = ConsolidationPolicyWhenEmpty
			Expect(nodePool.Validate(ctx)).To(Succeed())
		})
		It("should fail when setting consolidateAfter with consolidationPolicy=WhenUnderutilized", func() {
			nodePool.Spec.Disruption.ConsolidateAfter = &NillableDuration{Duration: lo.ToPtr(lo.Must(time.ParseDuration("30s")))}
			nodePool.Spec.Disruption.ConsolidationPolicy = ConsolidationPolicyWhenUnderutilized
			Expect(nodePool.Validate(ctx)).ToNot(Succeed())
		})
		It("should fail to validate a budget with an invalid cron", func() {
			nodePool.Spec.Disruption.Budgets = []Budget{{
				Nodes:    "10",
				Schedule: ptr.String("*"),
				Duration: &metav1.Duration{Duration: lo.Must(time.ParseDuration("20m"))},
			}}
			Expect(nodePool.Validate(ctx)).ToNot(Succeed())
		})
		It("should fail to validate a schedule with less than 5 entries", func() {
			nodePool.Spec.Disruption.Budgets = []Budget{{
				Nodes:    "10",
				Schedule: ptr.String("* * * * "),
				Duration: &metav1.Duration{Duration: lo.Must(time.ParseDuration("20m"))},
			}}
			Expect(nodePool.Validate(ctx)).ToNot(Succeed())
		})
		It("should fail to validate a budget with a cron but no duration", func() {
			nodePool.Spec.Disruption.Budgets = []Budget{{
				Nodes:    "10",
				Schedule: ptr.String("* * * * *"),
			}}
			Expect(nodePool.Validate(ctx)).ToNot(Succeed())
		})
		It("should fail to validate a budget with a duration but no cron", func() {
			nodePool.Spec.Disruption.Budgets = []Budget{{
				Nodes:    "10",
				Duration: &metav1.Duration{Duration: lo.Must(time.ParseDuration("20m"))},
			}}
			Expect(nodePool.Validate(ctx)).ToNot(Succeed())
		})
		It("should succeed to validate a budget with both duration and cron", func() {
			nodePool.Spec.Disruption.Budgets = []Budget{{
				Nodes:    "10",
				Schedule: ptr.String("* * * * *"),
				Duration: &metav1.Duration{Duration: lo.Must(time.ParseDuration("20m"))},
			}}
			Expect(nodePool.Validate(ctx)).To(Succeed())
		})
		It("should succeed to validate a budget with neither duration nor cron", func() {
			nodePool.Spec.Disruption.Budgets = []Budget{{
				Nodes: "10",
			}}
			Expect(nodePool.Validate(ctx)).To(Succeed())
		})
		It("should succeed to validate a budget with special cased crons", func() {
			nodePool.Spec.Disruption.Budgets = []Budget{{
				Nodes:    "10",
				Schedule: ptr.String("@annually"),
				Duration: &metav1.Duration{Duration: lo.Must(time.ParseDuration("20m"))},
			}}
			Expect(nodePool.Validate(ctx)).To(Succeed())
		})
		It("should succeed when creating a budget with hours and minutes in duration", func() {
			nodePool.Spec.Disruption.Budgets = []Budget{{
				Nodes:    "10",
				Schedule: ptr.String("* * * * *"),
				Duration: &metav1.Duration{Duration: lo.Must(time.ParseDuration("2h20m"))},
			}}
			Expect(nodePool.Validate(ctx)).To(Succeed())
		})
		It("should fail when creating two budgets where one has an invalid crontab", func() {
			nodePool.Spec.Disruption.Budgets = []Budget{
				{
					Nodes:    "10",
					Schedule: ptr.String("@annually"),
					Duration: &metav1.Duration{Duration: lo.Must(time.ParseDuration("20m"))},
				},
				{
					Nodes:    "10",
					Schedule: ptr.String("*"),
					Duration: &metav1.Duration{Duration: lo.Must(time.ParseDuration("20m"))},
				}}
			Expect(nodePool.Validate(ctx)).ToNot(Succeed())
		})
		It("should fail when creating multiple budgets where one doesn't have both schedule and duration", func() {
			nodePool.Spec.Disruption.Budgets = []Budget{
				{
					Nodes:    "10",
					Duration: &metav1.Duration{Duration: lo.Must(time.ParseDuration("20m"))},
				},
				{
					Nodes:    "10",
					Schedule: ptr.String("* * * * *"),
					Duration: &metav1.Duration{Duration: lo.Must(time.ParseDuration("20m"))},
				},
				{
					Nodes: "10",
				},
			}
			Expect(nodePool.Validate(ctx)).ToNot(Succeed())
		})
	})
	Context("Limits", func() {
		It("should allow undefined limits", func() {
			nodePool.Spec.Limits = nil
			Expect(nodePool.Validate(ctx)).To(Succeed())
		})
		It("should allow empty limits", func() {
			nodePool.Spec.Limits = Limits(v1.ResourceList{})
			Expect(nodePool.Validate(ctx)).To(Succeed())
		})
	})
	Context("Template", func() {
		It("should fail if resource requests are set", func() {
			nodePool.Spec.Template.Spec.Resources.Requests = v1.ResourceList{
				v1.ResourceCPU: resource.MustParse("5"),
			}
			Expect(nodePool.Validate(ctx)).ToNot(Succeed())
		})
		Context("Labels", func() {
			It("should allow unrecognized labels", func() {
				nodePool.Spec.Template.Labels = map[string]string{"foo": randomdata.SillyName()}
				Expect(nodePool.Validate(ctx)).To(Succeed())
			})
			It("should fail for the karpenter.sh/nodepool label", func() {
				nodePool.Spec.Template.Labels = map[string]string{NodePoolLabelKey: randomdata.SillyName()}
				Expect(nodePool.Validate(ctx)).ToNot(Succeed())
			})
			It("should fail for invalid label keys", func() {
				nodePool.Spec.Template.Labels = map[string]string{"spaces are not allowed": randomdata.SillyName()}
				Expect(nodePool.Validate(ctx)).ToNot(Succeed())
			})
			It("should fail for invalid label values", func() {
				nodePool.Spec.Template.Labels = map[string]string{randomdata.SillyName(): "/ is not allowed"}
				Expect(nodePool.Validate(ctx)).ToNot(Succeed())
			})
			It("should fail for restricted label domains", func() {
				for label := range RestrictedLabelDomains {
					nodePool.Spec.Template.Labels = map[string]string{label + "/unknown": randomdata.SillyName()}
					Expect(nodePool.Validate(ctx)).ToNot(Succeed())
				}
			})
			It("should allow labels kOps require", func() {
				nodePool.Spec.Template.Labels = map[string]string{
					"kops.k8s.io/instancegroup": "karpenter-nodes",
					"kops.k8s.io/gpu":           "1",
				}
				Expect(nodePool.Validate(ctx)).To(Succeed())
			})
			It("should allow labels in restricted domains exceptions list", func() {
				for label := range LabelDomainExceptions {
					nodePool.Spec.Template.Labels = map[string]string{
						label: "test-value",
					}
					Expect(nodePool.Validate(ctx)).To(Succeed())
				}
			})
			It("should allow labels prefixed with the restricted domain exceptions", func() {
				for label := range LabelDomainExceptions {
					nodePool.Spec.Template.Labels = map[string]string{
						fmt.Sprintf("%s/key", label): "test-value",
					}
					Expect(nodePool.Validate(ctx)).To(Succeed())
				}
			})
			It("should allow subdomain labels in restricted domains exceptions list", func() {
				for label := range LabelDomainExceptions {
					nodePool.Spec.Template.Labels = map[string]string{
						fmt.Sprintf("subdomain.%s", label): "test-value",
					}
					Expect(nodePool.Validate(ctx)).To(Succeed())
				}
			})
			It("should allow subdomain labels prefixed with the restricted domain exceptions", func() {
				for label := range LabelDomainExceptions {
					nodePool.Spec.Template.Labels = map[string]string{
						fmt.Sprintf("subdomain.%s/key", label): "test-value",
					}
					Expect(nodePool.Validate(ctx)).To(Succeed())
				}
			})
		})
		Context("Taints", func() {
			It("should succeed for valid taints", func() {
				nodePool.Spec.Template.Spec.Taints = []v1.Taint{
					{Key: "a", Value: "b", Effect: v1.TaintEffectNoSchedule},
					{Key: "c", Value: "d", Effect: v1.TaintEffectNoExecute},
					{Key: "e", Value: "f", Effect: v1.TaintEffectPreferNoSchedule},
					{Key: "key-only", Effect: v1.TaintEffectNoExecute},
				}
				Expect(nodePool.Validate(ctx)).To(Succeed())
			})
			It("should fail for invalid taint keys", func() {
				nodePool.Spec.Template.Spec.Taints = []v1.Taint{{Key: "???"}}
				Expect(nodePool.Validate(ctx)).ToNot(Succeed())
			})
			It("should fail for missing taint key", func() {
				nodePool.Spec.Template.Spec.Taints = []v1.Taint{{Effect: v1.TaintEffectNoSchedule}}
				Expect(nodePool.Validate(ctx)).ToNot(Succeed())
			})
			It("should fail for invalid taint value", func() {
				nodePool.Spec.Template.Spec.Taints = []v1.Taint{{Key: "invalid-value", Effect: v1.TaintEffectNoSchedule, Value: "???"}}
				Expect(nodePool.Validate(ctx)).ToNot(Succeed())
			})
			It("should fail for invalid taint effect", func() {
				nodePool.Spec.Template.Spec.Taints = []v1.Taint{{Key: "invalid-effect", Effect: "???"}}
				Expect(nodePool.Validate(ctx)).ToNot(Succeed())
			})
			It("should not fail for same key with different effects", func() {
				nodePool.Spec.Template.Spec.Taints = []v1.Taint{
					{Key: "a", Effect: v1.TaintEffectNoSchedule},
					{Key: "a", Effect: v1.TaintEffectNoExecute},
				}
				Expect(nodePool.Validate(ctx)).To(Succeed())
			})
			It("should fail for duplicate taint key/effect pairs", func() {
				nodePool.Spec.Template.Spec.Taints = []v1.Taint{
					{Key: "a", Effect: v1.TaintEffectNoSchedule},
					{Key: "a", Effect: v1.TaintEffectNoSchedule},
				}
				Expect(nodePool.Validate(ctx)).ToNot(Succeed())
				nodePool.Spec.Template.Spec.Taints = []v1.Taint{
					{Key: "a", Effect: v1.TaintEffectNoSchedule},
				}
				nodePool.Spec.Template.Spec.StartupTaints = []v1.Taint{
					{Key: "a", Effect: v1.TaintEffectNoSchedule},
				}
				Expect(nodePool.Validate(ctx)).ToNot(Succeed())
			})
		})
		Context("Requirements", func() {
			It("should fail for the karpenter.sh/nodepool label", func() {
				nodePool.Spec.Template.Spec.Requirements = []NodeSelectorRequirementWithMinValues{
					{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: NodePoolLabelKey, Operator: v1.NodeSelectorOpIn, Values: []string{randomdata.SillyName()}}},
				}
				Expect(nodePool.Validate(ctx)).ToNot(Succeed())
			})
			It("should allow supported ops", func() {
				nodePool.Spec.Template.Spec.Requirements = []NodeSelectorRequirementWithMinValues{
					{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpIn, Values: []string{"test"}}},
					{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpGt, Values: []string{"1"}}},
					{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpLt, Values: []string{"1"}}},
					{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpNotIn}},
					{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpExists}},
				}
				Expect(nodePool.Validate(ctx)).To(Succeed())
			})
			It("should fail for unsupported ops", func() {
				for _, op := range []v1.NodeSelectorOperator{"unknown"} {
					nodePool.Spec.Template.Spec.Requirements = []NodeSelectorRequirementWithMinValues{
						{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: v1.LabelTopologyZone, Operator: op, Values: []string{"test"}}},
					}
					Expect(nodePool.Validate(ctx)).ToNot(Succeed())
				}
			})
			It("should fail for restricted domains", func() {
				for label := range RestrictedLabelDomains {
					nodePool.Spec.Template.Spec.Requirements = []NodeSelectorRequirementWithMinValues{
						{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: label + "/test", Operator: v1.NodeSelectorOpIn, Values: []string{"test"}}},
					}
					Expect(nodePool.Validate(ctx)).ToNot(Succeed())
				}
			})
			It("should allow restricted domains exceptions", func() {
				for label := range LabelDomainExceptions {
					nodePool.Spec.Template.Spec.Requirements = []NodeSelectorRequirementWithMinValues{
						{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: label + "/test", Operator: v1.NodeSelectorOpIn, Values: []string{"test"}}},
					}
					Expect(nodePool.Validate(ctx)).To(Succeed())
				}
			})
			It("should allow restricted subdomains exceptions", func() {
				for label := range LabelDomainExceptions {
					nodePool.Spec.Template.Spec.Requirements = []NodeSelectorRequirementWithMinValues{
						{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: "subdomain." + label + "/test", Operator: v1.NodeSelectorOpIn, Values: []string{"test"}}},
					}
					Expect(nodePool.Validate(ctx)).To(Succeed())
				}
			})
			It("should allow well known label exceptions", func() {
				for label := range WellKnownLabels.Difference(sets.New(NodePoolLabelKey)) {
					nodePool.Spec.Template.Spec.Requirements = []NodeSelectorRequirementWithMinValues{
						{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: label, Operator: v1.NodeSelectorOpIn, Values: []string{"test"}}},
					}
					Expect(nodePool.Validate(ctx)).To(Succeed())
				}
			})
			It("should allow non-empty set after removing overlapped value", func() {
				nodePool.Spec.Template.Spec.Requirements = []NodeSelectorRequirementWithMinValues{
					{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpIn, Values: []string{"test", "foo"}}},
					{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpNotIn, Values: []string{"test", "bar"}}},
				}
				Expect(nodePool.Validate(ctx)).To(Succeed())
			})
			It("should allow empty requirements", func() {
				nodePool.Spec.Template.Spec.Requirements = []NodeSelectorRequirementWithMinValues{}
				Expect(nodePool.Validate(ctx)).To(Succeed())
			})
			It("should fail with invalid GT or LT values", func() {
				for _, requirement := range []NodeSelectorRequirementWithMinValues{
					{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpGt, Values: []string{}}},
					{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpGt, Values: []string{"1", "2"}}},
					{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpGt, Values: []string{"a"}}},
					{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpGt, Values: []string{"-1"}}},
					{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpLt, Values: []string{}}},
					{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpLt, Values: []string{"1", "2"}}},
					{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpLt, Values: []string{"a"}}},
					{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpLt, Values: []string{"-1"}}},
				} {
					nodePool.Spec.Template.Spec.Requirements = []NodeSelectorRequirementWithMinValues{requirement}
					Expect(nodePool.Validate(ctx)).ToNot(Succeed())
				}
			})
			It("should error when minValues is greater than the number of unique values specified within In operator", func() {
				nodePool.Spec.Template.Spec.Requirements = []NodeSelectorRequirementWithMinValues{
					{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: v1.LabelInstanceTypeStable, Operator: v1.NodeSelectorOpIn, Values: []string{"insance-type-1"}}, MinValues: lo.ToPtr(2)},
				}
				Expect(nodePool.Validate(ctx)).ToNot(Succeed())
			})
		})
	})
})

var _ = Describe("Limits", func() {
	var nodepool *NodePool

	BeforeEach(func() {
		nodepool = &NodePool{
			ObjectMeta: metav1.ObjectMeta{Name: strings.ToLower(randomdata.SillyName())},
			Spec: NodePoolSpec{
				Limits: Limits(v1.ResourceList{
					"cpu": resource.MustParse("16"),
				}),
			},
		}
	})

	It("should work when usage is lower than limit", func() {
		nodepool.Status.Resources = v1.ResourceList{"cpu": resource.MustParse("15")}
		Expect(nodepool.Spec.Limits.ExceededBy(nodepool.Status.Resources)).To(Succeed())
	})
	It("should work when usage is equal to limit", func() {
		nodepool.Status.Resources = v1.ResourceList{"cpu": resource.MustParse("16")}
		Expect(nodepool.Spec.Limits.ExceededBy(nodepool.Status.Resources)).To(Succeed())
	})
	It("should fail when usage is higher than limit", func() {
		nodepool.Status.Resources = v1.ResourceList{"cpu": resource.MustParse("17")}
		Expect(nodepool.Spec.Limits.ExceededBy(nodepool.Status.Resources)).To(MatchError("cpu resource usage of 17 exceeds limit of 16"))
	})
})
