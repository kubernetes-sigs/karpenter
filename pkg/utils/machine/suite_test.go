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

package machine_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"knative.dev/pkg/apis"
	. "knative.dev/pkg/logging/testing"
	"knative.dev/pkg/ptr"

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/apis/v1beta1"
	"github.com/aws/karpenter-core/pkg/scheduling"
	"github.com/aws/karpenter-core/pkg/test"
	machineutil "github.com/aws/karpenter-core/pkg/utils/machine"
)

var ctx context.Context

func TestAPIs(t *testing.T) {
	ctx = TestContextWithLogger(t)
	RegisterFailHandler(Fail)
	RunSpecs(t, "MachineUtils")
}

var _ = Describe("MachineUtils", func() {
	var provisioner *v1alpha5.Provisioner
	var node *v1.Node
	var nodeClaim *v1beta1.NodeClaim
	BeforeEach(func() {
		provisioner = test.Provisioner(test.ProvisionerOptions{
			Limits: v1.ResourceList{
				v1.ResourceCPU:              resource.MustParse("10"),
				v1.ResourceMemory:           resource.MustParse("10Mi"),
				v1.ResourceEphemeralStorage: resource.MustParse("1000Gi"),
			},
			ProviderRef: &v1alpha5.MachineTemplateRef{
				Kind:       "MachineTemplate",
				APIVersion: "test.cloudprovider/v1",
				Name:       "default",
			},
			Kubelet: &v1alpha5.KubeletConfiguration{
				ContainerRuntime: ptr.String("containerd"),
				MaxPods:          ptr.Int32(110),
				PodsPerCore:      ptr.Int32(10),
				SystemReserved: v1.ResourceList{
					v1.ResourceCPU:              resource.MustParse("200m"),
					v1.ResourceMemory:           resource.MustParse("200Mi"),
					v1.ResourceEphemeralStorage: resource.MustParse("1Gi"),
				},
				KubeReserved: v1.ResourceList{
					v1.ResourceCPU:              resource.MustParse("200m"),
					v1.ResourceMemory:           resource.MustParse("200Mi"),
					v1.ResourceEphemeralStorage: resource.MustParse("1Gi"),
				},
				EvictionHard: map[string]string{
					"memory.available":   "5%",
					"nodefs.available":   "5%",
					"nodefs.inodesFree":  "5%",
					"imagefs.available":  "5%",
					"imagefs.inodesFree": "5%",
					"pid.available":      "3%",
				},
				EvictionSoft: map[string]string{
					"memory.available":   "10%",
					"nodefs.available":   "10%",
					"nodefs.inodesFree":  "10%",
					"imagefs.available":  "10%",
					"imagefs.inodesFree": "10%",
					"pid.available":      "6%",
				},
				EvictionSoftGracePeriod: map[string]metav1.Duration{
					"memory.available":   {Duration: time.Minute * 2},
					"nodefs.available":   {Duration: time.Minute * 2},
					"nodefs.inodesFree":  {Duration: time.Minute * 2},
					"imagefs.available":  {Duration: time.Minute * 2},
					"imagefs.inodesFree": {Duration: time.Minute * 2},
					"pid.available":      {Duration: time.Minute * 2},
				},
				EvictionMaxPodGracePeriod:   ptr.Int32(120),
				ImageGCHighThresholdPercent: ptr.Int32(50),
				ImageGCLowThresholdPercent:  ptr.Int32(10),
				CPUCFSQuota:                 ptr.Bool(false),
			},
			Annotations: map[string]string{
				"test-annotation-key":  "test-annotation-value",
				"test-annotation-key2": "test-annotation-value2",
			},
			Labels: map[string]string{
				"test-label-key":  "test-label-value",
				"test-label-key2": "test-label-value2",
			},
			Taints: []v1.Taint{
				{
					Key:    "test-taint-key",
					Effect: v1.TaintEffectNoSchedule,
					Value:  "test-taint-value",
				},
				{
					Key:    "test-taint-key2",
					Effect: v1.TaintEffectNoExecute,
					Value:  "test-taint-value2",
				},
			},
			StartupTaints: []v1.Taint{
				{
					Key:    "test-startup-taint-key",
					Effect: v1.TaintEffectNoSchedule,
					Value:  "test-startup-taint-value",
				},
				{
					Key:    "test-startup-taint-key2",
					Effect: v1.TaintEffectNoExecute,
					Value:  "test-startup-taint-value2",
				},
			},
			Requirements: []v1.NodeSelectorRequirement{
				{
					Key:      v1.LabelTopologyZone,
					Operator: v1.NodeSelectorOpIn,
					Values:   []string{"test-zone-1", "test-zone-2"},
				},
				{
					Key:      v1alpha5.LabelCapacityType,
					Operator: v1.NodeSelectorOpIn,
					Values:   []string{v1alpha5.CapacityTypeOnDemand},
				},
				{
					Key:      v1.LabelHostname,
					Operator: v1.NodeSelectorOpExists,
				},
			},
			TTLSecondsUntilExpired: lo.ToPtr[int64](2592000),
			TTLSecondsAfterEmpty:   lo.ToPtr[int64](30),
			Consolidation: &v1alpha5.Consolidation{
				Enabled: lo.ToPtr(true),
			},
			Weight: lo.ToPtr[int32](100),
			Status: v1alpha5.ProvisionerStatus{
				Resources: v1.ResourceList{
					v1.ResourceCPU:              resource.MustParse("5"),
					v1.ResourceMemory:           resource.MustParse("5Mi"),
					v1.ResourceEphemeralStorage: resource.MustParse("500Gi"),
				},
				LastScaleTime: &apis.VolatileTime{Inner: metav1.Time{Time: time.Now()}},
			},
		})
		provisioner.Spec.Provider = nil
		node = test.Node(test.NodeOptions{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1.LabelTopologyZone:             "test-zone-1",
					v1.LabelTopologyRegion:           "test-region",
					"test-label-key":                 "test-label-value",
					"test-label-key2":                "test-label-value2",
					v1alpha5.LabelNodeRegistered:     "true",
					v1alpha5.LabelNodeInitialized:    "true",
					v1alpha5.ProvisionerNameLabelKey: provisioner.Name,
					v1alpha5.LabelCapacityType:       v1alpha5.CapacityTypeOnDemand,
					v1.LabelOSStable:                 "linux",
					v1.LabelInstanceTypeStable:       "test-instance-type",
				},
				Annotations: map[string]string{
					"test-annotation-key":        "test-annotation-value",
					"test-annotation-key2":       "test-annotation-value2",
					"node-custom-annotation-key": "node-custom-annotation-value",
				},
			},
			ReadyStatus: v1.ConditionTrue,
			Taints: []v1.Taint{
				{
					Key:    "test-taint-key",
					Effect: v1.TaintEffectNoSchedule,
					Value:  "test-taint-value",
				},
				{
					Key:    "test-taint-key2",
					Effect: v1.TaintEffectNoExecute,
					Value:  "test-taint-value2",
				},
			},
			ProviderID: test.RandomProviderID(),
			Capacity: v1.ResourceList{
				v1.ResourceCPU:              resource.MustParse("10"),
				v1.ResourceMemory:           resource.MustParse("10Mi"),
				v1.ResourceEphemeralStorage: resource.MustParse("100Gi"),
			},
			Allocatable: v1.ResourceList{
				v1.ResourceCPU:              resource.MustParse("8"),
				v1.ResourceMemory:           resource.MustParse("8Mi"),
				v1.ResourceEphemeralStorage: resource.MustParse("95Gi"),
			},
		})
		nodeClaim = test.NodeClaim(v1beta1.NodeClaim{
			Spec: v1beta1.NodeClaimSpec{
				Taints: []v1.Taint{
					{
						Key:    "test-taint-key",
						Effect: v1.TaintEffectNoSchedule,
						Value:  "test-taint-value",
					},
					{
						Key:    "test-taint-key2",
						Effect: v1.TaintEffectNoExecute,
						Value:  "test-taint-value2",
					},
				},
				StartupTaints: []v1.Taint{
					{
						Key:    "test-startup-taint-key",
						Effect: v1.TaintEffectNoSchedule,
						Value:  "test-startup-taint-value",
					},
					{
						Key:    "test-startup-taint-key2",
						Effect: v1.TaintEffectNoExecute,
						Value:  "test-startup-taint-value2",
					},
				},
				Requirements: []v1.NodeSelectorRequirement{
					{
						Key:      v1.LabelTopologyZone,
						Operator: v1.NodeSelectorOpIn,
						Values:   []string{"test-zone-1", "test-zone-2"},
					},
					{
						Key:      v1alpha5.LabelCapacityType,
						Operator: v1.NodeSelectorOpIn,
						Values:   []string{v1alpha5.CapacityTypeOnDemand},
					},
					{
						Key:      v1.LabelHostname,
						Operator: v1.NodeSelectorOpExists,
					},
				},
				Resources: v1beta1.ResourceRequirements{
					Requests: v1.ResourceList{
						v1.ResourceCPU:              resource.MustParse("5"),
						v1.ResourceMemory:           resource.MustParse("5Mi"),
						v1.ResourceEphemeralStorage: resource.MustParse("100Gi"),
					},
				},
				KubeletConfiguration: &v1beta1.Kubelet{
					ContainerRuntime: ptr.String("containerd"),
					MaxPods:          ptr.Int32(110),
					PodsPerCore:      ptr.Int32(10),
					SystemReserved: v1.ResourceList{
						v1.ResourceCPU:              resource.MustParse("200m"),
						v1.ResourceMemory:           resource.MustParse("200Mi"),
						v1.ResourceEphemeralStorage: resource.MustParse("1Gi"),
					},
					KubeReserved: v1.ResourceList{
						v1.ResourceCPU:              resource.MustParse("200m"),
						v1.ResourceMemory:           resource.MustParse("200Mi"),
						v1.ResourceEphemeralStorage: resource.MustParse("1Gi"),
					},
					EvictionHard: map[string]string{
						"memory.available":   "5%",
						"nodefs.available":   "5%",
						"nodefs.inodesFree":  "5%",
						"imagefs.available":  "5%",
						"imagefs.inodesFree": "5%",
						"pid.available":      "3%",
					},
					EvictionSoft: map[string]string{
						"memory.available":   "10%",
						"nodefs.available":   "10%",
						"nodefs.inodesFree":  "10%",
						"imagefs.available":  "10%",
						"imagefs.inodesFree": "10%",
						"pid.available":      "6%",
					},
					EvictionSoftGracePeriod: map[string]metav1.Duration{
						"memory.available":   {Duration: time.Minute * 2},
						"nodefs.available":   {Duration: time.Minute * 2},
						"nodefs.inodesFree":  {Duration: time.Minute * 2},
						"imagefs.available":  {Duration: time.Minute * 2},
						"imagefs.inodesFree": {Duration: time.Minute * 2},
						"pid.available":      {Duration: time.Minute * 2},
					},
					EvictionMaxPodGracePeriod:   ptr.Int32(120),
					ImageGCHighThresholdPercent: ptr.Int32(50),
					ImageGCLowThresholdPercent:  ptr.Int32(10),
					CPUCFSQuota:                 ptr.Bool(false),
				},
				NodeClass: &v1beta1.NodeClassReference{
					Kind:       "NodeClass",
					APIVersion: "test.cloudprovider/v1",
					Name:       "default",
				},
			},
			Status: v1beta1.NodeClaimStatus{
				NodeName:    node.Name,
				ProviderID:  node.Spec.ProviderID,
				Capacity:    node.Status.Capacity,
				Allocatable: node.Status.Allocatable,
			},
		})
		nodeClaim.StatusConditions().MarkTrue(v1beta1.Launched)
		nodeClaim.StatusConditions().MarkTrue(v1beta1.Registered)
		nodeClaim.StatusConditions().MarkTrue(v1beta1.Initialized)
	})
	It("should convert a Node and a Provisioner to a Machine", func() {
		machine := machineutil.New(node, provisioner)
		for k, v := range provisioner.Spec.Annotations {
			Expect(machine.Annotations).To(HaveKeyWithValue(k, v))
		}
		for k, v := range provisioner.Spec.Labels {
			Expect(machine.Labels).To(HaveKeyWithValue(k, v))
		}
		Expect(machine.OwnerReferences).To(ContainElement(metav1.OwnerReference{
			APIVersion:         v1alpha5.SchemeGroupVersion.String(),
			Kind:               "Provisioner",
			Name:               provisioner.Name,
			UID:                provisioner.UID,
			BlockOwnerDeletion: ptr.Bool(true),
		}))
		Expect(lo.Contains(machine.Finalizers, v1alpha5.TerminationFinalizer)).To(BeTrue())
		Expect(machine.Spec.Kubelet).To(Equal(provisioner.Spec.KubeletConfiguration))
		Expect(machine.Spec.Taints).To(Equal(provisioner.Spec.Taints))
		Expect(machine.Spec.StartupTaints).To(Equal(provisioner.Spec.StartupTaints))
		Expect(machine.Spec.Requirements).To(Equal(provisioner.Spec.Requirements))
		Expect(machine.Spec.MachineTemplateRef).To(Equal(provisioner.Spec.ProviderRef))
		Expect(machine.Status.NodeName).To(Equal(node.Name))
		Expect(machine.Status.ProviderID).To(Equal(node.Spec.ProviderID))
		Expect(machine.Status.Capacity).To(Equal(node.Status.Capacity))
		Expect(machine.Status.Allocatable).To(Equal(node.Status.Allocatable))
		Expect(machine.StatusConditions().GetCondition(v1alpha5.MachineLaunched).IsTrue()).To(BeTrue())
		Expect(machine.StatusConditions().GetCondition(v1alpha5.MachineRegistered).IsTrue()).To(BeTrue())
		Expect(machine.StatusConditions().GetCondition(v1alpha5.MachineInitialized).IsTrue()).To(BeTrue())
	})
	It("should convert a Node and a Provisioner to a Machine (with Provider)", func() {
		provisioner.Spec.Provider = &runtime.RawExtension{Raw: lo.Must(json.Marshal(map[string]string{
			"test-key":  "test-value",
			"test-key2": "test-value2",
		}))}
		provisioner.Spec.ProviderRef = nil

		machine := machineutil.New(node, provisioner)
		Expect(machine.Annotations).To(HaveKeyWithValue(v1alpha5.ProviderCompatabilityAnnotationKey, string(lo.Must(json.Marshal(provisioner.Spec.Provider)))))
	})
	It("should convert a Node to a Machine", func() {
		machine := machineutil.NewFromNode(node)
		for k, v := range node.Annotations {
			Expect(machine.Annotations).To(HaveKeyWithValue(k, v))
		}
		for k, v := range node.Labels {
			Expect(machine.Labels).To(HaveKeyWithValue(k, v))
		}
		Expect(lo.Contains(machine.Finalizers, v1alpha5.TerminationFinalizer)).To(BeTrue())
		Expect(machine.Spec.Taints).To(Equal(node.Spec.Taints))
		Expect(machine.Spec.Requirements).To(ContainElements(scheduling.NewLabelRequirements(node.Labels).NodeSelectorRequirements()))
		Expect(machine.Spec.Resources.Requests).To(Equal(node.Status.Allocatable))
		Expect(machine.Status.NodeName).To(Equal(node.Name))
		Expect(machine.Status.ProviderID).To(Equal(node.Spec.ProviderID))
		Expect(machine.Status.Capacity).To(Equal(node.Status.Capacity))
		Expect(machine.Status.Allocatable).To(Equal(node.Status.Allocatable))
		Expect(machine.StatusConditions().GetCondition(v1alpha5.MachineLaunched).IsTrue()).To(BeTrue())
		Expect(machine.StatusConditions().GetCondition(v1alpha5.MachineRegistered).IsTrue()).To(BeTrue())
		Expect(machine.StatusConditions().GetCondition(v1alpha5.MachineInitialized).IsTrue()).To(BeTrue())
	})
	It("should convert a NodeClaim to a Machine", func() {
		machine := machineutil.NewFromNodeClaim(nodeClaim)

		for k, v := range nodeClaim.Annotations {
			Expect(machine.Annotations).To(HaveKeyWithValue(k, v))
		}
		for k, v := range nodeClaim.Labels {
			Expect(machine.Labels).To(HaveKeyWithValue(k, v))
		}
		Expect(machine.Spec.Taints).To(Equal(nodeClaim.Spec.Taints))
		Expect(machine.Spec.StartupTaints).To(Equal(nodeClaim.Spec.StartupTaints))
		Expect(machine.Spec.Requirements).To(ContainElements(nodeClaim.Spec.Requirements))
		Expect(machine.Spec.Resources.Requests).To(Equal(nodeClaim.Spec.Resources.Requests))

		Expect(machine.Spec.Kubelet.ClusterDNS).To(Equal(nodeClaim.Spec.KubeletConfiguration.ClusterDNS))
		Expect(machine.Spec.Kubelet.ContainerRuntime).To(Equal(nodeClaim.Spec.KubeletConfiguration.ContainerRuntime))
		Expect(machine.Spec.Kubelet.MaxPods).To(Equal(nodeClaim.Spec.KubeletConfiguration.MaxPods))
		Expect(machine.Spec.Kubelet.PodsPerCore).To(Equal(nodeClaim.Spec.KubeletConfiguration.PodsPerCore))
		Expect(machine.Spec.Kubelet.SystemReserved).To(Equal(nodeClaim.Spec.KubeletConfiguration.SystemReserved))
		Expect(machine.Spec.Kubelet.KubeReserved).To(Equal(nodeClaim.Spec.KubeletConfiguration.KubeReserved))
		Expect(machine.Spec.Kubelet.EvictionHard).To(Equal(nodeClaim.Spec.KubeletConfiguration.EvictionHard))
		Expect(machine.Spec.Kubelet.EvictionSoft).To(Equal(nodeClaim.Spec.KubeletConfiguration.EvictionSoft))
		Expect(machine.Spec.Kubelet.EvictionSoftGracePeriod).To(Equal(nodeClaim.Spec.KubeletConfiguration.EvictionSoftGracePeriod))
		Expect(machine.Spec.Kubelet.EvictionMaxPodGracePeriod).To(Equal(nodeClaim.Spec.KubeletConfiguration.EvictionMaxPodGracePeriod))
		Expect(machine.Spec.Kubelet.ImageGCHighThresholdPercent).To(Equal(nodeClaim.Spec.KubeletConfiguration.ImageGCHighThresholdPercent))
		Expect(machine.Spec.Kubelet.ImageGCLowThresholdPercent).To(Equal(nodeClaim.Spec.KubeletConfiguration.ImageGCLowThresholdPercent))
		Expect(machine.Spec.Kubelet.CPUCFSQuota).To(Equal(nodeClaim.Spec.KubeletConfiguration.CPUCFSQuota))

		Expect(machine.Spec.MachineTemplateRef.Kind).To(Equal(nodeClaim.Spec.NodeClass.Kind))
		Expect(machine.Spec.MachineTemplateRef.APIVersion).To(Equal(nodeClaim.Spec.NodeClass.APIVersion))
		Expect(machine.Spec.MachineTemplateRef.Name).To(Equal(nodeClaim.Spec.NodeClass.Name))

		Expect(machine.Status.NodeName).To(Equal(nodeClaim.Status.NodeName))
		Expect(machine.Status.ProviderID).To(Equal(nodeClaim.Status.ProviderID))
		Expect(machine.Status.Capacity).To(Equal(nodeClaim.Status.Capacity))
		Expect(machine.Status.Allocatable).To(Equal(nodeClaim.Status.Allocatable))
		Expect(machine.StatusConditions().GetCondition(v1alpha5.MachineLaunched).IsTrue()).To(BeTrue())
		Expect(machine.StatusConditions().GetCondition(v1alpha5.MachineRegistered).IsTrue()).To(BeTrue())
		Expect(machine.StatusConditions().GetCondition(v1alpha5.MachineInitialized).IsTrue()).To(BeTrue())
	})
})
