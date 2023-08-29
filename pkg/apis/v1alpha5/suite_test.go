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

package v1alpha5_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/Pallinder/go-randomdata"
	"github.com/mitchellh/hashstructure/v2"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	. "knative.dev/pkg/logging/testing"
	"knative.dev/pkg/ptr"

	. "github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/test"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
)

var ctx context.Context

func TestAPIs(t *testing.T) {
	ctx = TestContextWithLogger(t)
	RegisterFailHandler(Fail)
	RunSpecs(t, "Validation")
}

var _ = Describe("Validation", func() {
	var provisioner *Provisioner

	BeforeEach(func() {
		provisioner = &Provisioner{
			ObjectMeta: metav1.ObjectMeta{Name: strings.ToLower(randomdata.SillyName())},
			Spec: ProvisionerSpec{
				ProviderRef: &MachineTemplateRef{
					Kind: "NodeTemplate",
					Name: "default",
				},
			},
		}
	})

	It("should fail on negative expiry ttl", func() {
		provisioner.Spec.TTLSecondsUntilExpired = ptr.Int64(-1)
		Expect(provisioner.Validate(ctx)).ToNot(Succeed())
	})
	It("should succeed on a missing expiry ttl", func() {
		// this already is true, but to be explicit
		provisioner.Spec.TTLSecondsUntilExpired = nil
		Expect(provisioner.Validate(ctx)).To(Succeed())
	})
	It("should fail on negative empty ttl", func() {
		provisioner.Spec.TTLSecondsAfterEmpty = ptr.Int64(-1)
		Expect(provisioner.Validate(ctx)).ToNot(Succeed())
	})
	It("should succeed on a missing empty ttl", func() {
		provisioner.Spec.TTLSecondsAfterEmpty = nil
		Expect(provisioner.Validate(ctx)).To(Succeed())
	})
	It("should succeed on a valid empty ttl", func() {
		provisioner.Spec.TTLSecondsAfterEmpty = ptr.Int64(30)
		Expect(provisioner.Validate(ctx)).To(Succeed())
	})
	It("should fail if both consolidation and TTLSecondsAfterEmpty are enabled", func() {
		provisioner.Spec.TTLSecondsAfterEmpty = ptr.Int64(30)
		provisioner.Spec.Consolidation = &Consolidation{Enabled: ptr.Bool(true)}
		Expect(provisioner.Validate(ctx)).ToNot(Succeed())
	})
	It("should succeed if consolidation is off and TTLSecondsAfterEmpty is set", func() {
		provisioner.Spec.TTLSecondsAfterEmpty = ptr.Int64(30)
		provisioner.Spec.Consolidation = &Consolidation{Enabled: ptr.Bool(false)}
		Expect(provisioner.Validate(ctx)).To(Succeed())
	})
	It("should succeed if consolidation is on and TTLSecondsAfterEmpty is not set", func() {
		provisioner.Spec.TTLSecondsAfterEmpty = nil
		provisioner.Spec.Consolidation = &Consolidation{Enabled: ptr.Bool(true)}
		Expect(provisioner.Validate(ctx)).To(Succeed())
	})

	Context("Limits", func() {
		It("should allow undefined limits", func() {
			provisioner.Spec.Limits = &Limits{}
			Expect(provisioner.Validate(ctx)).To(Succeed())
		})
		It("should allow empty limits", func() {
			provisioner.Spec.Limits = &Limits{Resources: v1.ResourceList{}}
			Expect(provisioner.Validate(ctx)).To(Succeed())
		})
	})
	Context("Provider", func() {
		It("should not allow provider and providerRef", func() {
			provisioner.Spec.Provider = &Provider{}
			provisioner.Spec.ProviderRef = &MachineTemplateRef{Name: "providerRef"}
			Expect(provisioner.Validate(ctx)).ToNot(Succeed())
		})
		It("should require at least one of provider and providerRef", func() {
			provisioner.Spec.ProviderRef = nil
			Expect(provisioner.Validate(ctx)).ToNot(Succeed())
		})
	})
	Context("Labels", func() {
		It("should allow unrecognized labels", func() {
			provisioner.Spec.Labels = map[string]string{"foo": randomdata.SillyName()}
			Expect(provisioner.Validate(ctx)).To(Succeed())
		})
		It("should fail for the provisioner name label", func() {
			provisioner.Spec.Labels = map[string]string{ProvisionerNameLabelKey: randomdata.SillyName()}
			Expect(provisioner.Validate(ctx)).ToNot(Succeed())
		})
		It("should fail for invalid label keys", func() {
			provisioner.Spec.Labels = map[string]string{"spaces are not allowed": randomdata.SillyName()}
			Expect(provisioner.Validate(ctx)).ToNot(Succeed())
		})
		It("should fail for invalid label values", func() {
			provisioner.Spec.Labels = map[string]string{randomdata.SillyName(): "/ is not allowed"}
			Expect(provisioner.Validate(ctx)).ToNot(Succeed())
		})
		It("should fail for restricted label domains", func() {
			for label := range RestrictedLabelDomains {
				provisioner.Spec.Labels = map[string]string{label + "/unknown": randomdata.SillyName()}
				Expect(provisioner.Validate(ctx)).ToNot(Succeed())
			}
		})
		It("should allow labels kOps require", func() {
			provisioner.Spec.Labels = map[string]string{
				"kops.k8s.io/instancegroup": "karpenter-nodes",
				"kops.k8s.io/gpu":           "1",
			}
			Expect(provisioner.Validate(ctx)).To(Succeed())
		})
		It("should allow labels in restricted domains exceptions list", func() {
			for label := range LabelDomainExceptions {
				provisioner.Spec.Labels = map[string]string{
					label: "test-value",
				}
				Expect(provisioner.Validate(ctx)).To(Succeed())
			}
		})
		It("should allow labels prefixed with the restricted domain exceptions", func() {
			for label := range LabelDomainExceptions {
				provisioner.Spec.Labels = map[string]string{
					fmt.Sprintf("%s/key", label): "test-value",
				}
				Expect(provisioner.Validate(ctx)).To(Succeed())
			}
		})
	})
	Context("Taints", func() {
		It("should succeed for valid taints", func() {
			provisioner.Spec.Taints = []v1.Taint{
				{Key: "a", Value: "b", Effect: v1.TaintEffectNoSchedule},
				{Key: "c", Value: "d", Effect: v1.TaintEffectNoExecute},
				{Key: "e", Value: "f", Effect: v1.TaintEffectPreferNoSchedule},
				{Key: "key-only", Effect: v1.TaintEffectNoExecute},
			}
			Expect(provisioner.Validate(ctx)).To(Succeed())
		})
		It("should fail for invalid taint keys", func() {
			provisioner.Spec.Taints = []v1.Taint{{Key: "???"}}
			Expect(provisioner.Validate(ctx)).ToNot(Succeed())
		})
		It("should fail for missing taint key", func() {
			provisioner.Spec.Taints = []v1.Taint{{Effect: v1.TaintEffectNoSchedule}}
			Expect(provisioner.Validate(ctx)).ToNot(Succeed())
		})
		It("should fail for invalid taint value", func() {
			provisioner.Spec.Taints = []v1.Taint{{Key: "invalid-value", Effect: v1.TaintEffectNoSchedule, Value: "???"}}
			Expect(provisioner.Validate(ctx)).ToNot(Succeed())
		})
		It("should fail for invalid taint effect", func() {
			provisioner.Spec.Taints = []v1.Taint{{Key: "invalid-effect", Effect: "???"}}
			Expect(provisioner.Validate(ctx)).ToNot(Succeed())
		})
		It("should not fail for same key with different effects", func() {
			provisioner.Spec.Taints = []v1.Taint{
				{Key: "a", Effect: v1.TaintEffectNoSchedule},
				{Key: "a", Effect: v1.TaintEffectNoExecute},
			}
			Expect(provisioner.Validate(ctx)).To(Succeed())
		})
		It("should fail for duplicate taint key/effect pairs", func() {
			provisioner.Spec.Taints = []v1.Taint{
				{Key: "a", Effect: v1.TaintEffectNoSchedule},
				{Key: "a", Effect: v1.TaintEffectNoSchedule},
			}
			Expect(provisioner.Validate(ctx)).ToNot(Succeed())
			provisioner.Spec.Taints = []v1.Taint{
				{Key: "a", Effect: v1.TaintEffectNoSchedule},
			}
			provisioner.Spec.StartupTaints = []v1.Taint{
				{Key: "a", Effect: v1.TaintEffectNoSchedule},
			}
			Expect(provisioner.Validate(ctx)).ToNot(Succeed())
		})
	})
	Context("Requirements", func() {
		It("should fail for the provisioner name label", func() {
			provisioner.Spec.Requirements = []v1.NodeSelectorRequirement{
				{Key: ProvisionerNameLabelKey, Operator: v1.NodeSelectorOpIn, Values: []string{randomdata.SillyName()}},
			}
			Expect(provisioner.Validate(ctx)).ToNot(Succeed())
		})
		It("should allow supported ops", func() {
			provisioner.Spec.Requirements = []v1.NodeSelectorRequirement{
				{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpIn, Values: []string{"test"}},
				{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpGt, Values: []string{"1"}},
				{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpLt, Values: []string{"1"}},
				{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpNotIn},
				{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpExists},
			}
			Expect(provisioner.Validate(ctx)).To(Succeed())
		})
		It("should fail for unsupported ops", func() {
			for _, op := range []v1.NodeSelectorOperator{"unknown"} {
				provisioner.Spec.Requirements = []v1.NodeSelectorRequirement{
					{Key: v1.LabelTopologyZone, Operator: op, Values: []string{"test"}},
				}
				Expect(provisioner.Validate(ctx)).ToNot(Succeed())
			}
		})
		It("should fail for restricted domains", func() {
			for label := range RestrictedLabelDomains {
				provisioner.Spec.Requirements = []v1.NodeSelectorRequirement{
					{Key: label + "/test", Operator: v1.NodeSelectorOpIn, Values: []string{"test"}},
				}
				Expect(provisioner.Validate(ctx)).ToNot(Succeed())
			}
		})
		It("should allow restricted domains exceptions", func() {
			for label := range LabelDomainExceptions {
				provisioner.Spec.Requirements = []v1.NodeSelectorRequirement{
					{Key: label + "/test", Operator: v1.NodeSelectorOpIn, Values: []string{"test"}},
				}
				Expect(provisioner.Validate(ctx)).To(Succeed())
			}
		})
		It("should allow well known label exceptions", func() {
			for label := range WellKnownLabels.Difference(sets.New(ProvisionerNameLabelKey)) {
				provisioner.Spec.Requirements = []v1.NodeSelectorRequirement{
					{Key: label, Operator: v1.NodeSelectorOpIn, Values: []string{"test"}},
				}
				Expect(provisioner.Validate(ctx)).To(Succeed())
			}
		})
		It("should allow non-empty set after removing overlapped value", func() {
			provisioner.Spec.Requirements = []v1.NodeSelectorRequirement{
				{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpIn, Values: []string{"test", "foo"}},
				{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpNotIn, Values: []string{"test", "bar"}},
			}
			Expect(provisioner.Validate(ctx)).To(Succeed())
		})
		It("should allow empty requirements", func() {
			provisioner.Spec.Requirements = []v1.NodeSelectorRequirement{}
			Expect(provisioner.Validate(ctx)).To(Succeed())
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
				provisioner.Spec.Requirements = []v1.NodeSelectorRequirement{requirement}
				Expect(provisioner.Validate(ctx)).ToNot(Succeed())
			}
		})
	})
	Context("KubeletConfiguration", func() {
		It("should fail on kubeReserved with invalid keys", func() {
			provisioner.Spec.KubeletConfiguration = &KubeletConfiguration{
				KubeReserved: v1.ResourceList{
					v1.ResourcePods: resource.MustParse("2"),
				},
			}
			Expect(provisioner.Validate(ctx)).ToNot(Succeed())
		})
		It("should fail on systemReserved with invalid keys", func() {
			provisioner.Spec.KubeletConfiguration = &KubeletConfiguration{
				SystemReserved: v1.ResourceList{
					v1.ResourcePods: resource.MustParse("2"),
				},
			}
			Expect(provisioner.Validate(ctx)).ToNot(Succeed())
		})
		Context("Eviction Signals", func() {
			Context("Eviction Hard", func() {
				It("should succeed on evictionHard with valid keys", func() {
					provisioner.Spec.KubeletConfiguration = &KubeletConfiguration{
						EvictionHard: map[string]string{
							"memory.available":   "5%",
							"nodefs.available":   "10%",
							"nodefs.inodesFree":  "15%",
							"imagefs.available":  "5%",
							"imagefs.inodesFree": "5%",
							"pid.available":      "5%",
						},
					}
					Expect(provisioner.Validate(ctx)).To(Succeed())
				})
				It("should fail on evictionHard with invalid keys", func() {
					provisioner.Spec.KubeletConfiguration = &KubeletConfiguration{
						EvictionHard: map[string]string{
							"memory": "5%",
						},
					}
					Expect(provisioner.Validate(ctx)).ToNot(Succeed())
				})
				It("should fail on invalid formatted percentage value in evictionHard", func() {
					provisioner.Spec.KubeletConfiguration = &KubeletConfiguration{
						EvictionHard: map[string]string{
							"memory.available": "5%3",
						},
					}
					Expect(provisioner.Validate(ctx)).ToNot(Succeed())
				})
				It("should fail on invalid percentage value (too large) in evictionHard", func() {
					provisioner.Spec.KubeletConfiguration = &KubeletConfiguration{
						EvictionHard: map[string]string{
							"memory.available": "110%",
						},
					}
					Expect(provisioner.Validate(ctx)).ToNot(Succeed())
				})
				It("should fail on invalid quantity value in evictionHard", func() {
					provisioner.Spec.KubeletConfiguration = &KubeletConfiguration{
						EvictionHard: map[string]string{
							"memory.available": "110GB",
						},
					}
					Expect(provisioner.Validate(ctx)).ToNot(Succeed())
				})
			})
		})
		Context("Eviction Soft", func() {
			It("should succeed on evictionSoft with valid keys", func() {
				provisioner.Spec.KubeletConfiguration = &KubeletConfiguration{
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
				Expect(provisioner.Validate(ctx)).To(Succeed())
			})
			It("should fail on evictionSoft with invalid keys", func() {
				provisioner.Spec.KubeletConfiguration = &KubeletConfiguration{
					EvictionSoft: map[string]string{
						"memory": "5%",
					},
					EvictionSoftGracePeriod: map[string]metav1.Duration{
						"memory": {Duration: time.Minute},
					},
				}
				Expect(provisioner.Validate(ctx)).ToNot(Succeed())
			})
			It("should fail on invalid formatted percentage value in evictionSoft", func() {
				provisioner.Spec.KubeletConfiguration = &KubeletConfiguration{
					EvictionSoft: map[string]string{
						"memory.available": "5%3",
					},
					EvictionSoftGracePeriod: map[string]metav1.Duration{
						"memory.available": {Duration: time.Minute},
					},
				}
				Expect(provisioner.Validate(ctx)).ToNot(Succeed())
			})
			It("should fail on invalid percentage value (too large) in evictionSoft", func() {
				provisioner.Spec.KubeletConfiguration = &KubeletConfiguration{
					EvictionSoft: map[string]string{
						"memory.available": "110%",
					},
					EvictionSoftGracePeriod: map[string]metav1.Duration{
						"memory.available": {Duration: time.Minute},
					},
				}
				Expect(provisioner.Validate(ctx)).ToNot(Succeed())
			})
			It("should fail on invalid quantity value in evictionSoft", func() {
				provisioner.Spec.KubeletConfiguration = &KubeletConfiguration{
					EvictionSoft: map[string]string{
						"memory.available": "110GB",
					},
					EvictionSoftGracePeriod: map[string]metav1.Duration{
						"memory.available": {Duration: time.Minute},
					},
				}
				Expect(provisioner.Validate(ctx)).ToNot(Succeed())
			})
			It("should fail when eviction soft doesn't have matching grace period", func() {
				provisioner.Spec.KubeletConfiguration = &KubeletConfiguration{
					EvictionSoft: map[string]string{
						"memory.available": "200Mi",
					},
				}
				Expect(provisioner.Validate(ctx)).ToNot(Succeed())
			})
		})
		Context("GCThresholdPercent", func() {
			Context("ImageGCHighThresholdPercent", func() {
				It("should succeed on a imageGCHighThresholdPercent", func() {
					provisioner.Spec.KubeletConfiguration = &KubeletConfiguration{
						ImageGCHighThresholdPercent: ptr.Int32(10),
					}
					Expect(provisioner.Validate(ctx)).To(Succeed())
				})
				It("should fail when imageGCHighThresholdPercent is less than imageGCLowThresholdPercent", func() {
					provisioner.Spec.KubeletConfiguration = &KubeletConfiguration{
						ImageGCHighThresholdPercent: ptr.Int32(50),
						ImageGCLowThresholdPercent:  ptr.Int32(60),
					}
					Expect(provisioner.Validate(ctx)).ToNot(Succeed())
				})
			})
			Context("ImageGCLowThresholdPercent", func() {
				It("should succeed on a imageGCLowThresholdPercent", func() {
					provisioner.Spec.KubeletConfiguration = &KubeletConfiguration{
						ImageGCLowThresholdPercent: ptr.Int32(10),
					}
					Expect(provisioner.Validate(ctx)).To(Succeed())
				})
				It("should fail when imageGCLowThresholdPercent is greather than imageGCHighThresheldPercent", func() {
					provisioner.Spec.KubeletConfiguration = &KubeletConfiguration{
						ImageGCHighThresholdPercent: ptr.Int32(50),
						ImageGCLowThresholdPercent:  ptr.Int32(60),
					}
					Expect(provisioner.Validate(ctx)).ToNot(Succeed())
				})
			})
		})
		Context("Eviction Soft Grace Period", func() {
			It("should succeed on evictionSoftGracePeriod with valid keys", func() {
				provisioner.Spec.KubeletConfiguration = &KubeletConfiguration{
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
				Expect(provisioner.Validate(ctx)).To(Succeed())
			})
			It("should fail on evictionSoftGracePeriod with invalid keys", func() {
				provisioner.Spec.KubeletConfiguration = &KubeletConfiguration{
					EvictionSoftGracePeriod: map[string]metav1.Duration{
						"memory": {Duration: time.Minute},
					},
				}
				Expect(provisioner.Validate(ctx)).ToNot(Succeed())
			})
			It("should fail when eviction soft grace period doesn't have matching threshold", func() {
				provisioner.Spec.KubeletConfiguration = &KubeletConfiguration{
					EvictionSoftGracePeriod: map[string]metav1.Duration{
						"memory.available": {Duration: time.Minute},
					},
				}
				Expect(provisioner.Validate(ctx)).ToNot(Succeed())
			})
		})
	})
})

var _ = Describe("Limits", func() {
	var provisioner *Provisioner

	BeforeEach(func() {
		provisioner = &Provisioner{
			ObjectMeta: metav1.ObjectMeta{Name: strings.ToLower(randomdata.SillyName())},
			Spec: ProvisionerSpec{
				Limits: &Limits{
					Resources: v1.ResourceList{
						"cpu": resource.MustParse("16"),
					},
				},
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

var _ = Describe("Provisioner Annotation", func() {
	var testProvisionerOptions test.ProvisionerOptions
	var provisioner *Provisioner
	BeforeEach(func() {
		taints := []v1.Taint{
			{
				Key:    "keyValue1",
				Effect: v1.TaintEffectNoExecute,
			},
			{
				Key:    "keyValue2",
				Effect: v1.TaintEffectNoExecute,
			},
		}
		testProvisionerOptions = test.ProvisionerOptions{
			Taints:        taints,
			StartupTaints: taints,
			Labels: map[string]string{
				"keyLabel":  "valueLabel",
				"keyLabel2": "valueLabel2",
			},
			Kubelet: &KubeletConfiguration{
				MaxPods: ptr.Int32(10),
			},
			Annotations: map[string]string{
				"keyAnnotation":  "valueAnnotation",
				"keyAnnotation2": "valueAnnotation2",
			},
		}
		provisioner = test.Provisioner(testProvisionerOptions)
	})
	It("should match previous static hashes", func() {
		provisioners := []struct {
			provisioner *Provisioner
			hash        string
		}{
			{
				provisioner: provisioner,
				hash:        "14114424411830460479",
			},
			{
				provisioner: test.Provisioner(testProvisionerOptions, test.ProvisionerOptions{Annotations: map[string]string{"keyAnnotationTest": "valueAnnotationTest"}}),
				hash:        "7374986726887162519",
			},
			{
				provisioner: test.Provisioner(testProvisionerOptions, test.ProvisionerOptions{Labels: map[string]string{"keyLabelTest": "valueLabelTest"}}),
				hash:        "9065364558915106368",
			},
			{
				provisioner: test.Provisioner(testProvisionerOptions, test.ProvisionerOptions{Taints: []v1.Taint{{Key: "keytest2Taint", Effect: v1.TaintEffectNoExecute}}}),
				hash:        "9081458816929490897",
			},
			{
				provisioner: test.Provisioner(testProvisionerOptions, test.ProvisionerOptions{StartupTaints: []v1.Taint{{Key: "keytest2StartupTaint", Effect: v1.TaintEffectNoExecute}}}),
				hash:        "2352640223763896447",
			},
		}

		for _, p := range provisioners {
			hash := p.provisioner.Hash()
			Expect(hash).To(Equal(p.hash))
		}
	})
	It("should change hash when static fields are updated", func() {
		expectedHash := provisioner.Hash()

		// Change one static field for 5 provisioners
		provisionerFieldToChange := []*Provisioner{
			test.Provisioner(testProvisionerOptions, test.ProvisionerOptions{Annotations: map[string]string{"keyAnnotationTest": "valueAnnotationTest"}}),
			test.Provisioner(testProvisionerOptions, test.ProvisionerOptions{Labels: map[string]string{"keyLabelTest": "valueLabelTest"}}),
			test.Provisioner(testProvisionerOptions, test.ProvisionerOptions{Taints: []v1.Taint{{Key: "keytest2Taint", Effect: v1.TaintEffectNoExecute}}}),
			test.Provisioner(testProvisionerOptions, test.ProvisionerOptions{StartupTaints: []v1.Taint{{Key: "keytest2StartupTaint", Effect: v1.TaintEffectNoExecute}}}),
			test.Provisioner(testProvisionerOptions, test.ProvisionerOptions{Kubelet: &KubeletConfiguration{MaxPods: ptr.Int32(30)}}),
		}

		for _, updatedProvisioner := range provisionerFieldToChange {
			actualHash := updatedProvisioner.Hash()
			Expect(actualHash).ToNot(Equal(fmt.Sprint(expectedHash)))
		}
	})
	It("should not change hash when behavior fields are updated", func() {
		actualHash := provisioner.Hash()

		expectedHash, err := hashstructure.Hash(provisioner.Spec, hashstructure.FormatV2, &hashstructure.HashOptions{
			SlicesAsSets:    true,
			IgnoreZeroValue: true,
			ZeroNil:         true,
		})
		Expect(err).ToNot(HaveOccurred())
		Expect(actualHash).To(Equal(fmt.Sprint(expectedHash)))

		// Update a behavior field
		provisioner.Spec.Limits = &Limits{Resources: v1.ResourceList{"cpu": resource.MustParse("16")}}
		provisioner.Spec.Consolidation = &Consolidation{Enabled: lo.ToPtr(true)}
		provisioner.Spec.TTLSecondsAfterEmpty = lo.ToPtr(int64(30))
		provisioner.Spec.TTLSecondsUntilExpired = lo.ToPtr(int64(50))
		provisioner.Spec.Requirements = []v1.NodeSelectorRequirement{
			{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpIn, Values: []string{"test"}},
			{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpGt, Values: []string{"1"}},
			{Key: v1.LabelTopologyZone, Operator: v1.NodeSelectorOpLt, Values: []string{"1"}},
		}
		provisioner.Spec.Weight = lo.ToPtr(int32(80))

		actualHash = provisioner.Hash()
		Expect(err).ToNot(HaveOccurred())
		Expect(actualHash).To(Equal(fmt.Sprint(expectedHash)))
	})
	It("should expect two provisioner with the same spec to have the same provisioner hash", func() {
		provisionerTwo := &Provisioner{
			ObjectMeta: metav1.ObjectMeta{Name: strings.ToLower(randomdata.SillyName())},
		}
		provisionerTwo.Spec = provisioner.Spec

		Expect(provisioner.Hash()).To(Equal(provisionerTwo.Hash()))
	})
	It("should expect hashes that are reordered to not produce a new hash", func() {
		expectedHash := provisioner.Hash()
		updatedTaints := []v1.Taint{
			{
				Key:    "keyValue2",
				Effect: v1.TaintEffectNoExecute,
			},
			{
				Key:    "keyValue1",
				Effect: v1.TaintEffectNoExecute,
			},
		}

		provisionerFieldToChange := []*Provisioner{
			test.Provisioner(testProvisionerOptions, test.ProvisionerOptions{Annotations: map[string]string{"keyAnnotation2": "valueAnnotation2", "keyAnnotation": "valueAnnotation"}}),
			test.Provisioner(testProvisionerOptions, test.ProvisionerOptions{Taints: updatedTaints}),
			test.Provisioner(testProvisionerOptions, test.ProvisionerOptions{StartupTaints: updatedTaints}),
		}

		for _, updatedProvisioner := range provisionerFieldToChange {
			actualHash := updatedProvisioner.Hash()
			Expect(actualHash).To(Equal(fmt.Sprint(expectedHash)))
		}
	})
})
