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

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/Pallinder/go-randomdata"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"knative.dev/pkg/ptr"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
)

var _ = Describe("Validation", func() {
	var nodeClaim *NodeClaim

	BeforeEach(func() {
		nodeClaim = &NodeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: strings.ToLower(randomdata.SillyName())},
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
			Expect(nodeClaim.Validate(ctx)).To(Succeed())
		})
		It("should fail for invalid taint keys", func() {
			nodeClaim.Spec.Taints = []v1.Taint{{Key: "???"}}
			Expect(nodeClaim.Validate(ctx)).ToNot(Succeed())
		})
		It("should fail for missing taint key", func() {
			nodeClaim.Spec.Taints = []v1.Taint{{Effect: v1.TaintEffectNoSchedule}}
			Expect(nodeClaim.Validate(ctx)).ToNot(Succeed())
		})
		It("should fail for invalid taint value", func() {
			nodeClaim.Spec.Taints = []v1.Taint{{Key: "invalid-value", Effect: v1.TaintEffectNoSchedule, Value: "???"}}
			Expect(nodeClaim.Validate(ctx)).ToNot(Succeed())
		})
		It("should fail for invalid taint effect", func() {
			nodeClaim.Spec.Taints = []v1.Taint{{Key: "invalid-effect", Effect: "???"}}
			Expect(nodeClaim.Validate(ctx)).ToNot(Succeed())
		})
		It("should not fail for same key with different effects", func() {
			nodeClaim.Spec.Taints = []v1.Taint{
				{Key: "a", Effect: v1.TaintEffectNoSchedule},
				{Key: "a", Effect: v1.TaintEffectNoExecute},
			}
			Expect(nodeClaim.Validate(ctx)).To(Succeed())
		})
		It("should fail for duplicate taint key/effect pairs", func() {
			nodeClaim.Spec.Taints = []v1.Taint{
				{Key: "a", Effect: v1.TaintEffectNoSchedule},
				{Key: "a", Effect: v1.TaintEffectNoSchedule},
			}
			Expect(nodeClaim.Validate(ctx)).ToNot(Succeed())
			nodeClaim.Spec.Taints = []v1.Taint{
				{Key: "a", Effect: v1.TaintEffectNoSchedule},
			}
			nodeClaim.Spec.StartupTaints = []v1.Taint{
				{Key: "a", Effect: v1.TaintEffectNoSchedule},
			}
			Expect(nodeClaim.Validate(ctx)).ToNot(Succeed())
		})
	})
	Context("Requirements", func() {
		It("should fail for the karpenter.sh/nodepool label", func() {
			nodeClaim.Spec.Requirements = []v1.NodeSelectorRequirement{
				{Key: NodePoolLabelKey, Operator: v1.NodeSelectorOpIn, Values: []string{randomdata.SillyName()}},
			}
			Expect(nodeClaim.Validate(ctx)).ToNot(Succeed())
		})
		It("should allow supported ops", func() {
			nodeClaim.Spec.Requirements = []v1.NodeSelectorRequirement{
				{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpIn, Values: []string{"test"}},
				{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpGt, Values: []string{"1"}},
				{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpLt, Values: []string{"1"}},
				{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpNotIn},
				{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpExists},
			}
			Expect(nodeClaim.Validate(ctx)).To(Succeed())
		})
		It("should fail for unsupported ops", func() {
			for _, op := range []v1.NodeSelectorOperator{"unknown"} {
				nodeClaim.Spec.Requirements = []v1.NodeSelectorRequirement{
					{Key: v1.LabelTopologyZone, Operator: op, Values: []string{"test"}},
				}
				Expect(nodeClaim.Validate(ctx)).ToNot(Succeed())
			}
		})
		It("should fail for restricted domains", func() {
			for label := range RestrictedLabelDomains {
				nodeClaim.Spec.Requirements = []v1.NodeSelectorRequirement{
					{Key: label + "/test", Operator: v1.NodeSelectorOpIn, Values: []string{"test"}},
				}
				Expect(nodeClaim.Validate(ctx)).ToNot(Succeed())
			}
		})
		It("should allow restricted domains exceptions", func() {
			for label := range LabelDomainExceptions {
				nodeClaim.Spec.Requirements = []v1.NodeSelectorRequirement{
					{Key: label + "/test", Operator: v1.NodeSelectorOpIn, Values: []string{"test"}},
				}
				Expect(nodeClaim.Validate(ctx)).To(Succeed())
			}
		})
		It("should allow well known label exceptions", func() {
			for label := range WellKnownLabels.Difference(sets.New(NodePoolLabelKey)) {
				nodeClaim.Spec.Requirements = []v1.NodeSelectorRequirement{
					{Key: label, Operator: v1.NodeSelectorOpIn, Values: []string{"test"}},
				}
				Expect(nodeClaim.Validate(ctx)).To(Succeed())
			}
		})
		It("should allow non-empty set after removing overlapped value", func() {
			nodeClaim.Spec.Requirements = []v1.NodeSelectorRequirement{
				{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpIn, Values: []string{"test", "foo"}},
				{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpNotIn, Values: []string{"test", "bar"}},
			}
			Expect(nodeClaim.Validate(ctx)).To(Succeed())
		})
		It("should allow empty requirements", func() {
			nodeClaim.Spec.Requirements = []v1.NodeSelectorRequirement{}
			Expect(nodeClaim.Validate(ctx)).To(Succeed())
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
				nodeClaim.Spec.Requirements = []v1.NodeSelectorRequirement{requirement}
				Expect(nodeClaim.Validate(ctx)).ToNot(Succeed())
			}
		})
	})
	Context("Kubelet", func() {
		It("should fail on kubeReserved with invalid keys", func() {
			nodeClaim.Spec.Kubelet = &KubeletConfiguration{
				KubeReserved: v1.ResourceList{
					v1.ResourcePods: resource.MustParse("2"),
				},
			}
			Expect(nodeClaim.Validate(ctx)).ToNot(Succeed())
		})
		It("should fail on systemReserved with invalid keys", func() {
			nodeClaim.Spec.Kubelet = &KubeletConfiguration{
				SystemReserved: v1.ResourceList{
					v1.ResourcePods: resource.MustParse("2"),
				},
			}
			Expect(nodeClaim.Validate(ctx)).ToNot(Succeed())
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
					Expect(nodeClaim.Validate(ctx)).To(Succeed())
				})
				It("should fail on evictionHard with invalid keys", func() {
					nodeClaim.Spec.Kubelet = &KubeletConfiguration{
						EvictionHard: map[string]string{
							"memory": "5%",
						},
					}
					Expect(nodeClaim.Validate(ctx)).ToNot(Succeed())
				})
				It("should fail on invalid formatted percentage value in evictionHard", func() {
					nodeClaim.Spec.Kubelet = &KubeletConfiguration{
						EvictionHard: map[string]string{
							"memory.available": "5%3",
						},
					}
					Expect(nodeClaim.Validate(ctx)).ToNot(Succeed())
				})
				It("should fail on invalid percentage value (too large) in evictionHard", func() {
					nodeClaim.Spec.Kubelet = &KubeletConfiguration{
						EvictionHard: map[string]string{
							"memory.available": "110%",
						},
					}
					Expect(nodeClaim.Validate(ctx)).ToNot(Succeed())
				})
				It("should fail on invalid quantity value in evictionHard", func() {
					nodeClaim.Spec.Kubelet = &KubeletConfiguration{
						EvictionHard: map[string]string{
							"memory.available": "110GB",
						},
					}
					Expect(nodeClaim.Validate(ctx)).ToNot(Succeed())
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
				Expect(nodeClaim.Validate(ctx)).To(Succeed())
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
				Expect(nodeClaim.Validate(ctx)).ToNot(Succeed())
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
				Expect(nodeClaim.Validate(ctx)).ToNot(Succeed())
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
				Expect(nodeClaim.Validate(ctx)).ToNot(Succeed())
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
				Expect(nodeClaim.Validate(ctx)).ToNot(Succeed())
			})
			It("should fail when eviction soft doesn't have matching grace period", func() {
				nodeClaim.Spec.Kubelet = &KubeletConfiguration{
					EvictionSoft: map[string]string{
						"memory.available": "200Mi",
					},
				}
				Expect(nodeClaim.Validate(ctx)).ToNot(Succeed())
			})
		})
		Context("GCThresholdPercent", func() {
			Context("ImageGCHighThresholdPercent", func() {
				It("should succeed on a imageGCHighThresholdPercent", func() {
					nodeClaim.Spec.Kubelet = &KubeletConfiguration{
						ImageGCHighThresholdPercent: ptr.Int32(10),
					}
					Expect(nodeClaim.Validate(ctx)).To(Succeed())
				})
				It("should fail when imageGCHighThresholdPercent is less than imageGCLowThresholdPercent", func() {
					nodeClaim.Spec.Kubelet = &KubeletConfiguration{
						ImageGCHighThresholdPercent: ptr.Int32(50),
						ImageGCLowThresholdPercent:  ptr.Int32(60),
					}
					Expect(nodeClaim.Validate(ctx)).ToNot(Succeed())
				})
			})
			Context("ImageGCLowThresholdPercent", func() {
				It("should succeed on a imageGCLowThresholdPercent", func() {
					nodeClaim.Spec.Kubelet = &KubeletConfiguration{
						ImageGCLowThresholdPercent: ptr.Int32(10),
					}
					Expect(nodeClaim.Validate(ctx)).To(Succeed())
				})
				It("should fail when imageGCLowThresholdPercent is greather than imageGCHighThresheldPercent", func() {
					nodeClaim.Spec.Kubelet = &KubeletConfiguration{
						ImageGCHighThresholdPercent: ptr.Int32(50),
						ImageGCLowThresholdPercent:  ptr.Int32(60),
					}
					Expect(nodeClaim.Validate(ctx)).ToNot(Succeed())
				})
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
				Expect(nodeClaim.Validate(ctx)).To(Succeed())
			})
			It("should fail on evictionSoftGracePeriod with invalid keys", func() {
				nodeClaim.Spec.Kubelet = &KubeletConfiguration{
					EvictionSoftGracePeriod: map[string]metav1.Duration{
						"memory": {Duration: time.Minute},
					},
				}
				Expect(nodeClaim.Validate(ctx)).ToNot(Succeed())
			})
			It("should fail when eviction soft grace period doesn't have matching threshold", func() {
				nodeClaim.Spec.Kubelet = &KubeletConfiguration{
					EvictionSoftGracePeriod: map[string]metav1.Duration{
						"memory.available": {Duration: time.Minute},
					},
				}
				Expect(nodeClaim.Validate(ctx)).ToNot(Succeed())
			})
		})
	})
})
