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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	. "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/apis/v1beta1"
	"sigs.k8s.io/karpenter/pkg/operator/injection"
)

var v1NodePool *NodePool
var v1beta1NodePool *v1beta1.NodePool

var _ = Describe("Convert V1 to V1beta1 NodePool API", func() {
	BeforeEach(func() {
		v1NodePool = &NodePool{}
		v1beta1NodePool = &v1beta1.NodePool{}
		cloudProvider.NodeClassGroupVersionKind = []schema.GroupVersionKind{
			{
				Group:   "fake-cloudprovider-group",
				Version: "fake-cloudprovider-version",
				Kind:    "fake-cloudprovider-kind",
			},
		}
		ctx = injection.WithNodeClasses(ctx, cloudProvider.GetSupportedNodeClasses())
	})

	Context("MetaData", func() {
		It("should convert v1 nodepool Name", func() {
			v1NodePool.Name = "test-name-v1"
			Expect(v1beta1NodePool.Name).To(BeEmpty())
			Expect(v1NodePool.ConvertTo(ctx, v1beta1NodePool)).To(BeNil())
			Expect(v1beta1NodePool.Name).To(Equal(v1NodePool.Name))
		})
		It("should convert v1 nodepool UID", func() {
			v1NodePool.UID = types.UID("test-name-v1")
			Expect(v1beta1NodePool.UID).To(BeEmpty())
			Expect(v1NodePool.ConvertTo(ctx, v1beta1NodePool)).To(BeNil())
			Expect(v1beta1NodePool.UID).To(Equal(v1NodePool.UID))
		})
	})
	Context("NodePool Spec", func() {
		It("should convert v1 nodepool weights", func() {
			v1NodePool.Spec.Weight = lo.ToPtr(int32(62))
			Expect(lo.FromPtr(v1beta1NodePool.Spec.Weight)).To(Equal(int32(0)))
			Expect(v1NodePool.ConvertTo(ctx, v1beta1NodePool)).To(BeNil())
			Expect(lo.FromPtr(v1beta1NodePool.Spec.Weight)).To(Equal(int32(62)))
		})
		It("should convert v1 nodepool limits", func() {
			v1NodePool.Spec.Limits = Limits{
				v1.ResourceCPU:    resource.MustParse("5"),
				v1.ResourceMemory: resource.MustParse("14145G"),
			}
			Expect(len(lo.Keys(v1beta1NodePool.Spec.Limits))).To(BeNumerically("==", 0))
			Expect(v1NodePool.ConvertTo(ctx, v1beta1NodePool)).To(BeNil())
			for _, resource := range lo.Keys(v1NodePool.Spec.Limits) {
				Expect(v1beta1NodePool.Spec.Limits[resource]).To(Equal(v1NodePool.Spec.Limits[resource]))
			}
		})
		Context("NodeClaimTemplate", func() {
			It("should convert v1 nodepool template labels", func() {
				v1NodePool.Spec.Template.Labels = map[string]string{
					"test-key-1": "test-value-1",
					"test-key-2": "test-value-2",
				}
				Expect(v1beta1NodePool.Spec.Template.Labels).To(BeNil())
				Expect(v1NodePool.ConvertTo(ctx, v1beta1NodePool)).To(BeNil())
				for key := range v1NodePool.Spec.Template.Labels {
					Expect(v1beta1NodePool.Spec.Template.Labels[key]).To(Equal(v1NodePool.Spec.Template.Labels[key]))
				}
			})
			It("should convert v1 nodepool template annotations", func() {
				v1NodePool.Spec.Template.Annotations = map[string]string{
					"test-key-1": "test-value-1",
					"test-key-2": "test-value-2",
				}
				Expect(v1beta1NodePool.Spec.Template.Annotations).To(BeNil())
				Expect(v1NodePool.ConvertTo(ctx, v1beta1NodePool)).To(BeNil())
				for key := range v1NodePool.Spec.Template.Annotations {
					Expect(v1beta1NodePool.Spec.Template.Annotations[key]).To(Equal(v1NodePool.Spec.Template.Annotations[key]))
				}
			})
			It("should convert v1 nodepool template taints", func() {
				v1NodePool.Spec.Template.Spec.Taints = []v1.Taint{
					{
						Key:    "test-key-1",
						Value:  "test-value-1",
						Effect: v1.TaintEffectNoExecute,
					},
					{
						Key:    "test-key-2",
						Value:  "test-value-2",
						Effect: v1.TaintEffectNoSchedule,
					},
				}
				Expect(len(v1beta1NodePool.Spec.Template.Spec.Taints)).To(BeNumerically("==", 0))
				Expect(v1NodePool.ConvertTo(ctx, v1beta1NodePool)).To(BeNil())
				for i := range v1NodePool.Spec.Template.Spec.Taints {
					Expect(v1beta1NodePool.Spec.Template.Spec.Taints[i].Key).To(Equal(v1NodePool.Spec.Template.Spec.Taints[i].Key))
					Expect(v1beta1NodePool.Spec.Template.Spec.Taints[i].Value).To(Equal(v1NodePool.Spec.Template.Spec.Taints[i].Value))
					Expect(v1beta1NodePool.Spec.Template.Spec.Taints[i].Effect).To(Equal(v1NodePool.Spec.Template.Spec.Taints[i].Effect))
				}
			})
			It("should convert v1 nodepool template startup taints", func() {
				v1NodePool.Spec.Template.Spec.StartupTaints = []v1.Taint{
					{
						Key:    "test-key--startup-1",
						Value:  "test-value-startup-1",
						Effect: v1.TaintEffectNoExecute,
					},
					{
						Key:    "test-key-startup-2",
						Value:  "test-value-startup-2",
						Effect: v1.TaintEffectNoSchedule,
					},
				}
				Expect(len(v1beta1NodePool.Spec.Template.Spec.StartupTaints)).To(BeNumerically("==", 0))
				Expect(v1NodePool.ConvertTo(ctx, v1beta1NodePool)).To(BeNil())
				for i := range v1NodePool.Spec.Template.Spec.StartupTaints {
					Expect(v1beta1NodePool.Spec.Template.Spec.StartupTaints[i].Key).To(Equal(v1NodePool.Spec.Template.Spec.StartupTaints[i].Key))
					Expect(v1beta1NodePool.Spec.Template.Spec.StartupTaints[i].Value).To(Equal(v1NodePool.Spec.Template.Spec.StartupTaints[i].Value))
					Expect(v1beta1NodePool.Spec.Template.Spec.StartupTaints[i].Effect).To(Equal(v1NodePool.Spec.Template.Spec.StartupTaints[i].Effect))
				}
			})
			It("should convert v1 nodepool template requirements", func() {
				v1NodePool.Spec.Template.Spec.Requirements = []NodeSelectorRequirementWithMinValues{
					{
						NodeSelectorRequirement: v1.NodeSelectorRequirement{
							Key:      v1.LabelArchStable,
							Operator: v1.NodeSelectorOpExists,
						},
						MinValues: lo.ToPtr(433234),
					},
					{
						NodeSelectorRequirement: v1.NodeSelectorRequirement{
							Key:      CapacityTypeLabelKey,
							Operator: v1.NodeSelectorOpIn,
							Values:   []string{CapacityTypeSpot},
						},
						MinValues: lo.ToPtr(65765),
					},
				}
				Expect(len(v1beta1NodePool.Spec.Template.Spec.Requirements)).To(BeNumerically("==", 0))
				Expect(v1NodePool.ConvertTo(ctx, v1beta1NodePool)).To(BeNil())
				for i := range v1NodePool.Spec.Template.Spec.Requirements {
					Expect(v1beta1NodePool.Spec.Template.Spec.Requirements[i].Key).To(Equal(v1NodePool.Spec.Template.Spec.Requirements[i].Key))
					Expect(v1beta1NodePool.Spec.Template.Spec.Requirements[i].Operator).To(Equal(v1NodePool.Spec.Template.Spec.Requirements[i].Operator))
					Expect(v1beta1NodePool.Spec.Template.Spec.Requirements[i].Values).To(Equal(v1NodePool.Spec.Template.Spec.Requirements[i].Values))
					Expect(v1beta1NodePool.Spec.Template.Spec.Requirements[i].MinValues).To(Equal(v1NodePool.Spec.Template.Spec.Requirements[i].MinValues))
				}
			})
			Context("NodeClassRef", func() {
				It("should convert v1 nodepool template nodeClassRef", func() {
					v1NodePool.Spec.Template.Spec.NodeClassRef = &NodeClassReference{
						Kind:  "fake-cloudprovider-kind",
						Name:  "nodeclass-test",
						Group: "fake-cloudprovider-group",
					}
					Expect(v1beta1NodePool.Spec.Template.Spec.NodeClassRef).To(BeNil())
					Expect(v1NodePool.ConvertTo(ctx, v1beta1NodePool)).To(BeNil())
					Expect(v1beta1NodePool.Spec.Template.Spec.NodeClassRef.Kind).To(Equal(v1NodePool.Spec.Template.Spec.NodeClassRef.Kind))
					Expect(v1beta1NodePool.Spec.Template.Spec.NodeClassRef.Name).To(Equal(v1NodePool.Spec.Template.Spec.NodeClassRef.Name))
					Expect(v1beta1NodePool.Spec.Template.Spec.NodeClassRef.APIVersion).To(Equal(cloudProvider.NodeClassGroupVersionKind[0].GroupVersion().String()))
				})
				It("should not include APIVersion for v1beta1 if Group and Kind is not in the supported nodeclass", func() {
					v1NodePool.Spec.Template.Spec.NodeClassRef = &NodeClassReference{
						Kind:  "test-kind",
						Name:  "nodeclass-test",
						Group: "testgroup.sh",
					}
					Expect(v1beta1NodePool.Spec.Template.Spec.NodeClassRef).To(BeNil())
					Expect(v1NodePool.ConvertTo(ctx, v1beta1NodePool)).To(BeNil())
					Expect(v1beta1NodePool.Spec.Template.Spec.NodeClassRef.Kind).To(Equal(v1NodePool.Spec.Template.Spec.NodeClassRef.Kind))
					Expect(v1beta1NodePool.Spec.Template.Spec.NodeClassRef.Name).To(Equal(v1NodePool.Spec.Template.Spec.NodeClassRef.Name))
					Expect(v1beta1NodePool.Spec.Template.Spec.NodeClassRef.APIVersion).To(Equal(""))
				})
			})
		})
		Context("Disruption", func() {
			It("should convert v1 nodepool consolidateAfter", func() {
				v1NodePool.Spec.Disruption.ConsolidateAfter = &NillableDuration{Duration: lo.ToPtr(time.Second * 2121)}
				Expect(v1beta1NodePool.Spec.Disruption.ConsolidateAfter).To(BeNil())
				Expect(v1NodePool.ConvertTo(ctx, v1beta1NodePool)).To(BeNil())
				Expect(lo.FromPtr(v1beta1NodePool.Spec.Disruption.ConsolidateAfter.Duration)).To(Equal(lo.FromPtr(v1NodePool.Spec.Disruption.ConsolidateAfter.Duration)))
			})
			It("should convert v1 nodepool consolidatePolicy", func() {
				v1NodePool.Spec.Disruption.ConsolidationPolicy = ConsolidationPolicyWhenEmpty
				Expect(v1beta1NodePool.Spec.Disruption.ConsolidationPolicy).To(Equal(v1beta1.ConsolidationPolicy("")))
				Expect(v1NodePool.ConvertTo(ctx, v1beta1NodePool)).To(BeNil())
				Expect(string(v1beta1NodePool.Spec.Disruption.ConsolidationPolicy)).To(Equal(string(v1NodePool.Spec.Disruption.ConsolidationPolicy)))
			})
			It("should convert v1 nodepool ExpireAfter", func() {
				v1NodePool.Spec.Disruption.ExpireAfter = NillableDuration{Duration: lo.ToPtr(time.Second * 2121)}
				Expect(v1beta1NodePool.Spec.Disruption.ExpireAfter.Duration).To(BeNil())
				Expect(v1NodePool.ConvertTo(ctx, v1beta1NodePool)).To(BeNil())
				Expect(v1beta1NodePool.Spec.Disruption.ExpireAfter.Duration).To(Equal(v1NodePool.Spec.Disruption.ExpireAfter.Duration))
			})
			Context("Budgets", func() {
				It("should convert v1 nodepool nodes", func() {
					v1NodePool.Spec.Disruption.Budgets = append(v1NodePool.Spec.Disruption.Budgets, Budget{
						Nodes: "1545",
					})
					Expect(len(v1beta1NodePool.Spec.Disruption.Budgets)).To(BeNumerically("==", 0))
					Expect(v1NodePool.ConvertTo(ctx, v1beta1NodePool)).To(BeNil())
					for i := range v1NodePool.Spec.Disruption.Budgets {
						Expect(v1beta1NodePool.Spec.Disruption.Budgets[i].Nodes).To(Equal(v1NodePool.Spec.Disruption.Budgets[i].Nodes))
					}
				})
				It("should convert v1 nodepool schedule", func() {
					v1NodePool.Spec.Disruption.Budgets = append(v1NodePool.Spec.Disruption.Budgets, Budget{
						Schedule: lo.ToPtr("1545"),
					})
					Expect(len(v1beta1NodePool.Spec.Disruption.Budgets)).To(BeNumerically("==", 0))
					Expect(v1NodePool.ConvertTo(ctx, v1beta1NodePool)).To(BeNil())
					for i := range v1NodePool.Spec.Disruption.Budgets {
						Expect(v1beta1NodePool.Spec.Disruption.Budgets[i].Schedule).To(Equal(v1NodePool.Spec.Disruption.Budgets[i].Schedule))
					}
				})
				It("should convert v1 nodepool duration", func() {
					v1NodePool.Spec.Disruption.Budgets = append(v1NodePool.Spec.Disruption.Budgets, Budget{
						Duration: &metav1.Duration{Duration: time.Second * 2121},
					})
					Expect(len(v1beta1NodePool.Spec.Disruption.Budgets)).To(BeNumerically("==", 0))
					Expect(v1NodePool.ConvertTo(ctx, v1beta1NodePool)).To(BeNil())
					for i := range v1NodePool.Spec.Disruption.Budgets {
						Expect(v1beta1NodePool.Spec.Disruption.Budgets[i].Duration.Duration).To(Equal(v1NodePool.Spec.Disruption.Budgets[i].Duration.Duration))
					}
				})
			})
		})
	})
	It("should convert v1 nodepool status", func() {
		v1NodePool.Status.Resources = v1.ResourceList{
			v1.ResourceCPU:    resource.MustParse("5"),
			v1.ResourceMemory: resource.MustParse("14145G"),
		}
		Expect(len(lo.Keys(v1beta1NodePool.Status.Resources))).To(BeNumerically("==", 0))
		Expect(v1NodePool.ConvertTo(ctx, v1beta1NodePool)).To(BeNil())
		for _, resource := range lo.Keys(v1NodePool.Status.Resources) {
			Expect(v1beta1NodePool.Status.Resources[resource]).To(Equal(v1NodePool.Status.Resources[resource]))
		}
	})
})

