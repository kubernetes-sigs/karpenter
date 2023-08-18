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

package v1beta1

import (
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
)

var _ = Describe("Validation", func() {
	var nodePool *NodePool

	BeforeEach(func() {
		nodePool = &NodePool{
			ObjectMeta: metav1.ObjectMeta{Name: strings.ToLower(randomdata.SillyName())},
			Spec: NodePoolSpec{
				Template: NodeClaimTemplate{
					Spec: NodeClaimSpec{
						NodeClass: &NodeClassReference{
							Kind: "NodeClaim",
							Name: "default",
						},
					},
				},
			},
		}
	})

	Context("Deprovisioning", func() {
		It("should fail on negative expiry ttl", func() {
			nodePool.Spec.Deprovisioning.ExpirationTTL.Duration = lo.Must(time.ParseDuration("-1s"))
			Expect(nodePool.Validate(ctx)).ToNot(Succeed())
		})
		It("should succeed on a missing expiry ttl", func() {
			// this already is true, but to be explicit
			nodePool.Spec.Deprovisioning.ExpirationTTL.Duration = 0
			Expect(nodePool.Validate(ctx)).To(Succeed())
		})
		It("should succeed on a valid expiry ttl", func() {
			nodePool.Spec.Deprovisioning.ExpirationTTL.Duration = lo.Must(time.ParseDuration("30s"))
			Expect(nodePool.Validate(ctx)).To(Succeed())
		})
		It("should fail on negative consolidation ttl", func() {
			nodePool.Spec.Deprovisioning.ConsolidationTTL.Duration = lo.Must(time.ParseDuration("-1s"))
			Expect(nodePool.Validate(ctx)).ToNot(Succeed())
		})
		It("should succeed on a missing consolidation ttl", func() {
			// this already is true, but to be explicit
			nodePool.Spec.Deprovisioning.ConsolidationTTL.Duration = 0
			Expect(nodePool.Validate(ctx)).To(Succeed())
		})
		It("should succeed on a valid consolidation ttl", func() {
			nodePool.Spec.Deprovisioning.ConsolidationTTL.Duration = lo.Must(time.ParseDuration("30s"))
			Expect(nodePool.Validate(ctx)).To(Succeed())
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
				nodePool.Spec.Template.Spec.Requirements = []v1.NodeSelectorRequirement{
					{Key: NodePoolLabelKey, Operator: v1.NodeSelectorOpIn, Values: []string{randomdata.SillyName()}},
				}
				Expect(nodePool.Validate(ctx)).ToNot(Succeed())
			})
			It("should allow supported ops", func() {
				nodePool.Spec.Template.Spec.Requirements = []v1.NodeSelectorRequirement{
					{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpIn, Values: []string{"test"}},
					{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpGt, Values: []string{"1"}},
					{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpLt, Values: []string{"1"}},
					{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpNotIn},
					{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpExists},
				}
				Expect(nodePool.Validate(ctx)).To(Succeed())
			})
			It("should fail for unsupported ops", func() {
				for _, op := range []v1.NodeSelectorOperator{"unknown"} {
					nodePool.Spec.Template.Spec.Requirements = []v1.NodeSelectorRequirement{
						{Key: v1.LabelTopologyZone, Operator: op, Values: []string{"test"}},
					}
					Expect(nodePool.Validate(ctx)).ToNot(Succeed())
				}
			})
			It("should fail for restricted domains", func() {
				for label := range RestrictedLabelDomains {
					nodePool.Spec.Template.Spec.Requirements = []v1.NodeSelectorRequirement{
						{Key: label + "/test", Operator: v1.NodeSelectorOpIn, Values: []string{"test"}},
					}
					Expect(nodePool.Validate(ctx)).ToNot(Succeed())
				}
			})
			It("should allow restricted domains exceptions", func() {
				for label := range LabelDomainExceptions {
					nodePool.Spec.Template.Spec.Requirements = []v1.NodeSelectorRequirement{
						{Key: label + "/test", Operator: v1.NodeSelectorOpIn, Values: []string{"test"}},
					}
					Expect(nodePool.Validate(ctx)).To(Succeed())
				}
			})
			It("should allow well known label exceptions", func() {
				for label := range WellKnownLabels.Difference(sets.New(NodePoolLabelKey)) {
					nodePool.Spec.Template.Spec.Requirements = []v1.NodeSelectorRequirement{
						{Key: label, Operator: v1.NodeSelectorOpIn, Values: []string{"test"}},
					}
					Expect(nodePool.Validate(ctx)).To(Succeed())
				}
			})
			It("should allow non-empty set after removing overlapped value", func() {
				nodePool.Spec.Template.Spec.Requirements = []v1.NodeSelectorRequirement{
					{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpIn, Values: []string{"test", "foo"}},
					{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpNotIn, Values: []string{"test", "bar"}},
				}
				Expect(nodePool.Validate(ctx)).To(Succeed())
			})
			It("should allow empty requirements", func() {
				nodePool.Spec.Template.Spec.Requirements = []v1.NodeSelectorRequirement{}
				Expect(nodePool.Validate(ctx)).To(Succeed())
			})
			It("should fail with invalid GT or LT values", func() {
				for _, requirement := range []v1.NodeSelectorRequirement{
					{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpGt, Values: []string{}},
					{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpGt, Values: []string{"1", "2"}},
					{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpGt, Values: []string{"a"}},
					{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpGt, Values: []string{"-1"}},
					{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpLt, Values: []string{}},
					{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpLt, Values: []string{"1", "2"}},
					{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpLt, Values: []string{"a"}},
					{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpLt, Values: []string{"-1"}},
				} {
					nodePool.Spec.Template.Spec.Requirements = []v1.NodeSelectorRequirement{requirement}
					Expect(nodePool.Validate(ctx)).ToNot(Succeed())
				}
			})
		})
		Context("KubeletConfiguration", func() {
			It("should fail on kubeReserved with invalid keys", func() {
				nodePool.Spec.Template.Spec.KubeletConfiguration = &KubeletConfiguration{
					KubeReserved: v1.ResourceList{
						v1.ResourcePods: resource.MustParse("2"),
					},
				}
				Expect(nodePool.Validate(ctx)).ToNot(Succeed())
			})
			It("should fail on systemReserved with invalid keys", func() {
				nodePool.Spec.Template.Spec.KubeletConfiguration = &KubeletConfiguration{
					SystemReserved: v1.ResourceList{
						v1.ResourcePods: resource.MustParse("2"),
					},
				}
				Expect(nodePool.Validate(ctx)).ToNot(Succeed())
			})
			Context("Eviction Signals", func() {
				Context("Eviction Hard", func() {
					It("should succeed on evictionHard with valid keys", func() {
						nodePool.Spec.Template.Spec.KubeletConfiguration = &KubeletConfiguration{
							EvictionHard: map[string]string{
								"memory.available":   "5%",
								"nodefs.available":   "10%",
								"nodefs.inodesFree":  "15%",
								"imagefs.available":  "5%",
								"imagefs.inodesFree": "5%",
								"pid.available":      "5%",
							},
						}
						Expect(nodePool.Validate(ctx)).To(Succeed())
					})
					It("should fail on evictionHard with invalid keys", func() {
						nodePool.Spec.Template.Spec.KubeletConfiguration = &KubeletConfiguration{
							EvictionHard: map[string]string{
								"memory": "5%",
							},
						}
						Expect(nodePool.Validate(ctx)).ToNot(Succeed())
					})
					It("should fail on invalid formatted percentage value in evictionHard", func() {
						nodePool.Spec.Template.Spec.KubeletConfiguration = &KubeletConfiguration{
							EvictionHard: map[string]string{
								"memory.available": "5%3",
							},
						}
						Expect(nodePool.Validate(ctx)).ToNot(Succeed())
					})
					It("should fail on invalid percentage value (too large) in evictionHard", func() {
						nodePool.Spec.Template.Spec.KubeletConfiguration = &KubeletConfiguration{
							EvictionHard: map[string]string{
								"memory.available": "110%",
							},
						}
						Expect(nodePool.Validate(ctx)).ToNot(Succeed())
					})
					It("should fail on invalid quantity value in evictionHard", func() {
						nodePool.Spec.Template.Spec.KubeletConfiguration = &KubeletConfiguration{
							EvictionHard: map[string]string{
								"memory.available": "110GB",
							},
						}
						Expect(nodePool.Validate(ctx)).ToNot(Succeed())
					})
				})
			})
			Context("Eviction Soft", func() {
				It("should succeed on evictionSoft with valid keys", func() {
					nodePool.Spec.Template.Spec.KubeletConfiguration = &KubeletConfiguration{
						EvictionSoft: map[string]string{
							"memory.available":   "5%",
							"nodefs.available":   "10%",
							"nodefs.inodesFree":  "15%",
							"imagefs.available":  "5%",
							"imagefs.inodesFree": "5%",
							"pid.available":      "5%",
						},
						EvictionSoftGracePeriod: map[string]metav1.Duration{
							"memory.available":   {Duration: time.Minute},
							"nodefs.available":   {Duration: time.Second * 90},
							"nodefs.inodesFree":  {Duration: time.Minute * 5},
							"imagefs.available":  {Duration: time.Hour},
							"imagefs.inodesFree": {Duration: time.Hour * 24},
							"pid.available":      {Duration: time.Minute},
						},
					}
					Expect(nodePool.Validate(ctx)).To(Succeed())
				})
				It("should fail on evictionSoft with invalid keys", func() {
					nodePool.Spec.Template.Spec.KubeletConfiguration = &KubeletConfiguration{
						EvictionSoft: map[string]string{
							"memory": "5%",
						},
						EvictionSoftGracePeriod: map[string]metav1.Duration{
							"memory": {Duration: time.Minute},
						},
					}
					Expect(nodePool.Validate(ctx)).ToNot(Succeed())
				})
				It("should fail on invalid formatted percentage value in evictionSoft", func() {
					nodePool.Spec.Template.Spec.KubeletConfiguration = &KubeletConfiguration{
						EvictionSoft: map[string]string{
							"memory.available": "5%3",
						},
						EvictionSoftGracePeriod: map[string]metav1.Duration{
							"memory.available": {Duration: time.Minute},
						},
					}
					Expect(nodePool.Validate(ctx)).ToNot(Succeed())
				})
				It("should fail on invalid percentage value (too large) in evictionSoft", func() {
					nodePool.Spec.Template.Spec.KubeletConfiguration = &KubeletConfiguration{
						EvictionSoft: map[string]string{
							"memory.available": "110%",
						},
						EvictionSoftGracePeriod: map[string]metav1.Duration{
							"memory.available": {Duration: time.Minute},
						},
					}
					Expect(nodePool.Validate(ctx)).ToNot(Succeed())
				})
				It("should fail on invalid quantity value in evictionSoft", func() {
					nodePool.Spec.Template.Spec.KubeletConfiguration = &KubeletConfiguration{
						EvictionSoft: map[string]string{
							"memory.available": "110GB",
						},
						EvictionSoftGracePeriod: map[string]metav1.Duration{
							"memory.available": {Duration: time.Minute},
						},
					}
					Expect(nodePool.Validate(ctx)).ToNot(Succeed())
				})
				It("should fail when eviction soft doesn't have matching grace period", func() {
					nodePool.Spec.Template.Spec.KubeletConfiguration = &KubeletConfiguration{
						EvictionSoft: map[string]string{
							"memory.available": "200Mi",
						},
					}
					Expect(nodePool.Validate(ctx)).ToNot(Succeed())
				})
			})
			Context("GCThresholdPercent", func() {
				Context("ImageGCHighThresholdPercent", func() {
					It("should succeed on a imageGCHighThresholdPercent", func() {
						nodePool.Spec.Template.Spec.KubeletConfiguration = &KubeletConfiguration{
							ImageGCHighThresholdPercent: ptr.Int32(10),
						}
						Expect(nodePool.Validate(ctx)).To(Succeed())
					})
					It("should fail when imageGCHighThresholdPercent is less than imageGCLowThresholdPercent", func() {
						nodePool.Spec.Template.Spec.KubeletConfiguration = &KubeletConfiguration{
							ImageGCHighThresholdPercent: ptr.Int32(50),
							ImageGCLowThresholdPercent:  ptr.Int32(60),
						}
						Expect(nodePool.Validate(ctx)).ToNot(Succeed())
					})
				})
				Context("ImageGCLowThresholdPercent", func() {
					It("should succeed on a imageGCLowThresholdPercent", func() {
						nodePool.Spec.Template.Spec.KubeletConfiguration = &KubeletConfiguration{
							ImageGCLowThresholdPercent: ptr.Int32(10),
						}
						Expect(nodePool.Validate(ctx)).To(Succeed())
					})
					It("should fail when imageGCLowThresholdPercent is greather than imageGCHighThresheldPercent", func() {
						nodePool.Spec.Template.Spec.KubeletConfiguration = &KubeletConfiguration{
							ImageGCHighThresholdPercent: ptr.Int32(50),
							ImageGCLowThresholdPercent:  ptr.Int32(60),
						}
						Expect(nodePool.Validate(ctx)).ToNot(Succeed())
					})
				})
			})
			Context("Eviction Soft Grace Period", func() {
				It("should succeed on evictionSoftGracePeriod with valid keys", func() {
					nodePool.Spec.Template.Spec.KubeletConfiguration = &KubeletConfiguration{
						EvictionSoft: map[string]string{
							"memory.available":   "5%",
							"nodefs.available":   "10%",
							"nodefs.inodesFree":  "15%",
							"imagefs.available":  "5%",
							"imagefs.inodesFree": "5%",
							"pid.available":      "5%",
						},
						EvictionSoftGracePeriod: map[string]metav1.Duration{
							"memory.available":   {Duration: time.Minute},
							"nodefs.available":   {Duration: time.Second * 90},
							"nodefs.inodesFree":  {Duration: time.Minute * 5},
							"imagefs.available":  {Duration: time.Hour},
							"imagefs.inodesFree": {Duration: time.Hour * 24},
							"pid.available":      {Duration: time.Minute},
						},
					}
					Expect(nodePool.Validate(ctx)).To(Succeed())
				})
				It("should fail on evictionSoftGracePeriod with invalid keys", func() {
					nodePool.Spec.Template.Spec.KubeletConfiguration = &KubeletConfiguration{
						EvictionSoftGracePeriod: map[string]metav1.Duration{
							"memory": {Duration: time.Minute},
						},
					}
					Expect(nodePool.Validate(ctx)).ToNot(Succeed())
				})
				It("should fail when eviction soft grace period doesn't have matching threshold", func() {
					nodePool.Spec.Template.Spec.KubeletConfiguration = &KubeletConfiguration{
						EvictionSoftGracePeriod: map[string]metav1.Duration{
							"memory.available": {Duration: time.Minute},
						},
					}
					Expect(nodePool.Validate(ctx)).ToNot(Succeed())
				})
			})
		})
	})
})

var _ = Describe("Limits", func() {
	var provisioner *NodePool

	BeforeEach(func() {
		provisioner = &NodePool{
			ObjectMeta: metav1.ObjectMeta{Name: strings.ToLower(randomdata.SillyName())},
			Spec: NodePoolSpec{
				Limits: Limits(v1.ResourceList{
					"cpu": resource.MustParse("16"),
				}),
			},
		}
	})

	It("should work when usage is lower than limit", func() {
		provisioner.Status.Resources = v1.ResourceList{"cpu": resource.MustParse("15")}
		Expect(provisioner.Spec.Limits.ExceededBy(provisioner.Status.Resources)).To(Succeed())
	})
	It("should work when usage is equal to limit", func() {
		provisioner.Status.Resources = v1.ResourceList{"cpu": resource.MustParse("16")}
		Expect(provisioner.Spec.Limits.ExceededBy(provisioner.Status.Resources)).To(Succeed())
	})
	It("should fail when usage is higher than limit", func() {
		provisioner.Status.Resources = v1.ResourceList{"cpu": resource.MustParse("17")}
		Expect(provisioner.Spec.Limits.ExceededBy(provisioner.Status.Resources)).To(MatchError("cpu resource usage of 17 exceeds limit of 16"))
	})
})
