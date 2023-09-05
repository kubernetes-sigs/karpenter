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

package nodepool_test

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

	"sigs.k8s.io/controller-runtime/pkg/client"

	coreapis "github.com/aws/karpenter-core/pkg/apis"
	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/apis/v1beta1"
	"github.com/aws/karpenter-core/pkg/operator/scheme"
	"github.com/aws/karpenter-core/pkg/test"
	. "github.com/aws/karpenter-core/pkg/test/expectations"
	nodepoolutil "github.com/aws/karpenter-core/pkg/utils/nodepool"
)

var ctx context.Context
var env *test.Environment

func TestAPIs(t *testing.T) {
	ctx = TestContextWithLogger(t)
	RegisterFailHandler(Fail)
	RunSpecs(t, "NodePoolUtils")
}

var _ = BeforeSuite(func() {
	env = test.NewEnvironment(scheme.Scheme, test.WithCRDs(coreapis.CRDs...))
})

var _ = AfterSuite(func() {
	Expect(env.Stop()).To(Succeed(), "Failed to stop environment")
})

var _ = AfterEach(func() {
	ExpectCleanedUp(ctx, env.Client)
})

var _ = Describe("NodePoolUtils", func() {
	var provisioner *v1alpha5.Provisioner
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
	})
	It("should convert a Provisioner to a NodePool", func() {
		nodePool := nodepoolutil.New(provisioner)
		for k, v := range provisioner.Annotations {
			Expect(nodePool.Annotations).To(HaveKeyWithValue(k, v))
		}
		for k, v := range provisioner.Labels {
			Expect(nodePool.Labels).To(HaveKeyWithValue(k, v))
		}
		for k, v := range provisioner.Spec.Annotations {
			Expect(nodePool.Spec.Template.Annotations).To(HaveKeyWithValue(k, v))
		}
		for k, v := range provisioner.Spec.Labels {
			Expect(nodePool.Spec.Template.Labels).To(HaveKeyWithValue(k, v))
		}

		Expect(nodePool.Spec.Template.Spec.Taints).To(Equal(provisioner.Spec.Taints))
		Expect(nodePool.Spec.Template.Spec.StartupTaints).To(Equal(provisioner.Spec.StartupTaints))
		Expect(nodePool.Spec.Template.Spec.Requirements).To(Equal(provisioner.Spec.Requirements))

		Expect(nodePool.Spec.Template.Spec.KubeletConfiguration.ClusterDNS).To(Equal(provisioner.Spec.KubeletConfiguration.ClusterDNS))
		Expect(nodePool.Spec.Template.Spec.KubeletConfiguration.ContainerRuntime).To(Equal(provisioner.Spec.KubeletConfiguration.ContainerRuntime))
		Expect(nodePool.Spec.Template.Spec.KubeletConfiguration.MaxPods).To(Equal(provisioner.Spec.KubeletConfiguration.MaxPods))
		Expect(nodePool.Spec.Template.Spec.KubeletConfiguration.PodsPerCore).To(Equal(provisioner.Spec.KubeletConfiguration.PodsPerCore))
		Expect(nodePool.Spec.Template.Spec.KubeletConfiguration.SystemReserved).To(Equal(provisioner.Spec.KubeletConfiguration.SystemReserved))
		Expect(nodePool.Spec.Template.Spec.KubeletConfiguration.KubeReserved).To(Equal(provisioner.Spec.KubeletConfiguration.KubeReserved))
		Expect(nodePool.Spec.Template.Spec.KubeletConfiguration.EvictionHard).To(Equal(provisioner.Spec.KubeletConfiguration.EvictionHard))
		Expect(nodePool.Spec.Template.Spec.KubeletConfiguration.EvictionSoft).To(Equal(provisioner.Spec.KubeletConfiguration.EvictionSoft))
		Expect(nodePool.Spec.Template.Spec.KubeletConfiguration.EvictionSoftGracePeriod).To(Equal(provisioner.Spec.KubeletConfiguration.EvictionSoftGracePeriod))
		Expect(nodePool.Spec.Template.Spec.KubeletConfiguration.EvictionMaxPodGracePeriod).To(Equal(provisioner.Spec.KubeletConfiguration.EvictionMaxPodGracePeriod))
		Expect(nodePool.Spec.Template.Spec.KubeletConfiguration.ImageGCHighThresholdPercent).To(Equal(provisioner.Spec.KubeletConfiguration.ImageGCHighThresholdPercent))
		Expect(nodePool.Spec.Template.Spec.KubeletConfiguration.ImageGCLowThresholdPercent).To(Equal(provisioner.Spec.KubeletConfiguration.ImageGCLowThresholdPercent))
		Expect(nodePool.Spec.Template.Spec.KubeletConfiguration.CPUCFSQuota).To(Equal(provisioner.Spec.KubeletConfiguration.CPUCFSQuota))

		Expect(nodePool.Spec.Template.Spec.NodeClass.Kind).To(Equal(provisioner.Spec.ProviderRef.Kind))
		Expect(nodePool.Spec.Template.Spec.NodeClass.APIVersion).To(Equal(provisioner.Spec.ProviderRef.APIVersion))
		Expect(nodePool.Spec.Template.Spec.NodeClass.Name).To(Equal(provisioner.Spec.ProviderRef.Name))

		Expect(nodePool.Spec.Disruption.ConsolidationPolicy).To(Equal(v1beta1.ConsolidationPolicyWhenUnderutilized))
		Expect(nodePool.Spec.Disruption.ExpireAfter.Duration.Seconds()).To(BeNumerically("==", lo.FromPtr(provisioner.Spec.TTLSecondsUntilExpired)))
		Expect(nodePool.Spec.Disruption.ConsolidateAfter.Duration).To(BeNil())

		ExpectResources(v1.ResourceList(nodePool.Spec.Limits), provisioner.Spec.Limits.Resources)
		Expect(lo.FromPtr(nodePool.Spec.Weight)).To(BeNumerically("==", lo.FromPtr(provisioner.Spec.Weight)))
	})
	It("should convert a Provisioner to a NodePool (with Provider)", func() {
		provisioner.Spec.Provider = &runtime.RawExtension{Raw: lo.Must(json.Marshal(map[string]string{
			"test-key":  "test-value",
			"test-key2": "test-value2",
		}))}
		provisioner.Spec.ProviderRef = nil

		nodePool := nodepoolutil.New(provisioner)
		Expect(nodePool.Spec.Template.Spec.Provider).To(Equal(provisioner.Spec.Provider))
	})
	It("should retrieve both NodePools and Provisioners on a list call", func() {
		Skip("Re-enable this test when NodePools are enabled and v1beta1 is released")

		numNodePools := 3
		numProvisioners := 5

		for i := 0; i < numNodePools; i++ {
			ExpectApplied(ctx, env.Client, test.NodePool(v1beta1.NodePool{
				Spec: v1beta1.NodePoolSpec{
					Template: v1beta1.NodeClaimTemplate{
						Spec: v1beta1.NodeClaimSpec{
							NodeClass: &v1beta1.NodeClassReference{
								Kind:       "NodeClass",
								APIVersion: "test.cloudprovider/v1",
								Name:       "default",
							},
						},
					},
				},
			}))
		}
		for i := 0; i < numProvisioners; i++ {
			ExpectApplied(ctx, env.Client, test.Provisioner())
		}

		retrieved, err := nodepoolutil.List(ctx, env.Client)
		Expect(err).ToNot(HaveOccurred())
		Expect(retrieved.Items).To(HaveLen(8))
	})
	It("should patch the status on a NodePool", func() {
		nodePool := test.NodePool(v1beta1.NodePool{
			Spec: v1beta1.NodePoolSpec{
				Template: v1beta1.NodeClaimTemplate{
					Spec: v1beta1.NodeClaimSpec{
						NodeClass: &v1beta1.NodeClassReference{
							Kind:       "NodeClass",
							APIVersion: "test.cloudprovider/v1",
							Name:       "default",
						},
					},
				},
			},
		})
		ExpectApplied(ctx, env.Client, nodePool)

		stored := nodePool.DeepCopy()
		nodePool.Status.Resources = v1.ResourceList{
			v1.ResourceCPU:              resource.MustParse("10"),
			v1.ResourceMemory:           resource.MustParse("10Mi"),
			v1.ResourceEphemeralStorage: resource.MustParse("100Gi"),
		}
		Expect(nodepoolutil.PatchStatus(ctx, env.Client, stored, nodePool)).To(Succeed())

		retrieved := &v1beta1.NodePool{}
		Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(nodePool), retrieved)).To(Succeed())
		ExpectResources(retrieved.Status.Resources, nodePool.Status.Resources)
	})
	It("should patch the status on a Provisioner", func() {
		provisioner := test.Provisioner()
		ExpectApplied(ctx, env.Client, provisioner)

		nodePool := nodepoolutil.New(provisioner)
		stored := nodePool.DeepCopy()
		nodePool.Status.Resources = v1.ResourceList{
			v1.ResourceCPU:              resource.MustParse("10"),
			v1.ResourceMemory:           resource.MustParse("10Mi"),
			v1.ResourceEphemeralStorage: resource.MustParse("100Gi"),
		}
		Expect(nodepoolutil.PatchStatus(ctx, env.Client, stored, nodePool)).To(Succeed())

		retrieved := &v1alpha5.Provisioner{}
		Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(provisioner), retrieved)).To(Succeed())
		ExpectResources(retrieved.Status.Resources, nodePool.Status.Resources)
	})
})
