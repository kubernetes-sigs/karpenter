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

package nodeclaim_test

import (
	"context"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	. "knative.dev/pkg/logging/testing"
	"knative.dev/pkg/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"

	"github.com/aws/karpenter-core/pkg/apis"
	"github.com/aws/karpenter-core/pkg/operator/scheme"
	. "github.com/aws/karpenter-core/pkg/test/expectations"

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/apis/v1beta1"
	"github.com/aws/karpenter-core/pkg/scheduling"
	"github.com/aws/karpenter-core/pkg/test"
	nodeclaimutil "github.com/aws/karpenter-core/pkg/utils/nodeclaim"
)

var ctx context.Context
var env *test.Environment

func TestAPIs(t *testing.T) {
	ctx = TestContextWithLogger(t)
	RegisterFailHandler(Fail)
	RunSpecs(t, "NodeClaimUtils")
}

var _ = BeforeSuite(func() {
	env = test.NewEnvironment(scheme.Scheme, test.WithCRDs(apis.CRDs...))
})

var _ = AfterSuite(func() {
	Expect(env.Stop()).To(Succeed(), "Failed to stop environment")
})

var _ = AfterEach(func() {
	ExpectCleanedUp(ctx, env.Client)
})

var _ = Describe("NodeClaimUtils", func() {
	var machine *v1alpha5.Machine
	var node *v1.Node
	BeforeEach(func() {
		node = test.Node(test.NodeOptions{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1.LabelTopologyZone:             "test-zone-1",
					v1.LabelTopologyRegion:           "test-region",
					"test-label-key":                 "test-label-value",
					"test-label-key2":                "test-label-value2",
					v1alpha5.LabelNodeRegistered:     "true",
					v1alpha5.LabelNodeInitialized:    "true",
					v1alpha5.ProvisionerNameLabelKey: "default",
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
		machine = test.Machine(v1alpha5.Machine{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1.LabelTopologyZone:             "test-zone-1",
					v1.LabelTopologyRegion:           "test-region",
					"test-label-key":                 "test-label-value",
					"test-label-key2":                "test-label-value2",
					v1alpha5.LabelNodeRegistered:     "true",
					v1alpha5.LabelNodeInitialized:    "true",
					v1alpha5.ProvisionerNameLabelKey: "default",
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
			Spec: v1alpha5.MachineSpec{
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
				Resources: v1alpha5.ResourceRequirements{
					Requests: v1.ResourceList{
						v1.ResourceCPU:              resource.MustParse("5"),
						v1.ResourceMemory:           resource.MustParse("5Mi"),
						v1.ResourceEphemeralStorage: resource.MustParse("100Gi"),
					},
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
				MachineTemplateRef: &v1alpha5.MachineTemplateRef{
					Kind:       "MachineTemplate",
					APIVersion: "test.cloudprovider/v1",
					Name:       "default",
				},
			},
			Status: v1alpha5.MachineStatus{
				NodeName:    node.Name,
				ProviderID:  node.Spec.ProviderID,
				Capacity:    node.Status.Capacity,
				Allocatable: node.Status.Allocatable,
			},
		})
		machine.StatusConditions().MarkTrue(v1alpha5.MachineLaunched)
		machine.StatusConditions().MarkTrue(v1alpha5.MachineRegistered)
		machine.StatusConditions().MarkTrue(v1alpha5.MachineInitialized)
	})
	It("should convert a Machine to a NodeClaim", func() {
		nodeClaim := nodeclaimutil.New(machine)

		for k, v := range machine.Annotations {
			Expect(nodeClaim.Annotations).To(HaveKeyWithValue(k, v))
		}
		for k, v := range machine.Labels {
			Expect(nodeClaim.Labels).To(HaveKeyWithValue(k, v))
		}
		Expect(nodeClaim.Spec.Taints).To(Equal(machine.Spec.Taints))
		Expect(nodeClaim.Spec.StartupTaints).To(Equal(machine.Spec.StartupTaints))
		Expect(nodeClaim.Spec.Requirements).To(ContainElements(machine.Spec.Requirements))
		Expect(nodeClaim.Spec.Resources.Requests).To(Equal(machine.Spec.Resources.Requests))

		Expect(nodeClaim.Spec.KubeletConfiguration.ClusterDNS).To(Equal(machine.Spec.Kubelet.ClusterDNS))
		Expect(nodeClaim.Spec.KubeletConfiguration.ContainerRuntime).To(Equal(machine.Spec.Kubelet.ContainerRuntime))
		Expect(nodeClaim.Spec.KubeletConfiguration.MaxPods).To(Equal(machine.Spec.Kubelet.MaxPods))
		Expect(nodeClaim.Spec.KubeletConfiguration.PodsPerCore).To(Equal(machine.Spec.Kubelet.PodsPerCore))
		Expect(nodeClaim.Spec.KubeletConfiguration.SystemReserved).To(Equal(machine.Spec.Kubelet.SystemReserved))
		Expect(nodeClaim.Spec.KubeletConfiguration.KubeReserved).To(Equal(machine.Spec.Kubelet.KubeReserved))
		Expect(nodeClaim.Spec.KubeletConfiguration.EvictionHard).To(Equal(machine.Spec.Kubelet.EvictionHard))
		Expect(nodeClaim.Spec.KubeletConfiguration.EvictionSoft).To(Equal(machine.Spec.Kubelet.EvictionSoft))
		Expect(nodeClaim.Spec.KubeletConfiguration.EvictionSoftGracePeriod).To(Equal(machine.Spec.Kubelet.EvictionSoftGracePeriod))
		Expect(nodeClaim.Spec.KubeletConfiguration.EvictionMaxPodGracePeriod).To(Equal(machine.Spec.Kubelet.EvictionMaxPodGracePeriod))
		Expect(nodeClaim.Spec.KubeletConfiguration.ImageGCHighThresholdPercent).To(Equal(machine.Spec.Kubelet.ImageGCHighThresholdPercent))
		Expect(nodeClaim.Spec.KubeletConfiguration.ImageGCLowThresholdPercent).To(Equal(machine.Spec.Kubelet.ImageGCLowThresholdPercent))
		Expect(nodeClaim.Spec.KubeletConfiguration.CPUCFSQuota).To(Equal(machine.Spec.Kubelet.CPUCFSQuota))

		Expect(nodeClaim.Spec.NodeClass.Kind).To(Equal(machine.Spec.MachineTemplateRef.Kind))
		Expect(nodeClaim.Spec.NodeClass.APIVersion).To(Equal(machine.Spec.MachineTemplateRef.APIVersion))
		Expect(nodeClaim.Spec.NodeClass.Name).To(Equal(machine.Spec.MachineTemplateRef.Name))

		Expect(nodeClaim.Status.NodeName).To(Equal(machine.Status.NodeName))
		Expect(nodeClaim.Status.ProviderID).To(Equal(machine.Status.ProviderID))
		Expect(nodeClaim.Status.Capacity).To(Equal(machine.Status.Capacity))
		Expect(nodeClaim.Status.Allocatable).To(Equal(machine.Status.Allocatable))

		Expect(nodeClaim.StatusConditions().GetCondition(v1beta1.NodeLaunched).IsTrue()).To(BeTrue())
		Expect(nodeClaim.StatusConditions().GetCondition(v1beta1.NodeRegistered).IsTrue()).To(BeTrue())
		Expect(nodeClaim.StatusConditions().GetCondition(v1beta1.NodeInitialized).IsTrue()).To(BeTrue())
	})
	It("should convert a Node to a NodeClaim", func() {
		nodeClaim := nodeclaimutil.NewFromNode(node)
		for k, v := range node.Annotations {
			Expect(nodeClaim.Annotations).To(HaveKeyWithValue(k, v))
		}
		for k, v := range node.Labels {
			Expect(nodeClaim.Labels).To(HaveKeyWithValue(k, v))
		}
		Expect(lo.Contains(nodeClaim.Finalizers, v1beta1.TerminationFinalizer)).To(BeTrue())
		Expect(nodeClaim.Spec.Taints).To(Equal(node.Spec.Taints))
		Expect(nodeClaim.Spec.Requirements).To(ContainElements(scheduling.NewLabelRequirements(node.Labels).NodeSelectorRequirements()))
		Expect(nodeClaim.Spec.Resources.Requests).To(Equal(node.Status.Allocatable))
		Expect(nodeClaim.Status.NodeName).To(Equal(node.Name))
		Expect(nodeClaim.Status.ProviderID).To(Equal(node.Spec.ProviderID))
		Expect(nodeClaim.Status.Capacity).To(Equal(node.Status.Capacity))
		Expect(nodeClaim.Status.Allocatable).To(Equal(node.Status.Allocatable))
		Expect(nodeClaim.StatusConditions().GetCondition(v1beta1.NodeLaunched).IsTrue()).To(BeTrue())
		Expect(nodeClaim.StatusConditions().GetCondition(v1beta1.NodeRegistered).IsTrue()).To(BeTrue())
		Expect(nodeClaim.StatusConditions().GetCondition(v1beta1.NodeInitialized).IsTrue()).To(BeTrue())
	})
	It("should retrieve a NodeClaim with a get call", func() {
		nodeClaim := test.NodeClaim(v1beta1.NodeClaim{
			Spec: v1beta1.NodeClaimSpec{
				NodeClass: &v1beta1.NodeClassReference{
					Kind:       "NodeClass",
					APIVersion: "test.cloudprovider/v1",
					Name:       "default",
				},
			},
		})
		ExpectApplied(ctx, env.Client, nodeClaim)

		retrieved, err := nodeclaimutil.Get(ctx, env.Client, nodeclaimutil.Key{Name: nodeClaim.Name, IsMachine: false})
		Expect(err).ToNot(HaveOccurred())
		Expect(retrieved.Name).To(Equal(nodeClaim.Name))
	})
	It("should retrieve a Machine with a get call", func() {
		machine := test.Machine()
		ExpectApplied(ctx, env.Client, machine)

		retrieved, err := nodeclaimutil.Get(ctx, env.Client, nodeclaimutil.Key{Name: machine.Name, IsMachine: true})
		Expect(err).ToNot(HaveOccurred())
		Expect(retrieved.Name).To(Equal(machine.Name))
	})
	It("should retrieve both NodeClaims and Machines on a list call", func() {
		Skip("Re-enable this test when NodeClaims are enabled and v1beta1 is released")

		numNodeClaims := 3
		numMachines := 5

		for i := 0; i < numNodeClaims; i++ {
			ExpectApplied(ctx, env.Client, test.NodeClaim(v1beta1.NodeClaim{
				Spec: v1beta1.NodeClaimSpec{
					NodeClass: &v1beta1.NodeClassReference{
						Kind:       "NodeClass",
						APIVersion: "test.cloudprovider/v1",
						Name:       "default",
					},
				},
			}))
		}
		for i := 0; i < numMachines; i++ {
			ExpectApplied(ctx, env.Client, test.Machine())
		}

		retrieved, err := nodeclaimutil.List(ctx, env.Client)
		Expect(err).ToNot(HaveOccurred())
		Expect(retrieved.Items).To(HaveLen(8))
	})
	It("should update the status on a NodeClaim", func() {
		nodeClaim := test.NodeClaim(v1beta1.NodeClaim{
			Spec: v1beta1.NodeClaimSpec{
				NodeClass: &v1beta1.NodeClassReference{
					Kind:       "NodeClass",
					APIVersion: "test.cloudprovider/v1",
					Name:       "default",
				},
			},
		})
		ExpectApplied(ctx, env.Client, nodeClaim)

		providerID := test.RandomProviderID()
		nodeClaim.Status.ProviderID = providerID
		Expect(nodeclaimutil.UpdateStatus(ctx, env.Client, nodeClaim)).To(Succeed())

		retrieved := &v1beta1.NodeClaim{}
		Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(nodeClaim), retrieved)).To(Succeed())
		Expect(retrieved.Status.ProviderID).To(Equal(providerID))
	})
	It("should update the status on a Machine", func() {
		machine := test.Machine()
		ExpectApplied(ctx, env.Client, machine)

		nodeClaim := nodeclaimutil.New(machine)
		providerID := test.RandomProviderID()
		nodeClaim.Status.ProviderID = providerID
		Expect(nodeclaimutil.UpdateStatus(ctx, env.Client, nodeClaim)).To(Succeed())

		retrieved := &v1alpha5.Machine{}
		Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(machine), retrieved)).To(Succeed())
		Expect(retrieved.Status.ProviderID).To(Equal(providerID))
	})
	It("should patch a NodeClaim", func() {
		nodeClaim := test.NodeClaim(v1beta1.NodeClaim{
			Spec: v1beta1.NodeClaimSpec{
				NodeClass: &v1beta1.NodeClassReference{
					Kind:       "NodeClass",
					APIVersion: "test.cloudprovider/v1",
					Name:       "default",
				},
			},
		})
		ExpectApplied(ctx, env.Client, nodeClaim)

		stored := nodeClaim.DeepCopy()
		nodeClaim.Labels = lo.Assign(nodeClaim.Labels, map[string]string{
			"custom-key": "custom-value",
		})
		Expect(nodeclaimutil.Patch(ctx, env.Client, stored, nodeClaim)).To(Succeed())

		retrieved := &v1beta1.NodeClaim{}
		Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(nodeClaim), retrieved)).To(Succeed())
		Expect(retrieved.Labels).To(HaveKeyWithValue("custom-key", "custom-value"))
	})
	It("should patch a Machine", func() {
		machine := test.Machine()
		ExpectApplied(ctx, env.Client, machine)

		nodeClaim := nodeclaimutil.New(machine)
		stored := nodeClaim.DeepCopy()
		nodeClaim.Labels = lo.Assign(nodeClaim.Labels, map[string]string{
			"custom-key": "custom-value",
		})
		Expect(nodeclaimutil.Patch(ctx, env.Client, stored, nodeClaim)).To(Succeed())

		retrieved := &v1alpha5.Machine{}
		Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(machine), retrieved)).To(Succeed())
		Expect(retrieved.Labels).To(HaveKeyWithValue("custom-key", "custom-value"))
	})
	It("should patch the status on a NodeClaim", func() {
		nodeClaim := test.NodeClaim(v1beta1.NodeClaim{
			Spec: v1beta1.NodeClaimSpec{
				NodeClass: &v1beta1.NodeClassReference{
					Kind:       "NodeClass",
					APIVersion: "test.cloudprovider/v1",
					Name:       "default",
				},
			},
		})
		ExpectApplied(ctx, env.Client, nodeClaim)

		stored := nodeClaim.DeepCopy()
		providerID := test.RandomProviderID()
		nodeClaim.Status.ProviderID = providerID
		Expect(nodeclaimutil.PatchStatus(ctx, env.Client, stored, nodeClaim)).To(Succeed())

		retrieved := &v1beta1.NodeClaim{}
		Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(nodeClaim), retrieved)).To(Succeed())
		Expect(retrieved.Status.ProviderID).To(Equal(providerID))
	})
	It("should patch the status on a Machine", func() {
		machine := test.Machine()
		ExpectApplied(ctx, env.Client, machine)

		nodeClaim := nodeclaimutil.New(machine)
		stored := nodeClaim.DeepCopy()
		providerID := test.RandomProviderID()
		nodeClaim.Status.ProviderID = providerID
		Expect(nodeclaimutil.PatchStatus(ctx, env.Client, stored, nodeClaim)).To(Succeed())

		retrieved := &v1alpha5.Machine{}
		Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(machine), retrieved)).To(Succeed())
		Expect(retrieved.Status.ProviderID).To(Equal(providerID))
	})
	It("should delete a NodeClaim with a delete call", func() {
		nodeClaim := test.NodeClaim(v1beta1.NodeClaim{
			Spec: v1beta1.NodeClaimSpec{
				NodeClass: &v1beta1.NodeClassReference{
					Kind:       "NodeClass",
					APIVersion: "test.cloudprovider/v1",
					Name:       "default",
				},
			},
		})
		ExpectApplied(ctx, env.Client, nodeClaim)

		err := nodeclaimutil.Delete(ctx, env.Client, nodeClaim)
		Expect(err).ToNot(HaveOccurred())

		nodeClaimList := &v1beta1.NodeClaimList{}
		Expect(env.Client.List(ctx, nodeClaimList)).To(Succeed())
		Expect(nodeClaimList.Items).To(HaveLen(0))
		Expect(errors.IsNotFound(env.Client.Get(ctx, client.ObjectKeyFromObject(nodeClaim), &v1beta1.NodeClaim{}))).To(BeTrue())
	})
	It("should delete a Machine with a delete call", func() {
		machine := test.Machine()
		ExpectApplied(ctx, env.Client, machine)

		nodeClaim := nodeclaimutil.New(machine)

		err := nodeclaimutil.Delete(ctx, env.Client, nodeClaim)
		Expect(err).ToNot(HaveOccurred())

		machineList := &v1alpha5.MachineList{}
		Expect(env.Client.List(ctx, machineList)).To(Succeed())
		Expect(machineList.Items).To(HaveLen(0))
		Expect(errors.IsNotFound(env.Client.Get(ctx, client.ObjectKeyFromObject(machine), &v1alpha5.Machine{}))).To(BeTrue())
	})
	It("should update the owner for a Node to a NodeClaim", func() {
		nodeClaim := test.NodeClaim(v1beta1.NodeClaim{
			Spec: v1beta1.NodeClaimSpec{
				NodeClass: &v1beta1.NodeClassReference{
					Kind:       "NodeClass",
					APIVersion: "test.cloudprovider/v1",
					Name:       "default",
				},
			},
		})
		node = test.Node(test.NodeOptions{ProviderID: nodeClaim.Status.ProviderID})
		node = nodeclaimutil.UpdateNodeOwnerReferences(nodeClaim, node)

		Expect(lo.Contains(node.OwnerReferences, metav1.OwnerReference{
			APIVersion:         lo.Must(apiutil.GVKForObject(nodeClaim, scheme.Scheme)).GroupVersion().String(),
			Kind:               lo.Must(apiutil.GVKForObject(nodeClaim, scheme.Scheme)).String(),
			Name:               nodeClaim.Name,
			UID:                nodeClaim.UID,
			BlockOwnerDeletion: lo.ToPtr(true),
		}))
	})
	It("should update the owner for a Node to a Machine", func() {
		machine := test.Machine()
		node = test.Node(test.NodeOptions{ProviderID: machine.Status.ProviderID})
		nodeClaim := nodeclaimutil.New(machine)
		node = nodeclaimutil.UpdateNodeOwnerReferences(nodeClaim, node)

		Expect(lo.Contains(node.OwnerReferences, metav1.OwnerReference{
			APIVersion:         lo.Must(apiutil.GVKForObject(machine, scheme.Scheme)).GroupVersion().String(),
			Kind:               lo.Must(apiutil.GVKForObject(machine, scheme.Scheme)).String(),
			Name:               machine.Name,
			UID:                machine.UID,
			BlockOwnerDeletion: lo.ToPtr(true),
		}))
	})
	It("should retrieve the owner for a NodeClaim", func() {
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
		nodeClaim := test.NodeClaim(v1beta1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1beta1.NodePoolLabelKey: nodePool.Name,
				},
			},
			Spec: v1beta1.NodeClaimSpec{
				NodeClass: &v1beta1.NodeClassReference{
					Kind:       "NodeClass",
					APIVersion: "test.cloudprovider/v1",
					Name:       "default",
				},
			},
		})
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim)

		name := nodeclaimutil.OwnerName(nodeClaim)
		Expect(name).To(Equal(nodePool.Name))

		owner, err := nodeclaimutil.Owner(ctx, env.Client, nodeClaim)
		Expect(err).ToNot(HaveOccurred())
		Expect(owner.Name).To(Equal(nodePool.Name))
	})
	It("should retrieve the owner for a Machine", func() {
		provisioner := test.Provisioner()
		machine := test.Machine(v1alpha5.Machine{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1alpha5.ProvisionerNameLabelKey: provisioner.Name,
				},
			},
		})
		ExpectApplied(ctx, env.Client, provisioner, machine)

		name := nodeclaimutil.OwnerName(machine)
		Expect(name).To(Equal(provisioner.Name))

		owner, err := nodeclaimutil.Owner(ctx, env.Client, machine)
		Expect(err).ToNot(HaveOccurred())
		Expect(owner.Name).To(Equal(provisioner.Name))
	})
})
