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
	"encoding/json"
	"time"

	"github.com/awslabs/operatorpkg/status"
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
	"sigs.k8s.io/karpenter/pkg/test"
)

var (
	v1NodeClaim      *NodeClaim
	v1beta1NodeClaim *v1beta1.NodeClaim
)

var _ = Describe("Convert v1 to v1beta1 NodeClaim API", func() {
	BeforeEach(func() {
		v1beta1NodePool = test.NodePool()
		v1NodePool = &NodePool{
			ObjectMeta: metav1.ObjectMeta{
				Name: "test-nodepool",
			},
			Spec: NodePoolSpec{
				Template: NodeClaimTemplate{
					Spec: NodeClaimTemplateSpec{
						NodeClassRef: &NodeClassReference{
							Name:  "test",
							Kind:  "test-kind",
							Group: "test-group",
						},
						Requirements: []NodeSelectorRequirementWithMinValues{},
					},
				},
			},
		}
		v1NodeClaim = &NodeClaim{}
		v1NodeClaim.Labels = map[string]string{
			NodePoolLabelKey: v1NodePool.Name,
		}
		v1beta1NodeClaim = &v1beta1.NodeClaim{}
		v1beta1NodeClaim.Labels = map[string]string{
			NodePoolLabelKey: v1beta1NodePool.Name,
		}
		Expect(env.Client.Create(ctx, v1NodePool)).To(BeNil())
		cloudProvider.NodeClassGroupVersionKind = []schema.GroupVersionKind{
			{
				Group:   "fake-cloudprovider-group",
				Version: "fake-cloudprovider-version",
				Kind:    "fake-cloudprovider-kind",
			},
		}
		ctx = injection.NodeClassToContext(ctx, cloudProvider.GetSupportedNodeClasses())
		ctx = injection.WithClient(ctx, env.Client)
	})

	Context("MetaData", func() {
		It("should convert v1 nodeclaim Name", func() {
			v1NodeClaim.Name = "test-name-v1"
			Expect(v1beta1NodeClaim.Name).To(BeEmpty())
			Expect(v1NodeClaim.ConvertTo(ctx, v1beta1NodeClaim)).To(BeNil())
			Expect(v1beta1NodeClaim.Name).To(Equal(v1NodeClaim.Name))
		})
		It("should convert v1 nodeclaim UID", func() {
			v1NodeClaim.UID = types.UID("test-name-v1")
			Expect(v1beta1NodeClaim.UID).To(BeEmpty())
			Expect(v1NodeClaim.ConvertTo(ctx, v1beta1NodeClaim)).To(BeNil())
			Expect(v1beta1NodeClaim.UID).To(Equal(v1NodeClaim.UID))
		})
		It("should update v1 nodeclaim drift hash from v1beta1 nodepool", func() {
			v1NodePool.Annotations = map[string]string{
				NodePoolHashAnnotationKey:        "test-hash-1",
				NodePoolHashVersionAnnotationKey: "test-hash-version-1",
			}
			Expect(env.Client.Update(ctx, v1NodePool)).To(BeNil())
			Expect(v1beta1NodeClaim.Annotations).To(BeNil())
			Expect(v1NodeClaim.ConvertTo(ctx, v1beta1NodeClaim)).To(BeNil())
			driftHash, found := v1beta1NodeClaim.Annotations[v1beta1.NodePoolHashAnnotationKey]
			Expect(found).To(BeTrue())
			Expect(driftHash).To(Equal("test-hash-1"))
			driftHashVersion, found := v1beta1NodeClaim.Annotations[v1beta1.NodePoolHashVersionAnnotationKey]
			Expect(found).To(BeTrue())
			Expect(driftHashVersion).To(Equal("test-hash-version-1"))
		})
	})
	Context("NodeClaim Spec", func() {
		It("should convert v1 nodeclaim taints", func() {
			v1NodeClaim.Spec.Taints = []v1.Taint{
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
			Expect(len(v1beta1NodeClaim.Spec.Taints)).To(BeNumerically("==", 0))
			Expect(v1NodeClaim.ConvertTo(ctx, v1beta1NodeClaim)).To(BeNil())
			for i := range v1NodeClaim.Spec.Taints {
				Expect(v1beta1NodeClaim.Spec.Taints[i].Key).To(Equal(v1NodeClaim.Spec.Taints[i].Key))
				Expect(v1beta1NodeClaim.Spec.Taints[i].Value).To(Equal(v1NodeClaim.Spec.Taints[i].Value))
				Expect(v1beta1NodeClaim.Spec.Taints[i].Effect).To(Equal(v1NodeClaim.Spec.Taints[i].Effect))
			}
		})
		It("should convert v1 nodeclaim startup taints", func() {
			v1NodeClaim.Spec.StartupTaints = []v1.Taint{
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
			Expect(len(v1beta1NodeClaim.Spec.StartupTaints)).To(BeNumerically("==", 0))
			Expect(v1NodeClaim.ConvertTo(ctx, v1beta1NodeClaim)).To(BeNil())
			for i := range v1NodeClaim.Spec.StartupTaints {
				Expect(v1beta1NodeClaim.Spec.StartupTaints[i].Key).To(Equal(v1NodeClaim.Spec.StartupTaints[i].Key))
				Expect(v1beta1NodeClaim.Spec.StartupTaints[i].Value).To(Equal(v1NodeClaim.Spec.StartupTaints[i].Value))
				Expect(v1beta1NodeClaim.Spec.StartupTaints[i].Effect).To(Equal(v1NodeClaim.Spec.StartupTaints[i].Effect))
			}
		})
		It("should convert v1 nodeclaim requirements", func() {
			v1NodeClaim.Spec.Requirements = []NodeSelectorRequirementWithMinValues{
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
			Expect(len(v1beta1NodeClaim.Spec.Requirements)).To(BeNumerically("==", 0))
			Expect(v1NodeClaim.ConvertTo(ctx, v1beta1NodeClaim)).To(BeNil())
			for i := range v1NodeClaim.Spec.Requirements {
				Expect(v1beta1NodeClaim.Spec.Requirements[i].Key).To(Equal(v1NodeClaim.Spec.Requirements[i].Key))
				Expect(v1beta1NodeClaim.Spec.Requirements[i].Operator).To(Equal(v1NodeClaim.Spec.Requirements[i].Operator))
				Expect(v1beta1NodeClaim.Spec.Requirements[i].Values).To(Equal(v1NodeClaim.Spec.Requirements[i].Values))
			}
		})
		It("should convert v1 nodeclaim resources", func() {
			v1NodeClaim.Spec.Resources = ResourceRequirements{
				Requests: v1.ResourceList{
					v1.ResourceCPU:    resource.MustParse("1"),
					v1.ResourceMemory: resource.MustParse("134G"),
				},
			}
			Expect(v1beta1NodeClaim.Spec.Resources.Requests).To(BeNil())
			Expect(v1NodeClaim.ConvertTo(ctx, v1beta1NodeClaim)).To(BeNil())
			for key := range v1NodeClaim.Spec.Resources.Requests {
				Expect(v1NodeClaim.Spec.Resources.Requests[key]).To(Equal(v1beta1NodeClaim.Spec.Resources.Requests[key]))
			}
		})
		Context("NodeClassRef", func() {
			It("should convert v1 nodeclaim template nodeClassRef", func() {
				v1NodeClaim.Spec.NodeClassRef = &NodeClassReference{
					Kind:  "fake-cloudprovider-kind",
					Name:  "nodeclass-test",
					Group: "fake-cloudprovider-group",
				}
				Expect(v1beta1NodeClaim.Spec.NodeClassRef).To(BeNil())
				Expect(v1NodeClaim.ConvertTo(ctx, v1beta1NodeClaim)).To(BeNil())
				Expect(v1beta1NodeClaim.Spec.NodeClassRef.Kind).To(Equal(v1NodeClaim.Spec.NodeClassRef.Kind))
				Expect(v1beta1NodeClaim.Spec.NodeClassRef.Name).To(Equal(v1NodeClaim.Spec.NodeClassRef.Name))
				Expect(v1beta1NodeClaim.Spec.NodeClassRef.APIVersion).To(Equal(cloudProvider.NodeClassGroupVersionKind[0].GroupVersion().String()))
			})
			It("should not include APIVersion for v1beta1 if Group and Kind is not in the supported nodeclass", func() {
				v1NodeClaim.Spec.NodeClassRef = &NodeClassReference{
					Kind:  "test-kind",
					Name:  "nodeclass-test",
					Group: "testgroup.sh",
				}
				Expect(v1beta1NodeClaim.Spec.NodeClassRef).To(BeNil())
				Expect(v1NodeClaim.ConvertTo(ctx, v1beta1NodeClaim)).To(BeNil())
				Expect(v1beta1NodeClaim.Spec.NodeClassRef.Kind).To(Equal(v1NodeClaim.Spec.NodeClassRef.Kind))
				Expect(v1beta1NodeClaim.Spec.NodeClassRef.Name).To(Equal(v1NodeClaim.Spec.NodeClassRef.Name))
				Expect(v1beta1NodeClaim.Spec.NodeClassRef.APIVersion).To(Equal(""))
			})
		})
	})
	Context("NodeClaim Status", func() {
		It("should convert v1 nodeclaim nodename", func() {
			v1NodeClaim.Status.NodeName = "test-node-name"
			Expect(v1beta1NodeClaim.Status.NodeName).To(Equal(""))
			Expect(v1NodeClaim.ConvertTo(ctx, v1beta1NodeClaim)).To(BeNil())
			Expect(v1NodeClaim.Status.NodeName).To(Equal(v1beta1NodeClaim.Status.NodeName))
		})
		It("should convert v1 nodeclaim provider id", func() {
			v1NodeClaim.Status.ProviderID = "test-provider-id"
			Expect(v1beta1NodeClaim.Status.ProviderID).To(Equal(""))
			Expect(v1NodeClaim.ConvertTo(ctx, v1beta1NodeClaim)).To(BeNil())
			Expect(v1NodeClaim.Status.ProviderID).To(Equal(v1beta1NodeClaim.Status.ProviderID))
		})
		It("should convert v1 nodeclaim image id", func() {
			v1NodeClaim.Status.ImageID = "test-image-id"
			Expect(v1beta1NodeClaim.Status.ImageID).To(Equal(""))
			Expect(v1NodeClaim.ConvertTo(ctx, v1beta1NodeClaim)).To(BeNil())
			Expect(v1NodeClaim.Status.ImageID).To(Equal(v1beta1NodeClaim.Status.ImageID))
		})
		It("should convert v1 nodeclaim capacity", func() {
			v1NodeClaim.Status.Capacity = v1.ResourceList{
				v1.ResourceCPU:    resource.MustParse("13432"),
				v1.ResourceMemory: resource.MustParse("1332G"),
			}
			Expect(v1beta1NodeClaim.Status.Capacity).To(BeNil())
			Expect(v1NodeClaim.ConvertTo(ctx, v1beta1NodeClaim)).To(BeNil())
			Expect(v1NodeClaim.Status.Capacity).To(Equal(v1beta1NodeClaim.Status.Capacity))
		})
		It("should convert v1 nodeclaim allocatable", func() {
			v1NodeClaim.Status.Allocatable = v1.ResourceList{
				v1.ResourceCPU:    resource.MustParse("13432"),
				v1.ResourceMemory: resource.MustParse("1332G"),
			}
			Expect(v1beta1NodeClaim.Status.Allocatable).To(BeNil())
			Expect(v1NodeClaim.ConvertTo(ctx, v1beta1NodeClaim)).To(BeNil())
			Expect(v1NodeClaim.Status.Allocatable).To(Equal(v1beta1NodeClaim.Status.Allocatable))
		})
		It("should convert v1 nodeclaim conditions", func() {
			v1NodeClaim.Status.Conditions = []status.Condition{
				{
					Status: status.ConditionReady,
					Reason: "test-reason",
				},
				{
					Status: ConditionTypeDrifted,
					Reason: "test-reason",
				},
			}
			Expect(v1beta1NodeClaim.Status.Conditions).To(BeNil())
			Expect(v1NodeClaim.ConvertTo(ctx, v1beta1NodeClaim)).To(BeNil())
			Expect(v1NodeClaim.Status.Conditions).To(Equal(v1beta1NodeClaim.Status.Conditions))
		})
	})
})

var _ = Describe("Convert V1beta1 to V1 NodeClaim API", func() {
	BeforeEach(func() {
		v1NodePool = &NodePool{
			ObjectMeta: metav1.ObjectMeta{
				Name: "test-nodepool",
			},
			Spec: NodePoolSpec{
				Template: NodeClaimTemplate{
					Spec: NodeClaimTemplateSpec{
						NodeClassRef: &NodeClassReference{
							Name:  "test",
							Kind:  "test-kind",
							Group: "test-group",
						},
						Requirements: []NodeSelectorRequirementWithMinValues{},
					},
				},
			},
		}
		v1beta1NodePool = test.NodePool()
		v1NodeClaim = &NodeClaim{}
		v1NodeClaim.Labels = map[string]string{
			NodePoolLabelKey: v1NodePool.Name,
		}
		v1beta1NodeClaim = &v1beta1.NodeClaim{}
		v1beta1NodeClaim.Labels = map[string]string{
			NodePoolLabelKey: v1beta1NodePool.Name,
		}
		Expect(env.Client.Create(ctx, v1beta1NodePool)).To(BeNil())
		cloudProvider.NodeClassGroupVersionKind = []schema.GroupVersionKind{
			{
				Group:   "fake-cloudprovider-group",
				Version: "fake-cloudprovider-version",
				Kind:    "fake-cloudprovider-kind",
			},
		}
		ctx = injection.NodeClassToContext(ctx, cloudProvider.GetSupportedNodeClasses())
		ctx = injection.WithClient(ctx, env.Client)
	})

	Context("MetaData", func() {
		It("should convert v1beta1 nodeclaim Name", func() {
			v1beta1NodeClaim.Name = "test-name-v1"
			Expect(v1NodeClaim.Name).To(BeEmpty())
			Expect(v1NodeClaim.ConvertFrom(ctx, v1beta1NodeClaim)).To(BeNil())
			Expect(v1NodeClaim.Name).To(Equal(v1beta1NodeClaim.Name))
		})
		It("should convert v1beta1 nodeclaim UID", func() {
			v1beta1NodeClaim.UID = types.UID("test-name-v1")
			Expect(v1NodeClaim.UID).To(BeEmpty())
			Expect(v1NodeClaim.ConvertFrom(ctx, v1beta1NodeClaim)).To(BeNil())
			Expect(v1NodeClaim.UID).To(Equal(v1beta1NodeClaim.UID))
		})
		It("should update v1beta1 nodeclaim drift hash from v1 nodepool", func() {
			v1beta1NodePool.Annotations = map[string]string{
				NodePoolHashAnnotationKey:        "test-hash-1",
				NodePoolHashVersionAnnotationKey: "test-hash-version-1",
			}
			Expect(env.Client.Update(ctx, v1beta1NodePool)).To(BeNil())
			Expect(v1NodeClaim.Annotations).To(BeNil())
			Expect(v1NodeClaim.ConvertFrom(ctx, v1beta1NodeClaim)).To(BeNil())
			driftHash, found := v1NodeClaim.Annotations[NodePoolHashAnnotationKey]
			Expect(found).To(BeTrue())
			Expect(driftHash).To(Equal("test-hash-1"))
			driftHashVersion, found := v1NodeClaim.Annotations[v1beta1.NodePoolHashVersionAnnotationKey]
			Expect(found).To(BeTrue())
			Expect(driftHashVersion).To(Equal("test-hash-version-1"))
		})
	})
	Context("NodeClaim Spec", func() {
		It("should convert v1beta1 nodeclaim taints", func() {
			v1beta1NodeClaim.Spec.Taints = []v1.Taint{
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
			Expect(len(v1NodeClaim.Spec.Taints)).To(BeNumerically("==", 0))
			Expect(v1NodeClaim.ConvertFrom(ctx, v1beta1NodeClaim)).To(BeNil())
			for i := range v1beta1NodeClaim.Spec.Taints {
				Expect(v1NodeClaim.Spec.Taints[i].Key).To(Equal(v1beta1NodeClaim.Spec.Taints[i].Key))
				Expect(v1NodeClaim.Spec.Taints[i].Value).To(Equal(v1beta1NodeClaim.Spec.Taints[i].Value))
				Expect(v1NodeClaim.Spec.Taints[i].Effect).To(Equal(v1beta1NodeClaim.Spec.Taints[i].Effect))
			}
		})
		It("should convert v1beta1 nodeclaim startup taints", func() {
			v1beta1NodeClaim.Spec.StartupTaints = []v1.Taint{
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
			Expect(len(v1NodeClaim.Spec.StartupTaints)).To(BeNumerically("==", 0))
			Expect(v1NodeClaim.ConvertFrom(ctx, v1beta1NodeClaim)).To(BeNil())
			for i := range v1beta1NodeClaim.Spec.StartupTaints {
				Expect(v1NodeClaim.Spec.StartupTaints[i].Key).To(Equal(v1beta1NodeClaim.Spec.StartupTaints[i].Key))
				Expect(v1NodeClaim.Spec.StartupTaints[i].Value).To(Equal(v1beta1NodeClaim.Spec.StartupTaints[i].Value))
				Expect(v1NodeClaim.Spec.StartupTaints[i].Effect).To(Equal(v1beta1NodeClaim.Spec.StartupTaints[i].Effect))
			}
		})
		It("should convert v1beta1 nodeclaim requirements", func() {
			v1beta1NodeClaim.Spec.Requirements = []v1beta1.NodeSelectorRequirementWithMinValues{
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
			Expect(len(v1NodeClaim.Spec.Requirements)).To(BeNumerically("==", 0))
			Expect(v1NodeClaim.ConvertFrom(ctx, v1beta1NodeClaim)).To(BeNil())
			for i := range v1beta1NodeClaim.Spec.Requirements {
				Expect(v1NodeClaim.Spec.Requirements[i].Key).To(Equal(v1beta1NodeClaim.Spec.Requirements[i].Key))
				Expect(v1NodeClaim.Spec.Requirements[i].Operator).To(Equal(v1beta1NodeClaim.Spec.Requirements[i].Operator))
				Expect(v1NodeClaim.Spec.Requirements[i].Values).To(Equal(v1beta1NodeClaim.Spec.Requirements[i].Values))
			}
		})
		It("should convert v1beta1 nodeclaim resources", func() {
			v1beta1NodeClaim.Spec.Resources = v1beta1.ResourceRequirements{
				Requests: v1.ResourceList{
					v1.ResourceCPU:    resource.MustParse("1"),
					v1.ResourceMemory: resource.MustParse("134G"),
				},
			}
			Expect(v1NodeClaim.Spec.Resources.Requests).To(BeNil())
			Expect(v1NodeClaim.ConvertFrom(ctx, v1beta1NodeClaim)).To(BeNil())
			for key := range v1beta1NodeClaim.Spec.Resources.Requests {
				Expect(v1beta1NodeClaim.Spec.Resources.Requests[key]).To(Equal(v1NodeClaim.Spec.Resources.Requests[key]))
			}
		})
		It("should convert v1 nodeclaim template kubelet", func() {
			v1beta1NodeClaim.Spec.Kubelet = &v1beta1.KubeletConfiguration{
				ClusterDNS:                  []string{"test-cluster-dns"},
				MaxPods:                     lo.ToPtr(int32(9383)),
				PodsPerCore:                 lo.ToPtr(int32(9334283)),
				SystemReserved:              map[string]string{"system-key": "reserved"},
				KubeReserved:                map[string]string{"kube-key": "reserved"},
				EvictionHard:                map[string]string{"eviction-key": "eviction"},
				EvictionSoft:                map[string]string{"eviction-key": "eviction"},
				EvictionSoftGracePeriod:     map[string]metav1.Duration{"test-soft-grace": {Duration: time.Hour}},
				EvictionMaxPodGracePeriod:   lo.ToPtr(int32(382902)),
				ImageGCHighThresholdPercent: lo.ToPtr(int32(382902)),
				CPUCFSQuota:                 lo.ToPtr(false),
			}
			Expect(v1NodeClaim.Annotations).To(BeNil())
			Expect(v1NodeClaim.ConvertFrom(ctx, v1beta1NodeClaim)).To(BeNil())
			kubelet := &v1beta1.KubeletConfiguration{}
			kubeletString, found := v1NodeClaim.Annotations[V1Beta1KubeletConfiguration]
			Expect(found).To(BeTrue())
			err := json.Unmarshal([]byte(kubeletString), kubelet)
			Expect(err).To(BeNil())
			Expect(kubelet.ClusterDNS).To(Equal(v1beta1NodeClaim.Spec.Kubelet.ClusterDNS))
			Expect(lo.FromPtr(kubelet.MaxPods)).To(Equal(lo.FromPtr(v1beta1NodeClaim.Spec.Kubelet.MaxPods)))
			Expect(lo.FromPtr(kubelet.PodsPerCore)).To(Equal(lo.FromPtr(v1beta1NodeClaim.Spec.Kubelet.PodsPerCore)))
			Expect(lo.FromPtr(kubelet.EvictionMaxPodGracePeriod)).To(Equal(lo.FromPtr(v1beta1NodeClaim.Spec.Kubelet.EvictionMaxPodGracePeriod)))
			Expect(lo.FromPtr(kubelet.ImageGCHighThresholdPercent)).To(Equal(lo.FromPtr(v1beta1NodeClaim.Spec.Kubelet.ImageGCHighThresholdPercent)))
			Expect(lo.FromPtr(kubelet.ImageGCHighThresholdPercent)).To(Equal(lo.FromPtr(v1beta1NodeClaim.Spec.Kubelet.ImageGCHighThresholdPercent)))
			Expect(lo.FromPtr(kubelet.ImageGCHighThresholdPercent)).To(Equal(lo.FromPtr(v1beta1NodeClaim.Spec.Kubelet.ImageGCHighThresholdPercent)))
			Expect(lo.FromPtr(kubelet.ImageGCHighThresholdPercent)).To(Equal(lo.FromPtr(v1beta1NodeClaim.Spec.Kubelet.ImageGCHighThresholdPercent)))
			Expect(kubelet.SystemReserved).To(Equal(v1beta1NodeClaim.Spec.Kubelet.SystemReserved))
			Expect(kubelet.KubeReserved).To(Equal(v1beta1NodeClaim.Spec.Kubelet.KubeReserved))
			Expect(kubelet.EvictionHard).To(Equal(v1beta1NodeClaim.Spec.Kubelet.EvictionHard))
			Expect(kubelet.EvictionSoft).To(Equal(v1beta1NodeClaim.Spec.Kubelet.EvictionSoft))
			Expect(kubelet.EvictionSoftGracePeriod).To(Equal(v1beta1NodeClaim.Spec.Kubelet.EvictionSoftGracePeriod))
			Expect(lo.FromPtr(kubelet.CPUCFSQuota)).To(Equal(lo.FromPtr(v1beta1NodeClaim.Spec.Kubelet.CPUCFSQuota)))
		})
		Context("NodeClassRef", func() {
			It("should convert v1beta1 nodeclaim template nodeClassRef", func() {
				v1beta1NodeClaim.Spec.NodeClassRef = &v1beta1.NodeClassReference{
					Kind:       "test-kind",
					Name:       "nodeclass-test",
					APIVersion: "testgroup.sh/testversion",
				}
				Expect(v1NodeClaim.Spec.NodeClassRef).To(BeNil())
				Expect(v1NodeClaim.ConvertFrom(ctx, v1beta1NodeClaim)).To(BeNil())
				Expect(v1NodeClaim.Spec.NodeClassRef.Kind).To(Equal(v1beta1NodeClaim.Spec.NodeClassRef.Kind))
				Expect(v1NodeClaim.Spec.NodeClassRef.Name).To(Equal(v1beta1NodeClaim.Spec.NodeClassRef.Name))
				Expect(v1NodeClaim.Spec.NodeClassRef.Group).To(Equal("testgroup.sh"))
			})
			It("should set default nodeclass group and kind on v1beta1 nodeclassRef", func() {
				v1beta1NodeClaim.Spec.NodeClassRef = &v1beta1.NodeClassReference{
					Name: "nodeclass-test",
				}
				Expect(v1NodeClaim.Spec.NodeClassRef).To(BeNil())
				Expect(v1NodeClaim.ConvertFrom(ctx, v1beta1NodeClaim)).To(BeNil())
				Expect(v1NodeClaim.Spec.NodeClassRef.Kind).To(Equal(cloudProvider.NodeClassGroupVersionKind[0].Kind))
				Expect(v1NodeClaim.Spec.NodeClassRef.Name).To(Equal(v1beta1NodeClaim.Spec.NodeClassRef.Name))
				Expect(v1NodeClaim.Spec.NodeClassRef.Group).To(Equal(cloudProvider.NodeClassGroupVersionKind[0].Group))
			})
		})
	})
	Context("NodeClaim Status", func() {
		It("should convert v1beta1 nodeclaim nodename", func() {
			v1beta1NodeClaim.Status.NodeName = "test-node-name"
			Expect(v1NodeClaim.Status.NodeName).To(Equal(""))
			Expect(v1NodeClaim.ConvertFrom(ctx, v1beta1NodeClaim)).To(BeNil())
			Expect(v1beta1NodeClaim.Status.NodeName).To(Equal(v1NodeClaim.Status.NodeName))
		})
		It("should convert v1beta1 nodeclaim provider id", func() {
			v1beta1NodeClaim.Status.ProviderID = "test-provider-id"
			Expect(v1NodeClaim.Status.ProviderID).To(Equal(""))
			Expect(v1NodeClaim.ConvertFrom(ctx, v1beta1NodeClaim)).To(BeNil())
			Expect(v1beta1NodeClaim.Status.ProviderID).To(Equal(v1NodeClaim.Status.ProviderID))
		})
		It("should convert v1beta1 nodeclaim image id", func() {
			v1beta1NodeClaim.Status.ImageID = "test-image-id"
			Expect(v1NodeClaim.Status.ImageID).To(Equal(""))
			Expect(v1NodeClaim.ConvertFrom(ctx, v1beta1NodeClaim)).To(BeNil())
			Expect(v1beta1NodeClaim.Status.ImageID).To(Equal(v1NodeClaim.Status.ImageID))
		})
		It("should convert v1beta1 nodeclaim capacity", func() {
			v1beta1NodeClaim.Status.Capacity = v1.ResourceList{
				v1.ResourceCPU:    resource.MustParse("13432"),
				v1.ResourceMemory: resource.MustParse("1332G"),
			}
			Expect(v1NodeClaim.Status.Capacity).To(BeNil())
			Expect(v1NodeClaim.ConvertFrom(ctx, v1beta1NodeClaim)).To(BeNil())
			Expect(v1beta1NodeClaim.Status.Capacity).To(Equal(v1NodeClaim.Status.Capacity))
		})
		It("should convert v1beta1 nodeclaim allocatable", func() {
			v1beta1NodeClaim.Status.Allocatable = v1.ResourceList{
				v1.ResourceCPU:    resource.MustParse("13432"),
				v1.ResourceMemory: resource.MustParse("1332G"),
			}
			Expect(v1NodeClaim.Status.Allocatable).To(BeNil())
			Expect(v1NodeClaim.ConvertFrom(ctx, v1beta1NodeClaim)).To(BeNil())
			Expect(v1beta1NodeClaim.Status.Allocatable).To(Equal(v1NodeClaim.Status.Allocatable))
		})
		It("should convert v1beta1 nodeclaim conditions", func() {
			v1beta1NodeClaim.Status.Conditions = []status.Condition{
				{
					Status: status.ConditionReady,
					Reason: "test-reason",
				},
				{
					Status: ConditionTypeDrifted,
					Reason: "test-reason",
				},
			}
			Expect(v1NodeClaim.Status.Conditions).To(BeNil())
			Expect(v1NodeClaim.ConvertFrom(ctx, v1beta1NodeClaim)).To(BeNil())
			Expect(v1beta1NodeClaim.Status.Conditions).To(Equal(v1NodeClaim.Status.Conditions))
		})
	})
})