var _ = Describe("Convert V1beta1 to V1 NodePool API", func() {
	BeforeEach(func() {
		v1NodePool = &NodePool{}
		v1beta1NodePool = &v1beta1.NodePool{}
		cloudProvider.NodeClassGroupVersionKind = []schema.GroupVersionKind{
			{
				Group:   "fake-cloudprovider-group",
				Version: "fake-cloudprovider-version",
				Kind:    "fake-cloudprovider-kind",
			},
		}
		ctx = injection.WithNodeClasses(ctx, cloudProvider.GetSupportedNodeClasses())
	})

	Context("MetaData", func() {
		It("should convert v1beta1 nodepool Name", func() {
			v1beta1NodePool.Name = "test-name-v1"
			Expect(v1NodePool.Name).To(BeEmpty())
			Expect(v1NodePool.ConvertFrom(ctx, v1beta1NodePool)).To(BeNil())
			Expect(v1NodePool.Name).To(Equal(v1beta1NodePool.Name))
		})
		It("should convert v1beta1 nodepool UID", func() {
			v1beta1NodePool.UID = types.UID("test-name-v1")
			Expect(v1NodePool.UID).To(BeEmpty())
			Expect(v1NodePool.ConvertFrom(ctx, v1beta1NodePool)).To(BeNil())
			Expect(v1NodePool.UID).To(Equal(v1beta1NodePool.UID))
		})
	})
	Context("NodePool Spec", func() {
		It("should convert v1beta1 nodepool weights", func() {
			v1beta1NodePool.Spec.Weight = lo.ToPtr(int32(62))
			Expect(lo.FromPtr(v1NodePool.Spec.Weight)).To(Equal(int32(0)))
			Expect(v1NodePool.ConvertFrom(ctx, v1beta1NodePool)).To(BeNil())
			Expect(v1NodePool.Spec.Weight).To(Equal(v1beta1NodePool.Spec.Weight))
		})
		It("should convert v1beta1 nodepool limits", func() {
			v1beta1NodePool.Spec.Limits = v1beta1.Limits{
				v1.ResourceCPU:    resource.MustParse("5"),
				v1.ResourceMemory: resource.MustParse("14145G"),
			}
			Expect(len(lo.Keys(v1NodePool.Spec.Limits))).To(BeNumerically("==", 0))
			Expect(v1NodePool.ConvertFrom(ctx, v1beta1NodePool)).To(BeNil())
			for _, resource := range lo.Keys(v1beta1NodePool.Spec.Limits) {
				Expect(v1NodePool.Spec.Limits[resource]).To(Equal(v1beta1NodePool.Spec.Limits[resource]))
			}
		})
		Context("NodeClaimTemplate", func() {
			It("should convert v1beta1 nodepool template labels", func() {
				v1beta1NodePool.Spec.Template.Labels = map[string]string{
					"test-key-1": "test-value-1",
					"test-key-2": "test-value-2",
				}
				Expect(v1NodePool.Spec.Template.Labels).To(BeNil())
				Expect(v1NodePool.ConvertFrom(ctx, v1beta1NodePool)).To(BeNil())
				for key := range v1beta1NodePool.Spec.Template.Labels {
					Expect(v1NodePool.Spec.Template.Labels[key]).To(Equal(v1beta1NodePool.Spec.Template.Labels[key]))
				}
			})
			It("should convert v1beta1 nodepool template annotations", func() {
				v1beta1NodePool.Spec.Template.Annotations = map[string]string{
					"test-key-1": "test-value-1",
					"test-key-2": "test-value-2",
				}
				Expect(v1NodePool.Spec.Template.Annotations).To(BeNil())
				Expect(v1NodePool.ConvertFrom(ctx, v1beta1NodePool)).To(BeNil())
				for key := range v1beta1NodePool.Spec.Template.Annotations {
					Expect(v1NodePool.Spec.Template.Annotations[key]).To(Equal(v1beta1NodePool.Spec.Template.Annotations[key]))
				}
			})
			It("should convert v1beta1 nodepool template taints", func() {
				v1beta1NodePool.Spec.Template.Spec.Taints = []v1.Taint{
					{
						Key:    "test-key-1",
						Value:  "test-value-1",
						Effect: v1.TaintEffectNoExecute,
					},
					{
						Key:    "test-key-2",
						Value:  "test-value-2",
						Effect: v1.TaintEffectNoSchedule,
					},
				}
				Expect(len(v1NodePool.Spec.Template.Spec.Taints)).To(BeNumerically("==", 0))
				Expect(v1NodePool.ConvertFrom(ctx, v1beta1NodePool)).To(BeNil())
				for i := range v1beta1NodePool.Spec.Template.Spec.Taints {
					Expect(v1NodePool.Spec.Template.Spec.Taints[i].Key).To(Equal(v1beta1NodePool.Spec.Template.Spec.Taints[i].Key))
					Expect(v1NodePool.Spec.Template.Spec.Taints[i].Value).To(Equal(v1beta1NodePool.Spec.Template.Spec.Taints[i].Value))
					Expect(v1NodePool.Spec.Template.Spec.Taints[i].Effect).To(Equal(v1beta1NodePool.Spec.Template.Spec.Taints[i].Effect))
				}
			})
			It("should convert v1beta1 nodepool template startup taints", func() {
				v1beta1NodePool.Spec.Template.Spec.StartupTaints = []v1.Taint{
					{
						Key:    "test-key-startup-1",
						Value:  "test-value-startup-1",
						Effect: v1.TaintEffectNoExecute,
					},
					{
						Key:    "test-key-startup-2",
						Value:  "test-value-startup-2",
						Effect: v1.TaintEffectNoSchedule,
					},
				}
				Expect(len(v1NodePool.Spec.Template.Spec.StartupTaints)).To(BeNumerically("==", 0))
				Expect(v1NodePool.ConvertFrom(ctx, v1beta1NodePool)).To(BeNil())
				for i := range v1beta1NodePool.Spec.Template.Spec.StartupTaints {
					Expect(v1NodePool.Spec.Template.Spec.StartupTaints[i].Key).To(Equal(v1beta1NodePool.Spec.Template.Spec.StartupTaints[i].Key))
					Expect(v1NodePool.Spec.Template.Spec.StartupTaints[i].Value).To(Equal(v1beta1NodePool.Spec.Template.Spec.StartupTaints[i].Value))
					Expect(v1NodePool.Spec.Template.Spec.StartupTaints[i].Effect).To(Equal(v1beta1NodePool.Spec.Template.Spec.StartupTaints[i].Effect))
				}
			})
			It("should convert v1beta1 nodepool template requirements", func() {
				v1beta1NodePool.Spec.Template.Spec.Requirements = []v1beta1.NodeSelectorRequirementWithMinValues{
					{
						NodeSelectorRequirement: v1.NodeSelectorRequirement{
							Key:      v1.LabelArchStable,
							Operator: v1.NodeSelectorOpExists,
						},
						MinValues: lo.ToPtr(0),
					},
					{
						NodeSelectorRequirement: v1.NodeSelectorRequirement{
							Key:      CapacityTypeLabelKey,
							Operator: v1.NodeSelectorOpIn,
							Values:   []string{CapacityTypeSpot},
						},
						MinValues: lo.ToPtr(0),
					},
				}
				Expect(len(v1NodePool.Spec.Template.Spec.Requirements)).To(BeNumerically("==", 0))
				Expect(v1NodePool.ConvertFrom(ctx, v1beta1NodePool)).To(BeNil())
				for i := range v1beta1NodePool.Spec.Template.Spec.Requirements {
					Expect(v1NodePool.Spec.Template.Spec.Requirements[i].Key).To(Equal(v1beta1NodePool.Spec.Template.Spec.Requirements[i].Key))
					Expect(v1NodePool.Spec.Template.Spec.Requirements[i].Operator).To(Equal(v1beta1NodePool.Spec.Template.Spec.Requirements[i].Operator))
					Expect(v1NodePool.Spec.Template.Spec.Requirements[i].Values).To(Equal(v1beta1NodePool.Spec.Template.Spec.Requirements[i].Values))
				}
			})
			// It("should convert v1 nodepool template kubelet", func() {
			// 	v1beta1NodePool.Spec.Template.Spec.Kubelet = &v1beta1.KubeletConfiguration{
			// 		ClusterDNS:                  []string{"test-cluster-dns"},
			// 		MaxPods:                     lo.ToPtr(int32(9383)),
			// 		PodsPerCore:                 lo.ToPtr(int32(9334283)),
			// 		SystemReserved:              map[string]string{"system-key": "reserved"},
			// 		KubeReserved:                map[string]string{"kube-key": "reserved"},
			// 		EvictionHard:                map[string]string{"eviction-key": "eviction"},
			// 		EvictionSoft:                map[string]string{"eviction-key": "eviction"},
			// 		EvictionSoftGracePeriod:     map[string]metav1.Duration{"test-soft-grace": {Duration: time.Hour}},
			// 		EvictionMaxPodGracePeriod:   lo.ToPtr(int32(382902)),
			// 		ImageGCHighThresholdPercent: lo.ToPtr(int32(382902)),
			// 		CPUCFSQuota:                 lo.ToPtr(false),
			// 	}
			// 	Expect(v1NodePool.Annotations).To(BeNil())
			// 	Expect(v1NodePool.ConvertFrom(ctx, v1beta1NodePool)).To(BeNil())
			// 	kubelet := &v1beta1.KubeletConfiguration{}
			// 	kubeletString, found := v1NodePool.Annotations[V1Beta1KubeletConfiguration]
			// 	Expect(found).To(BeTrue())
			// 	err := json.Unmarshal([]byte(kubeletString), kubelet)
			// 	Expect(err).To(BeNil())
			// 	Expect(kubelet.ClusterDNS).To(Equal(v1beta1NodePool.Spec.Template.Spec.Kubelet.ClusterDNS))
			// 	Expect(lo.FromPtr(kubelet.MaxPods)).To(Equal(lo.FromPtr(v1beta1NodePool.Spec.Template.Spec.Kubelet.MaxPods)))
			// 	Expect(lo.FromPtr(kubelet.PodsPerCore)).To(Equal(lo.FromPtr(v1beta1NodePool.Spec.Template.Spec.Kubelet.PodsPerCore)))
			// 	Expect(lo.FromPtr(kubelet.EvictionMaxPodGracePeriod)).To(Equal(lo.FromPtr(v1beta1NodePool.Spec.Template.Spec.Kubelet.EvictionMaxPodGracePeriod)))
			// 	Expect(lo.FromPtr(kubelet.ImageGCHighThresholdPercent)).To(Equal(lo.FromPtr(v1beta1NodePool.Spec.Template.Spec.Kubelet.ImageGCHighThresholdPercent)))
			// 	Expect(lo.FromPtr(kubelet.ImageGCHighThresholdPercent)).To(Equal(lo.FromPtr(v1beta1NodePool.Spec.Template.Spec.Kubelet.ImageGCHighThresholdPercent)))
			// 	Expect(lo.FromPtr(kubelet.ImageGCHighThresholdPercent)).To(Equal(lo.FromPtr(v1beta1NodePool.Spec.Template.Spec.Kubelet.ImageGCHighThresholdPercent)))
			// 	Expect(lo.FromPtr(kubelet.ImageGCHighThresholdPercent)).To(Equal(lo.FromPtr(v1beta1NodePool.Spec.Template.Spec.Kubelet.ImageGCHighThresholdPercent)))
			// 	Expect(kubelet.SystemReserved).To(Equal(v1beta1NodePool.Spec.Template.Spec.Kubelet.SystemReserved))
			// 	Expect(kubelet.KubeReserved).To(Equal(v1beta1NodePool.Spec.Template.Spec.Kubelet.KubeReserved))
			// 	Expect(kubelet.EvictionHard).To(Equal(v1beta1NodePool.Spec.Template.Spec.Kubelet.EvictionHard))
			// 	Expect(kubelet.EvictionSoft).To(Equal(v1beta1NodePool.Spec.Template.Spec.Kubelet.EvictionSoft))
			// 	Expect(kubelet.EvictionSoftGracePeriod).To(Equal(v1beta1NodePool.Spec.Template.Spec.Kubelet.EvictionSoftGracePeriod))
			// 	Expect(lo.FromPtr(kubelet.CPUCFSQuota)).To(Equal(lo.FromPtr(v1beta1NodePool.Spec.Template.Spec.Kubelet.CPUCFSQuota)))
			// })
			Context("NodeClassRef", func() {
				It("should convert v1beta1 nodepool template nodeClassRef", func() {
					v1beta1NodePool.Spec.Template.Spec.NodeClassRef = &v1beta1.NodeClassReference{
						Kind:       "test-kind",
						Name:       "nodeclass-test",
						APIVersion: "testgroup.sh/testversion",
					}
					Expect(v1NodePool.Spec.Template.Spec.NodeClassRef).To(BeNil())
					Expect(v1NodePool.ConvertFrom(ctx, v1beta1NodePool)).To(BeNil())
					Expect(v1NodePool.Spec.Template.Spec.NodeClassRef.Kind).To(Equal(v1beta1NodePool.Spec.Template.Spec.NodeClassRef.Kind))
					Expect(v1NodePool.Spec.Template.Spec.NodeClassRef.Name).To(Equal(v1beta1NodePool.Spec.Template.Spec.NodeClassRef.Name))
					Expect(v1NodePool.Spec.Template.Spec.NodeClassRef.Group).To(Equal("testgroup.sh"))
				})
				It("should set default nodeclass group and kind on v1beta1 nodeclassRef", func() {
					v1beta1NodePool.Spec.Template.Spec.NodeClassRef = &v1beta1.NodeClassReference{
						Name: "nodeclass-test",
					}
					Expect(v1NodePool.Spec.Template.Spec.NodeClassRef).To(BeNil())
					Expect(v1NodePool.ConvertFrom(ctx, v1beta1NodePool)).To(BeNil())
					Expect(v1NodePool.Spec.Template.Spec.NodeClassRef.Kind).To(Equal(cloudProvider.NodeClassGroupVersionKind[0].Kind))
					Expect(v1NodePool.Spec.Template.Spec.NodeClassRef.Name).To(Equal(v1beta1NodePool.Spec.Template.Spec.NodeClassRef.Name))
					Expect(v1NodePool.Spec.Template.Spec.NodeClassRef.Group).To(Equal(cloudProvider.NodeClassGroupVersionKind[0].Group))
				})
			})
		})
		Context("Disruption", func() {
			It("should convert v1beta1 nodepool consolidateAfter", func() {
				v1beta1NodePool.Spec.Disruption.ConsolidateAfter = &v1beta1.NillableDuration{Duration: lo.ToPtr(time.Second * 2121)}
				Expect(v1NodePool.Spec.Disruption.ConsolidateAfter).To(BeNil())
				Expect(v1NodePool.ConvertFrom(ctx, v1beta1NodePool)).To(BeNil())
				Expect(v1NodePool.Spec.Disruption.ConsolidateAfter.Duration).To(Equal(v1beta1NodePool.Spec.Disruption.ConsolidateAfter.Duration))
			})
			It("should convert v1beta1 nodepool consolidatePolicy", func() {
				v1beta1NodePool.Spec.Disruption.ConsolidationPolicy = v1beta1.ConsolidationPolicyWhenEmpty
				Expect(v1NodePool.Spec.Disruption.ConsolidationPolicy).To(Equal(ConsolidationPolicy("")))
				Expect(v1NodePool.ConvertFrom(ctx, v1beta1NodePool)).To(BeNil())
				Expect(string(v1NodePool.Spec.Disruption.ConsolidationPolicy)).To(Equal(string(v1beta1NodePool.Spec.Disruption.ConsolidationPolicy)))
			})
			It("should convert v1beta1 nodepool ExpireAfter", func() {
				v1beta1NodePool.Spec.Disruption.ExpireAfter = v1beta1.NillableDuration{Duration: lo.ToPtr(time.Second * 2121)}
				Expect(v1NodePool.Spec.Disruption.ExpireAfter.Duration).To(BeNil())
				Expect(v1NodePool.ConvertFrom(ctx, v1beta1NodePool)).To(BeNil())
				Expect(v1NodePool.Spec.Disruption.ExpireAfter.Duration).To(Equal(v1beta1NodePool.Spec.Disruption.ExpireAfter.Duration))
			})
			Context("Budgets", func() {
				It("should convert v1beta1 nodepool nodes", func() {
					v1beta1NodePool.Spec.Disruption.Budgets = append(v1beta1NodePool.Spec.Disruption.Budgets, v1beta1.Budget{
						Nodes: "1545",
					})
					Expect(len(v1NodePool.Spec.Disruption.Budgets)).To(BeNumerically("==", 0))
					Expect(v1NodePool.ConvertFrom(ctx, v1beta1NodePool)).To(BeNil())
					for i := range v1beta1NodePool.Spec.Disruption.Budgets {
						Expect(v1NodePool.Spec.Disruption.Budgets[i].Nodes).To(Equal(v1beta1NodePool.Spec.Disruption.Budgets[i].Nodes))
					}
				})
				It("should convert v1beta1 nodepool schedule", func() {
					v1beta1NodePool.Spec.Disruption.Budgets = append(v1beta1NodePool.Spec.Disruption.Budgets, v1beta1.Budget{
						Schedule: lo.ToPtr("1545"),
					})
					Expect(len(v1NodePool.Spec.Disruption.Budgets)).To(BeNumerically("==", 0))
					Expect(v1NodePool.ConvertFrom(ctx, v1beta1NodePool)).To(BeNil())
					for i := range v1beta1NodePool.Spec.Disruption.Budgets {
						Expect(v1NodePool.Spec.Disruption.Budgets[i].Schedule).To(Equal(v1beta1NodePool.Spec.Disruption.Budgets[i].Schedule))
					}
				})
				It("should convert v1beta1 nodepool duration", func() {
					v1beta1NodePool.Spec.Disruption.Budgets = append(v1beta1NodePool.Spec.Disruption.Budgets, v1beta1.Budget{
						Duration: &metav1.Duration{Duration: time.Second * 2121},
					})
					Expect(len(v1NodePool.Spec.Disruption.Budgets)).To(BeNumerically("==", 0))
					Expect(v1NodePool.ConvertFrom(ctx, v1beta1NodePool)).To(BeNil())
					for i := range v1beta1NodePool.Spec.Disruption.Budgets {
						Expect(v1NodePool.Spec.Disruption.Budgets[i].Duration.Duration).To(Equal(v1beta1NodePool.Spec.Disruption.Budgets[i].Duration.Duration))
					}
				})
			})
		})
	})
	It("should convert v1beta1 nodepool status", func() {
		v1beta1NodePool.Status.Resources = v1.ResourceList{
			v1.ResourceCPU:    resource.MustParse("5"),
			v1.ResourceMemory: resource.MustParse("14145G"),
		}
		Expect(len(lo.Keys(v1NodePool.Status.Resources))).To(BeNumerically("==", 0))
		Expect(v1NodePool.ConvertFrom(ctx, v1beta1NodePool)).To(BeNil())
		for _, resource := range lo.Keys(v1beta1NodePool.Status.Resources) {
			Expect(v1beta1NodePool.Status.Resources[resource]).To(Equal(v1NodePool.Status.Resources[resource]))
		}
	})
})
