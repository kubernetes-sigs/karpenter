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

package provisioner_test

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
	. "knative.dev/pkg/logging/testing"
	"knative.dev/pkg/ptr"

	. "github.com/aws/karpenter-core/pkg/test/expectations"

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/apis/v1beta1"
	"github.com/aws/karpenter-core/pkg/test"
	provisionerutil "github.com/aws/karpenter-core/pkg/utils/provisioner"
)

var ctx context.Context

func TestAPIs(t *testing.T) {
	ctx = TestContextWithLogger(t)
	RegisterFailHandler(Fail)
	RunSpecs(t, "ProvisionerUtils")
}

var _ = Describe("ProvisionerUtils", func() {
	var nodePool *v1beta1.NodePool
	BeforeEach(func() {
		nodePool = test.NodePool(v1beta1.NodePool{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					"top-level-annotation": "top-level-annotation-value",
				},
				Labels: map[string]string{
					"top-level-label": "top-level-label-value",
				},
			},
			Spec: v1beta1.NodePoolSpec{
				Template: v1beta1.NodeClaimTemplate{
					ObjectMeta: metav1.ObjectMeta{
						Annotations: map[string]string{
							"test-annotation-key":  "test-annotation-value",
							"test-annotation-key2": "test-annotation-value2",
						},
						Labels: map[string]string{
							"test-label-key":  "test-label-value",
							"test-label-key2": "test-label-value2",
						},
					},
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
								v1.ResourceCPU:              resource.MustParse("10"),
								v1.ResourceMemory:           resource.MustParse("10Mi"),
								v1.ResourceEphemeralStorage: resource.MustParse("100Gi"),
							},
						},
						Kubelet: &v1beta1.KubeletConfiguration{
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
				},
				Disruption: v1beta1.Disruption{
					ConsolidationPolicy: v1beta1.ConsolidationPolicyWhenUnderutilized,
					ExpireAfter:         v1beta1.NillableDuration{Duration: lo.ToPtr(lo.Must(time.ParseDuration("2160h")))},
				},
				Limits: v1beta1.Limits(v1.ResourceList{
					v1.ResourceCPU:              resource.MustParse("10"),
					v1.ResourceMemory:           resource.MustParse("10Mi"),
					v1.ResourceEphemeralStorage: resource.MustParse("1000Gi"),
				}),
				Weight: lo.ToPtr[int32](100),
			},
		})
	})
	It("should convert a Provisioner to a NodePool", func() {
		provisioner := provisionerutil.New(nodePool)
		for k, v := range nodePool.Annotations {
			Expect(provisioner.Annotations).To(HaveKeyWithValue(k, v))
		}
		for k, v := range nodePool.Labels {
			Expect(provisioner.Labels).To(HaveKeyWithValue(k, v))
		}
		for k, v := range nodePool.Spec.Template.Annotations {
			Expect(provisioner.Spec.Annotations).To(HaveKeyWithValue(k, v))
		}
		for k, v := range nodePool.Spec.Template.Labels {
			Expect(provisioner.Spec.Labels).To(HaveKeyWithValue(k, v))
		}

		Expect(provisioner.Spec.Taints).To(Equal(nodePool.Spec.Template.Spec.Taints))
		Expect(provisioner.Spec.StartupTaints).To(Equal(nodePool.Spec.Template.Spec.StartupTaints))
		Expect(provisioner.Spec.Requirements).To(Equal(nodePool.Spec.Template.Spec.Requirements))

		Expect(provisioner.Spec.KubeletConfiguration.ClusterDNS).To(Equal(nodePool.Spec.Template.Spec.Kubelet.ClusterDNS))
		Expect(provisioner.Spec.KubeletConfiguration.ContainerRuntime).To(Equal(nodePool.Spec.Template.Spec.Kubelet.ContainerRuntime))
		Expect(provisioner.Spec.KubeletConfiguration.MaxPods).To(Equal(nodePool.Spec.Template.Spec.Kubelet.MaxPods))
		Expect(provisioner.Spec.KubeletConfiguration.PodsPerCore).To(Equal(nodePool.Spec.Template.Spec.Kubelet.PodsPerCore))
		Expect(provisioner.Spec.KubeletConfiguration.SystemReserved).To(Equal(nodePool.Spec.Template.Spec.Kubelet.SystemReserved))
		Expect(provisioner.Spec.KubeletConfiguration.KubeReserved).To(Equal(nodePool.Spec.Template.Spec.Kubelet.KubeReserved))
		Expect(provisioner.Spec.KubeletConfiguration.EvictionHard).To(Equal(nodePool.Spec.Template.Spec.Kubelet.EvictionHard))
		Expect(provisioner.Spec.KubeletConfiguration.EvictionSoft).To(Equal(nodePool.Spec.Template.Spec.Kubelet.EvictionSoft))
		Expect(provisioner.Spec.KubeletConfiguration.EvictionSoftGracePeriod).To(Equal(nodePool.Spec.Template.Spec.Kubelet.EvictionSoftGracePeriod))
		Expect(provisioner.Spec.KubeletConfiguration.EvictionMaxPodGracePeriod).To(Equal(nodePool.Spec.Template.Spec.Kubelet.EvictionMaxPodGracePeriod))
		Expect(provisioner.Spec.KubeletConfiguration.ImageGCHighThresholdPercent).To(Equal(nodePool.Spec.Template.Spec.Kubelet.ImageGCHighThresholdPercent))
		Expect(provisioner.Spec.KubeletConfiguration.ImageGCLowThresholdPercent).To(Equal(nodePool.Spec.Template.Spec.Kubelet.ImageGCLowThresholdPercent))
		Expect(provisioner.Spec.KubeletConfiguration.CPUCFSQuota).To(Equal(nodePool.Spec.Template.Spec.Kubelet.CPUCFSQuota))

		Expect(provisioner.Spec.ProviderRef.Kind).To(Equal(nodePool.Spec.Template.Spec.NodeClass.Kind))
		Expect(provisioner.Spec.ProviderRef.APIVersion).To(Equal(nodePool.Spec.Template.Spec.NodeClass.APIVersion))
		Expect(provisioner.Spec.ProviderRef.Name).To(Equal(nodePool.Spec.Template.Spec.NodeClass.Name))

		Expect(provisioner.Spec.Consolidation).ToNot(BeNil())
		Expect(provisioner.Spec.Consolidation.Enabled).ToNot(BeNil())
		Expect(lo.FromPtr(provisioner.Spec.Consolidation.Enabled)).To(BeTrue())

		Expect(lo.FromPtr(provisioner.Spec.TTLSecondsUntilExpired)).To(BeNumerically("==", nodePool.Spec.Disruption.ExpireAfter.Duration.Seconds()))
		Expect(provisioner.Spec.TTLSecondsAfterEmpty).To(BeNil())

		ExpectResources(provisioner.Spec.Limits.Resources, v1.ResourceList(nodePool.Spec.Limits))
		Expect(lo.FromPtr(provisioner.Spec.Weight)).To(BeNumerically("==", lo.FromPtr(nodePool.Spec.Weight)))

		ExpectResources(provisioner.Status.Resources, nodePool.Status.Resources)
	})
	It("should convert a Provisioner to a NodePool (with Provider)", func() {
		nodePool.Spec.Template.Spec.Provider = &runtime.RawExtension{Raw: lo.Must(json.Marshal(map[string]string{
			"test-key":  "test-value",
			"test-key2": "test-value2",
		}))}
		nodePool.Spec.Template.Spec.NodeClass = nil

		provisioner := provisionerutil.New(nodePool)
		Expect(provisioner.Spec.Provider).To(Equal(nodePool.Spec.Template.Spec.Provider))
	})
})
