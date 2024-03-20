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

package v1beta1_test

import (
	"strconv"
	"strings"
	"time"

	"github.com/Pallinder/go-randomdata"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	"knative.dev/pkg/ptr"

	. "sigs.k8s.io/karpenter/pkg/apis/v1beta1"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
)

var _ = Describe("Validation", func() {
	var nodeClaim *NodeClaim

	BeforeEach(func() {
		if env.Version.Minor() < 25 {
			Skip("CEL Validation is for 1.25>")
		}
		nodeClaim = &NodeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: strings.ToLower(randomdata.SillyName())},
			Spec: NodeClaimSpec{
				NodeClassRef: &NodeClassReference{
					Kind: "NodeClaim",
					Name: "default",
				},
				Requirements: []NodeSelectorRequirementWithMinValues{
					{
						NodeSelectorRequirement: v1.NodeSelectorRequirement{
							Key:      CapacityTypeLabelKey,
							Operator: v1.NodeSelectorOpExists,
						},
					},
				},
			},
		}
	})

	Context("Taints", func() {
		It("should succeed for valid taints", func() {
			nodeClaim.Spec.Taints = []v1.Taint{
				{Key: "a", Value: "b", Effect: v1.TaintEffectNoSchedule},
				{Key: "c", Value: "d", Effect: v1.TaintEffectNoExecute},
				{Key: "e", Value: "f", Effect: v1.TaintEffectPreferNoSchedule},
				{Key: "key-only", Effect: v1.TaintEffectNoExecute},
			}
			Expect(env.Client.Create(ctx, nodeClaim)).To(Succeed())
		})
		It("should fail for invalid taint keys", func() {
			nodeClaim.Spec.Taints = []v1.Taint{{Key: "???"}}
			Expect(env.Client.Create(ctx, nodeClaim)).ToNot(Succeed())
		})
		It("should fail for missing taint key", func() {
			nodeClaim.Spec.Taints = []v1.Taint{{Effect: v1.TaintEffectNoSchedule}}
			Expect(env.Client.Create(ctx, nodeClaim)).ToNot(Succeed())
		})
		It("should fail for invalid taint value", func() {
			nodeClaim.Spec.Taints = []v1.Taint{{Key: "invalid-value", Effect: v1.TaintEffectNoSchedule, Value: "???"}}
			Expect(env.Client.Create(ctx, nodeClaim)).ToNot(Succeed())
		})
		It("should fail for invalid taint effect", func() {
			nodeClaim.Spec.Taints = []v1.Taint{{Key: "invalid-effect", Effect: "???"}}
			Expect(env.Client.Create(ctx, nodeClaim)).ToNot(Succeed())
		})
		It("should not fail for same key with different effects", func() {
			nodeClaim.Spec.Taints = []v1.Taint{
				{Key: "a", Effect: v1.TaintEffectNoSchedule},
				{Key: "a", Effect: v1.TaintEffectNoExecute},
			}
			Expect(env.Client.Create(ctx, nodeClaim)).To(Succeed())
		})
	})
	Context("Requirements", func() {
		It("should allow supported ops", func() {
			nodeClaim.Spec.Requirements = []NodeSelectorRequirementWithMinValues{
				{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpIn, Values: []string{"test"}}},
				{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpGt, Values: []string{"1"}}},
				{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpLt, Values: []string{"1"}}},
				{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpNotIn}},
				{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpExists}},
			}
			Expect(env.Client.Create(ctx, nodeClaim)).To(Succeed())
		})
		It("should fail for unsupported ops", func() {
			for _, op := range []v1.NodeSelectorOperator{"unknown"} {
				nodeClaim.Spec.Requirements = []NodeSelectorRequirementWithMinValues{
					{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: v1.LabelTopologyZone, Operator: op, Values: []string{"test"}}},
				}
				Expect(env.Client.Create(ctx, nodeClaim)).ToNot(Succeed())
			}
		})
		It("should fail for restricted domains", func() {
			for label := range RestrictedLabelDomains {
				nodeClaim.Spec.Requirements = []NodeSelectorRequirementWithMinValues{
					{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: label + "/test", Operator: v1.NodeSelectorOpIn, Values: []string{"test"}}},
				}
				Expect(env.Client.Create(ctx, nodeClaim)).ToNot(Succeed())
			}
		})
		It("should allow restricted domains exceptions", func() {
			oldNodeClaim := nodeClaim.DeepCopy()
			for label := range LabelDomainExceptions {
				nodeClaim.Spec.Requirements = []NodeSelectorRequirementWithMinValues{
					{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: label + "/test", Operator: v1.NodeSelectorOpIn, Values: []string{"test"}}},
				}
				Expect(env.Client.Create(ctx, nodeClaim)).To(Succeed())
				Expect(env.Client.Delete(ctx, nodeClaim)).To(Succeed())
				nodeClaim = oldNodeClaim.DeepCopy()
			}
		})
		It("should allow restricted subdomains exceptions", func() {
			oldNodeClaim := nodeClaim.DeepCopy()
			for label := range LabelDomainExceptions {
				nodeClaim.Spec.Requirements = []NodeSelectorRequirementWithMinValues{
					{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: "subdomain." + label + "/test", Operator: v1.NodeSelectorOpIn, Values: []string{"test"}}},
				}
				Expect(env.Client.Create(ctx, nodeClaim)).To(Succeed())
				Expect(env.Client.Delete(ctx, nodeClaim)).To(Succeed())
				nodeClaim = oldNodeClaim.DeepCopy()
			}
		})
		It("should allow well known label exceptions", func() {
			oldNodeClaim := nodeClaim.DeepCopy()
			for label := range WellKnownLabels.Difference(sets.New(NodePoolLabelKey)) {
				nodeClaim.Spec.Requirements = []NodeSelectorRequirementWithMinValues{
					{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: label, Operator: v1.NodeSelectorOpIn, Values: []string{"test"}}},
				}
				Expect(env.Client.Create(ctx, nodeClaim)).To(Succeed())
				Expect(env.Client.Delete(ctx, nodeClaim)).To(Succeed())
				nodeClaim = oldNodeClaim.DeepCopy()
			}
		})
		It("should allow non-empty set after removing overlapped value", func() {
			nodeClaim.Spec.Requirements = []NodeSelectorRequirementWithMinValues{
				{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpIn, Values: []string{"test", "foo"}}},
				{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpNotIn, Values: []string{"test", "bar"}}},
			}
			Expect(env.Client.Create(ctx, nodeClaim)).To(Succeed())
		})
		It("should allow empty requirements", func() {
			nodeClaim.Spec.Requirements = []NodeSelectorRequirementWithMinValues{}
			Expect(env.Client.Create(ctx, nodeClaim)).To(Succeed())
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
				nodeClaim.Spec.Requirements = []NodeSelectorRequirementWithMinValues{requirement}
				Expect(env.Client.Create(ctx, nodeClaim)).ToNot(Succeed())
			}
		})
		It("should error when minValues is negative", func() {
			nodeClaim.Spec.Requirements = []NodeSelectorRequirementWithMinValues{
				{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: v1.LabelInstanceTypeStable, Operator: v1.NodeSelectorOpIn, Values: []string{"insance-type-1"}}, MinValues: lo.ToPtr(-1)},
			}
			Expect(env.Client.Create(ctx, nodeClaim)).ToNot(Succeed())
		})
		It("should error when minValues is zero", func() {
			nodeClaim.Spec.Requirements = []NodeSelectorRequirementWithMinValues{
				{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: v1.LabelInstanceTypeStable, Operator: v1.NodeSelectorOpIn, Values: []string{"insance-type-1"}}, MinValues: lo.ToPtr(0)},
			}
			Expect(env.Client.Create(ctx, nodeClaim)).ToNot(Succeed())
		})
		It("should error when minValues is more than 50", func() {
			nodeClaim.Spec.Requirements = []NodeSelectorRequirementWithMinValues{
				{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: v1.LabelInstanceTypeStable, Operator: v1.NodeSelectorOpExists}, MinValues: lo.ToPtr(51)},
			}
			Expect(env.Client.Create(ctx, nodeClaim)).ToNot(Succeed())
		})
		It("should allow more than 50 values if minValues is not specified.", func() {
			var instanceTypes []string
			for i := 0; i < 90; i++ {
				instanceTypes = append(instanceTypes, "instance"+strconv.Itoa(i))
			}
			nodeClaim.Spec.Requirements = []NodeSelectorRequirementWithMinValues{
				{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: v1.LabelInstanceTypeStable, Operator: v1.NodeSelectorOpIn, Values: instanceTypes}},
			}
			Expect(env.Client.Create(ctx, nodeClaim)).To(Succeed())
		})
		It("should error when minValues is greater than the number of unique values specified within In operator", func() {
			nodeClaim.Spec.Requirements = []NodeSelectorRequirementWithMinValues{
				{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: v1.LabelInstanceTypeStable, Operator: v1.NodeSelectorOpIn, Values: []string{"insance-type-1"}}, MinValues: lo.ToPtr(2)},
			}
			Expect(nodeClaim.Validate(ctx)).ToNot(Succeed())
		})
	})
	Context("Kubelet", func() {
		It("should fail on kubeReserved with invalid keys", func() {
			nodeClaim.Spec.Kubelet = &KubeletConfiguration{
				KubeReserved: map[string]string{
					string(v1.ResourcePods): "2",
				},
			}
			Expect(env.Client.Create(ctx, nodeClaim)).ToNot(Succeed())
		})
		It("should fail on systemReserved with invalid keys", func() {
			nodeClaim.Spec.Kubelet = &KubeletConfiguration{
				SystemReserved: map[string]string{
					string(v1.ResourcePods): "2",
				},
			}
			Expect(env.Client.Create(ctx, nodeClaim)).ToNot(Succeed())
		})
		Context("Eviction Signals", func() {
			Context("Eviction Hard", func() {
				It("should succeed on evictionHard with valid keys", func() {
					nodeClaim.Spec.Kubelet = &KubeletConfiguration{
						EvictionHard: map[string]string{
							"memory.available":   "5%",
							"nodefs.available":   "10%",
							"nodefs.inodesFree":  "15%",
							"imagefs.available":  "5%",
							"imagefs.inodesFree": "5%",
							"pid.available":      "5%",
						},
					}
					Expect(env.Client.Create(ctx, nodeClaim)).To(Succeed())
				})
				It("should fail on evictionHard with invalid keys", func() {
					nodeClaim.Spec.Kubelet = &KubeletConfiguration{
						EvictionHard: map[string]string{
							"memory": "5%",
						},
					}
					Expect(env.Client.Create(ctx, nodeClaim)).ToNot(Succeed())
				})
				It("should fail on invalid formatted percentage value in evictionHard", func() {
					nodeClaim.Spec.Kubelet = &KubeletConfiguration{
						EvictionHard: map[string]string{
							"memory.available": "5%3",
						},
					}
					Expect(env.Client.Create(ctx, nodeClaim)).ToNot(Succeed())
				})
				It("should fail on invalid percentage value (too large) in evictionHard", func() {
					nodeClaim.Spec.Kubelet = &KubeletConfiguration{
						EvictionHard: map[string]string{
							"memory.available": "110%",
						},
					}
					Expect(env.Client.Create(ctx, nodeClaim)).ToNot(Succeed())
				})
				It("should fail on invalid quantity value in evictionHard", func() {
					nodeClaim.Spec.Kubelet = &KubeletConfiguration{
						EvictionHard: map[string]string{
							"memory.available": "110GB",
						},
					}
					Expect(env.Client.Create(ctx, nodeClaim)).ToNot(Succeed())
				})
			})
		})
		Context("Eviction Soft", func() {
			It("should succeed on evictionSoft with valid keys", func() {
				nodeClaim.Spec.Kubelet = &KubeletConfiguration{
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
				Expect(env.Client.Create(ctx, nodeClaim)).To(Succeed())
			})
			It("should fail on evictionSoft with invalid keys", func() {
				nodeClaim.Spec.Kubelet = &KubeletConfiguration{
					EvictionSoft: map[string]string{
						"memory": "5%",
					},
					EvictionSoftGracePeriod: map[string]metav1.Duration{
						"memory": {Duration: time.Minute},
					},
				}
				Expect(env.Client.Create(ctx, nodeClaim)).ToNot(Succeed())
			})
			It("should fail on invalid formatted percentage value in evictionSoft", func() {
				nodeClaim.Spec.Kubelet = &KubeletConfiguration{
					EvictionSoft: map[string]string{
						"memory.available": "5%3",
					},
					EvictionSoftGracePeriod: map[string]metav1.Duration{
						"memory.available": {Duration: time.Minute},
					},
				}
				Expect(env.Client.Create(ctx, nodeClaim)).ToNot(Succeed())
			})
			It("should fail on invalid percentage value (too large) in evictionSoft", func() {
				nodeClaim.Spec.Kubelet = &KubeletConfiguration{
					EvictionSoft: map[string]string{
						"memory.available": "110%",
					},
					EvictionSoftGracePeriod: map[string]metav1.Duration{
						"memory.available": {Duration: time.Minute},
					},
				}
				Expect(env.Client.Create(ctx, nodeClaim)).ToNot(Succeed())
			})
			It("should fail on invalid quantity value in evictionSoft", func() {
				nodeClaim.Spec.Kubelet = &KubeletConfiguration{
					EvictionSoft: map[string]string{
						"memory.available": "110GB",
					},
					EvictionSoftGracePeriod: map[string]metav1.Duration{
						"memory.available": {Duration: time.Minute},
					},
				}
				Expect(env.Client.Create(ctx, nodeClaim)).ToNot(Succeed())
			})
			It("should fail when eviction soft doesn't have matching grace period", func() {
				nodeClaim.Spec.Kubelet = &KubeletConfiguration{
					EvictionSoft: map[string]string{
						"memory.available": "200Mi",
					},
				}
				Expect(env.Client.Create(ctx, nodeClaim)).ToNot(Succeed())
			})
		})
		Context("GCThresholdPercent", func() {
			It("should succeed on a valid imageGCHighThresholdPercent", func() {
				nodeClaim.Spec.Kubelet = &KubeletConfiguration{
					ImageGCHighThresholdPercent: ptr.Int32(10),
				}
				Expect(env.Client.Create(ctx, nodeClaim)).To(Succeed())
			})
			It("should fail when imageGCHighThresholdPercent is less than imageGCLowThresholdPercent", func() {
				nodeClaim.Spec.Kubelet = &KubeletConfiguration{
					ImageGCHighThresholdPercent: ptr.Int32(50),
					ImageGCLowThresholdPercent:  ptr.Int32(60),
				}
				Expect(env.Client.Create(ctx, nodeClaim)).ToNot(Succeed())
			})
			It("should fail when imageGCLowThresholdPercent is greather than imageGCHighThresheldPercent", func() {
				nodeClaim.Spec.Kubelet = &KubeletConfiguration{
					ImageGCHighThresholdPercent: ptr.Int32(50),
					ImageGCLowThresholdPercent:  ptr.Int32(60),
				}
				Expect(env.Client.Create(ctx, nodeClaim)).ToNot(Succeed())
			})
		})
		Context("Eviction Soft Grace Period", func() {
			It("should succeed on evictionSoftGracePeriod with valid keys", func() {
				nodeClaim.Spec.Kubelet = &KubeletConfiguration{
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
				Expect(env.Client.Create(ctx, nodeClaim)).To(Succeed())
			})
			It("should fail on evictionSoftGracePeriod with invalid keys", func() {
				nodeClaim.Spec.Kubelet = &KubeletConfiguration{
					EvictionSoftGracePeriod: map[string]metav1.Duration{
						"memory": {Duration: time.Minute},
					},
				}
				Expect(env.Client.Create(ctx, nodeClaim)).ToNot(Succeed())
			})
			It("should fail when eviction soft grace period doesn't have matching threshold", func() {
				nodeClaim.Spec.Kubelet = &KubeletConfiguration{
					EvictionSoftGracePeriod: map[string]metav1.Duration{
						"memory.available": {Duration: time.Minute},
					},
				}
				Expect(env.Client.Create(ctx, nodeClaim)).ToNot(Succeed())
			})
		})
	})
})
