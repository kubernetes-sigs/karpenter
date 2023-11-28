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

package scheduling_test

import (
	"fmt"
	"math/rand"

	"github.com/mitchellh/hashstructure/v2"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/sets"

	"sigs.k8s.io/karpenter/pkg/apis/v1beta1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
	"sigs.k8s.io/karpenter/pkg/utils/resources"
)

var _ = Describe("Instance Type Selection", func() {
	var nodePool *v1beta1.NodePool
	var minPrice float64
	var instanceTypeMap map[string]*cloudprovider.InstanceType
	nodePrice := func(n *v1.Node) float64 {
		of, _ := instanceTypeMap[n.Labels[v1.LabelInstanceTypeStable]].Offerings.Get(n.Labels[v1beta1.CapacityTypeLabelKey], n.Labels[v1.LabelTopologyZone])
		return of.Price
	}

	BeforeEach(func() {
		nodePool = test.NodePool(v1beta1.NodePool{
			Spec: v1beta1.NodePoolSpec{
				Template: v1beta1.NodeClaimTemplate{
					Spec: v1beta1.NodeClaimSpec{
						Requirements: []v1.NodeSelectorRequirement{
							{
								Key:      v1beta1.CapacityTypeLabelKey,
								Operator: v1.NodeSelectorOpIn,
								Values:   []string{v1beta1.CapacityTypeSpot, v1beta1.CapacityTypeOnDemand},
							},
							{
								Key:      v1.LabelArchStable,
								Operator: v1.NodeSelectorOpIn,
								Values:   []string{v1beta1.ArchitectureArm64, v1beta1.ArchitectureAmd64},
							},
						},
					},
				},
			},
		})
		cloudProvider.InstanceTypes = fake.InstanceTypesAssorted()
		instanceTypeMap = getInstanceTypeMap(cloudProvider.InstanceTypes)
		minPrice = getMinPrice(cloudProvider.InstanceTypes)

		// add some randomness to instance type ordering to ensure we sort everywhere we need to
		rand.Shuffle(len(cloudProvider.InstanceTypes), func(i, j int) {
			cloudProvider.InstanceTypes[i], cloudProvider.InstanceTypes[j] = cloudProvider.InstanceTypes[j], cloudProvider.InstanceTypes[i]
		})
	})

	// This set of tests ensure that we schedule on the cheapest valid instance type while also ensuring that all of the
	// instance types passed to the cloud provider are also valid per nodePool and node selector requirements.  In some
	// ways they repeat some other tests, but the testing regarding checking against all possible instance types
	// passed to the cloud provider is unique.
	It("should schedule on one of the cheapest instances", func() {
		ExpectApplied(ctx, env.Client, nodePool)
		pod := test.UnschedulablePod()
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
		node := ExpectScheduled(ctx, env.Client, pod)
		Expect(nodePrice(node)).To(Equal(minPrice))
	})
	It("should schedule on one of the cheapest instances (pod arch = amd64)", func() {
		ExpectApplied(ctx, env.Client, nodePool)
		pod := test.UnschedulablePod(
			test.PodOptions{NodeRequirements: []v1.NodeSelectorRequirement{{
				Key:      v1.LabelArchStable,
				Operator: v1.NodeSelectorOpIn,
				Values:   []string{v1beta1.ArchitectureAmd64},
			}}})
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
		node := ExpectScheduled(ctx, env.Client, pod)
		Expect(nodePrice(node)).To(Equal(minPrice))
		// ensure that the entire list of instance types match the label
		ExpectInstancesWithLabel(supportedInstanceTypes(cloudProvider.CreateCalls[0]), v1.LabelArchStable, v1beta1.ArchitectureAmd64)
	})
	It("should schedule on one of the cheapest instances (pod arch = arm64)", func() {
		ExpectApplied(ctx, env.Client, nodePool)
		pod := test.UnschedulablePod(
			test.PodOptions{NodeRequirements: []v1.NodeSelectorRequirement{{
				Key:      v1.LabelArchStable,
				Operator: v1.NodeSelectorOpIn,
				Values:   []string{v1beta1.ArchitectureArm64},
			}}})
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
		node := ExpectScheduled(ctx, env.Client, pod)
		Expect(nodePrice(node)).To(Equal(minPrice))
		ExpectInstancesWithLabel(supportedInstanceTypes(cloudProvider.CreateCalls[0]), v1.LabelArchStable, v1beta1.ArchitectureArm64)
	})
	It("should schedule on one of the cheapest instances (prov arch = amd64)", func() {
		nodePool.Spec.Template.Spec.Requirements = []v1.NodeSelectorRequirement{
			{
				Key:      v1.LabelArchStable,
				Operator: v1.NodeSelectorOpIn,
				Values:   []string{v1beta1.ArchitectureAmd64},
			},
		}
		ExpectApplied(ctx, env.Client, nodePool)
		pod := test.UnschedulablePod()
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
		node := ExpectScheduled(ctx, env.Client, pod)
		Expect(nodePrice(node)).To(Equal(minPrice))
		ExpectInstancesWithLabel(supportedInstanceTypes(cloudProvider.CreateCalls[0]), v1.LabelArchStable, v1beta1.ArchitectureAmd64)
	})
	It("should schedule on one of the cheapest instances (prov arch = arm64)", func() {
		nodePool.Spec.Template.Spec.Requirements = []v1.NodeSelectorRequirement{
			{
				Key:      v1.LabelArchStable,
				Operator: v1.NodeSelectorOpIn,
				Values:   []string{v1beta1.ArchitectureArm64},
			},
		}
		ExpectApplied(ctx, env.Client, nodePool)
		pod := test.UnschedulablePod()
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
		node := ExpectScheduled(ctx, env.Client, pod)
		Expect(nodePrice(node)).To(Equal(minPrice))
		ExpectInstancesWithLabel(supportedInstanceTypes(cloudProvider.CreateCalls[0]), v1.LabelArchStable, v1beta1.ArchitectureArm64)
	})
	It("should schedule on one of the cheapest instances (prov os = windows)", func() {
		nodePool.Spec.Template.Spec.Requirements = []v1.NodeSelectorRequirement{
			{
				Key:      v1.LabelOSStable,
				Operator: v1.NodeSelectorOpIn,
				Values:   []string{string(v1.Windows)},
			},
		}
		ExpectApplied(ctx, env.Client, nodePool)
		pod := test.UnschedulablePod()
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
		node := ExpectScheduled(ctx, env.Client, pod)
		Expect(nodePrice(node)).To(Equal(minPrice))
		ExpectInstancesWithLabel(supportedInstanceTypes(cloudProvider.CreateCalls[0]), v1.LabelOSStable, string(v1.Windows))
	})
	It("should schedule on one of the cheapest instances (pod os = windows)", func() {
		ExpectApplied(ctx, env.Client, nodePool)
		pod := test.UnschedulablePod(
			test.PodOptions{NodeRequirements: []v1.NodeSelectorRequirement{{
				Key:      v1.LabelOSStable,
				Operator: v1.NodeSelectorOpIn,
				Values:   []string{string(v1.Windows)},
			}}})
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
		node := ExpectScheduled(ctx, env.Client, pod)
		Expect(nodePrice(node)).To(Equal(minPrice))
		ExpectInstancesWithLabel(supportedInstanceTypes(cloudProvider.CreateCalls[0]), v1.LabelOSStable, string(v1.Windows))
	})
	It("should schedule on one of the cheapest instances (prov os = windows)", func() {
		nodePool.Spec.Template.Spec.Requirements = []v1.NodeSelectorRequirement{
			{
				Key:      v1.LabelOSStable,
				Operator: v1.NodeSelectorOpIn,
				Values:   []string{string(v1.Windows)},
			},
		}
		ExpectApplied(ctx, env.Client, nodePool)
		pod := test.UnschedulablePod()
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
		node := ExpectScheduled(ctx, env.Client, pod)
		Expect(nodePrice(node)).To(Equal(minPrice))
		ExpectInstancesWithLabel(supportedInstanceTypes(cloudProvider.CreateCalls[0]), v1.LabelOSStable, string(v1.Windows))
	})
	It("should schedule on one of the cheapest instances (pod os = linux)", func() {
		ExpectApplied(ctx, env.Client, nodePool)
		pod := test.UnschedulablePod(
			test.PodOptions{NodeRequirements: []v1.NodeSelectorRequirement{{
				Key:      v1.LabelOSStable,
				Operator: v1.NodeSelectorOpIn,
				Values:   []string{string(v1.Linux)},
			}}})
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
		node := ExpectScheduled(ctx, env.Client, pod)
		Expect(nodePrice(node)).To(Equal(minPrice))
		ExpectInstancesWithLabel(supportedInstanceTypes(cloudProvider.CreateCalls[0]), v1.LabelOSStable, string(v1.Linux))
	})
	It("should schedule on one of the cheapest instances (pod os = linux)", func() {
		ExpectApplied(ctx, env.Client, nodePool)
		pod := test.UnschedulablePod(
			test.PodOptions{NodeRequirements: []v1.NodeSelectorRequirement{{
				Key:      v1.LabelOSStable,
				Operator: v1.NodeSelectorOpIn,
				Values:   []string{string(v1.Linux)},
			}}})
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
		node := ExpectScheduled(ctx, env.Client, pod)
		Expect(nodePrice(node)).To(Equal(minPrice))
		ExpectInstancesWithLabel(supportedInstanceTypes(cloudProvider.CreateCalls[0]), v1.LabelOSStable, string(v1.Linux))
	})
	It("should schedule on one of the cheapest instances (prov zone = test-zone-2)", func() {
		nodePool.Spec.Template.Spec.Requirements = []v1.NodeSelectorRequirement{
			{
				Key:      v1.LabelTopologyZone,
				Operator: v1.NodeSelectorOpIn,
				Values:   []string{"test-zone-2"},
			},
		}
		ExpectApplied(ctx, env.Client, nodePool)
		pod := test.UnschedulablePod()
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
		node := ExpectScheduled(ctx, env.Client, pod)
		Expect(nodePrice(node)).To(Equal(minPrice))
		ExpectInstancesWithLabel(supportedInstanceTypes(cloudProvider.CreateCalls[0]), v1.LabelTopologyZone, "test-zone-2")
	})
	It("should schedule on one of the cheapest instances (pod zone = test-zone-2)", func() {
		ExpectApplied(ctx, env.Client, nodePool)
		pod := test.UnschedulablePod(
			test.PodOptions{NodeRequirements: []v1.NodeSelectorRequirement{{
				Key:      v1.LabelTopologyZone,
				Operator: v1.NodeSelectorOpIn,
				Values:   []string{"test-zone-2"},
			}}})
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
		node := ExpectScheduled(ctx, env.Client, pod)
		Expect(nodePrice(node)).To(Equal(minPrice))
		ExpectInstancesWithLabel(supportedInstanceTypes(cloudProvider.CreateCalls[0]), v1.LabelTopologyZone, "test-zone-2")
	})
	It("should schedule on one of the cheapest instances (prov ct = spot)", func() {
		nodePool.Spec.Template.Spec.Requirements = []v1.NodeSelectorRequirement{
			{
				Key:      v1beta1.CapacityTypeLabelKey,
				Operator: v1.NodeSelectorOpIn,
				Values:   []string{v1beta1.CapacityTypeSpot},
			},
		}
		ExpectApplied(ctx, env.Client, nodePool)
		pod := test.UnschedulablePod()
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
		node := ExpectScheduled(ctx, env.Client, pod)
		Expect(nodePrice(node)).To(Equal(minPrice))
		ExpectInstancesWithLabel(supportedInstanceTypes(cloudProvider.CreateCalls[0]), v1beta1.CapacityTypeLabelKey, v1beta1.CapacityTypeSpot)
	})
	It("should schedule on one of the cheapest instances (pod ct = spot)", func() {
		ExpectApplied(ctx, env.Client, nodePool)
		pod := test.UnschedulablePod(
			test.PodOptions{NodeRequirements: []v1.NodeSelectorRequirement{{
				Key:      v1beta1.CapacityTypeLabelKey,
				Operator: v1.NodeSelectorOpIn,
				Values:   []string{v1beta1.CapacityTypeSpot},
			}}})
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
		node := ExpectScheduled(ctx, env.Client, pod)
		Expect(nodePrice(node)).To(Equal(minPrice))
		ExpectInstancesWithLabel(supportedInstanceTypes(cloudProvider.CreateCalls[0]), v1beta1.CapacityTypeLabelKey, v1beta1.CapacityTypeSpot)
	})
	It("should schedule on one of the cheapest instances (prov ct = ondemand, prov zone = test-zone-1)", func() {
		nodePool.Spec.Template.Spec.Requirements = []v1.NodeSelectorRequirement{
			{
				Key:      v1beta1.CapacityTypeLabelKey,
				Operator: v1.NodeSelectorOpIn,
				Values:   []string{v1beta1.CapacityTypeOnDemand},
			},
			{
				Key:      v1.LabelTopologyZone,
				Operator: v1.NodeSelectorOpIn,
				Values:   []string{"test-zone-1"},
			},
		}
		ExpectApplied(ctx, env.Client, nodePool)
		pod := test.UnschedulablePod()
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
		node := ExpectScheduled(ctx, env.Client, pod)
		Expect(nodePrice(node)).To(Equal(minPrice))
		ExpectInstancesWithOffering(supportedInstanceTypes(cloudProvider.CreateCalls[0]), v1beta1.CapacityTypeOnDemand, "test-zone-1")
	})
	It("should schedule on one of the cheapest instances (pod ct = spot, pod zone = test-zone-1)", func() {
		ExpectApplied(ctx, env.Client, nodePool)
		pod := test.UnschedulablePod(
			test.PodOptions{NodeRequirements: []v1.NodeSelectorRequirement{{
				Key:      v1beta1.CapacityTypeLabelKey,
				Operator: v1.NodeSelectorOpIn,
				Values:   []string{v1beta1.CapacityTypeSpot},
			},
				{
					Key:      v1.LabelTopologyZone,
					Operator: v1.NodeSelectorOpIn,
					Values:   []string{"test-zone-1"},
				},
			}})
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
		node := ExpectScheduled(ctx, env.Client, pod)
		Expect(nodePrice(node)).To(Equal(minPrice))
		ExpectInstancesWithOffering(supportedInstanceTypes(cloudProvider.CreateCalls[0]), v1beta1.CapacityTypeSpot, "test-zone-1")
	})
	It("should schedule on one of the cheapest instances (prov ct = spot, pod zone = test-zone-2)", func() {
		nodePool.Spec.Template.Spec.Requirements = []v1.NodeSelectorRequirement{
			{
				Key:      v1beta1.CapacityTypeLabelKey,
				Operator: v1.NodeSelectorOpIn,
				Values:   []string{v1beta1.CapacityTypeSpot},
			},
		}
		ExpectApplied(ctx, env.Client, nodePool)
		pod := test.UnschedulablePod(
			test.PodOptions{NodeRequirements: []v1.NodeSelectorRequirement{{
				Key:      v1.LabelTopologyZone,
				Operator: v1.NodeSelectorOpIn,
				Values:   []string{"test-zone-2"},
			}}})
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
		node := ExpectScheduled(ctx, env.Client, pod)
		Expect(nodePrice(node)).To(Equal(minPrice))
		ExpectInstancesWithOffering(supportedInstanceTypes(cloudProvider.CreateCalls[0]), v1beta1.CapacityTypeSpot, "test-zone-2")
	})
	It("should schedule on one of the cheapest instances (prov ct = ondemand/test-zone-1/arm64/windows)", func() {
		nodePool.Spec.Template.Spec.Requirements = []v1.NodeSelectorRequirement{
			{
				Key:      v1.LabelArchStable,
				Operator: v1.NodeSelectorOpIn,
				Values:   []string{v1beta1.ArchitectureArm64},
			},
			{
				Key:      v1.LabelOSStable,
				Operator: v1.NodeSelectorOpIn,
				Values:   []string{string(v1.Windows)},
			},
			{
				Key:      v1beta1.CapacityTypeLabelKey,
				Operator: v1.NodeSelectorOpIn,
				Values:   []string{v1beta1.CapacityTypeOnDemand},
			},
			{
				Key:      v1.LabelTopologyZone,
				Operator: v1.NodeSelectorOpIn,
				Values:   []string{"test-zone-1"},
			},
		}
		ExpectApplied(ctx, env.Client, nodePool)
		pod := test.UnschedulablePod()
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
		node := ExpectScheduled(ctx, env.Client, pod)
		Expect(nodePrice(node)).To(Equal(minPrice))
		ExpectInstancesWithOffering(supportedInstanceTypes(cloudProvider.CreateCalls[0]), v1beta1.CapacityTypeOnDemand, "test-zone-1")
		ExpectInstancesWithLabel(supportedInstanceTypes(cloudProvider.CreateCalls[0]), v1.LabelOSStable, string(v1.Windows))
		ExpectInstancesWithLabel(supportedInstanceTypes(cloudProvider.CreateCalls[0]), v1.LabelArchStable, "arm64")
	})
	It("should schedule on one of the cheapest instances (prov = spot/test-zone-2, pod = amd64/linux)", func() {
		nodePool.Spec.Template.Spec.Requirements = []v1.NodeSelectorRequirement{
			{
				Key:      v1.LabelArchStable,
				Operator: v1.NodeSelectorOpIn,
				Values:   []string{v1beta1.ArchitectureAmd64},
			},
			{
				Key:      v1.LabelOSStable,
				Operator: v1.NodeSelectorOpIn,
				Values:   []string{string(v1.Linux)},
			},
		}
		ExpectApplied(ctx, env.Client, nodePool)
		pod := test.UnschedulablePod(
			test.PodOptions{NodeRequirements: []v1.NodeSelectorRequirement{
				{
					Key:      v1beta1.CapacityTypeLabelKey,
					Operator: v1.NodeSelectorOpIn,
					Values:   []string{v1beta1.CapacityTypeSpot},
				},
				{
					Key:      v1.LabelTopologyZone,
					Operator: v1.NodeSelectorOpIn,
					Values:   []string{"test-zone-2"},
				},
			}})
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
		node := ExpectScheduled(ctx, env.Client, pod)
		Expect(nodePrice(node)).To(Equal(minPrice))
		ExpectInstancesWithOffering(supportedInstanceTypes(cloudProvider.CreateCalls[0]), v1beta1.CapacityTypeSpot, "test-zone-2")
		ExpectInstancesWithLabel(supportedInstanceTypes(cloudProvider.CreateCalls[0]), v1.LabelOSStable, string(v1.Linux))
		ExpectInstancesWithLabel(supportedInstanceTypes(cloudProvider.CreateCalls[0]), v1.LabelArchStable, "amd64")
	})
	It("should schedule on one of the cheapest instances (pod ct = spot/test-zone-2/amd64/linux)", func() {
		ExpectApplied(ctx, env.Client, nodePool)
		pod := test.UnschedulablePod(
			test.PodOptions{NodeRequirements: []v1.NodeSelectorRequirement{
				{
					Key:      v1.LabelArchStable,
					Operator: v1.NodeSelectorOpIn,
					Values:   []string{v1beta1.ArchitectureAmd64},
				},
				{
					Key:      v1.LabelOSStable,
					Operator: v1.NodeSelectorOpIn,
					Values:   []string{string(v1.Linux)},
				},
				{
					Key:      v1beta1.CapacityTypeLabelKey,
					Operator: v1.NodeSelectorOpIn,
					Values:   []string{v1beta1.CapacityTypeSpot},
				},
				{
					Key:      v1.LabelTopologyZone,
					Operator: v1.NodeSelectorOpIn,
					Values:   []string{"test-zone-2"},
				},
			}})
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
		node := ExpectScheduled(ctx, env.Client, pod)
		Expect(nodePrice(node)).To(Equal(minPrice))
		ExpectInstancesWithOffering(supportedInstanceTypes(cloudProvider.CreateCalls[0]), v1beta1.CapacityTypeSpot, "test-zone-2")
		ExpectInstancesWithLabel(supportedInstanceTypes(cloudProvider.CreateCalls[0]), v1.LabelOSStable, string(v1.Linux))
		ExpectInstancesWithLabel(supportedInstanceTypes(cloudProvider.CreateCalls[0]), v1.LabelArchStable, "amd64")
	})
	It("should not schedule if no instance type matches selector (pod arch = arm)", func() {
		// remove all Arm instance types
		cloudProvider.InstanceTypes = filterInstanceTypes(cloudProvider.InstanceTypes, func(i *cloudprovider.InstanceType) bool {
			return i.Requirements.Get(v1.LabelArchStable).Has(v1beta1.ArchitectureAmd64)
		})

		Expect(len(cloudProvider.InstanceTypes)).To(BeNumerically(">", 0))
		ExpectApplied(ctx, env.Client, nodePool)
		pod := test.UnschedulablePod(
			test.PodOptions{NodeRequirements: []v1.NodeSelectorRequirement{
				{
					Key:      v1.LabelArchStable,
					Operator: v1.NodeSelectorOpIn,
					Values:   []string{v1beta1.ArchitectureArm64},
				},
			}})
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
		ExpectNotScheduled(ctx, env.Client, pod)
		Expect(cloudProvider.CreateCalls).To(HaveLen(0))
	})
	It("should not schedule if no instance type matches selector (pod arch = arm zone=test-zone-2)", func() {
		// remove all Arm instance types in zone-2
		cloudProvider.InstanceTypes = filterInstanceTypes(cloudProvider.InstanceTypes, func(i *cloudprovider.InstanceType) bool {
			for _, off := range i.Offerings {
				if off.Zone == "test-zone-2" {
					return i.Requirements.Get(v1.LabelArchStable).Has(v1beta1.ArchitectureAmd64)
				}
			}
			return true
		})
		Expect(len(cloudProvider.InstanceTypes)).To(BeNumerically(">", 0))
		ExpectApplied(ctx, env.Client, nodePool)
		pod := test.UnschedulablePod(
			test.PodOptions{NodeRequirements: []v1.NodeSelectorRequirement{
				{
					Key:      v1.LabelArchStable,
					Operator: v1.NodeSelectorOpIn,
					Values:   []string{v1beta1.ArchitectureArm64},
				},
				{
					Key:      v1.LabelTopologyZone,
					Operator: v1.NodeSelectorOpIn,
					Values:   []string{"test-zone-2"},
				},
			}})
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
		ExpectNotScheduled(ctx, env.Client, pod)
		Expect(cloudProvider.CreateCalls).To(HaveLen(0))
	})
	It("should not schedule if no instance type matches selector (prov arch = arm / pod zone=test-zone-2)", func() {
		// remove all Arm instance types in zone-2
		cloudProvider.InstanceTypes = filterInstanceTypes(cloudProvider.InstanceTypes, func(i *cloudprovider.InstanceType) bool {
			for _, off := range i.Offerings {
				if off.Zone == "test-zone-2" {
					return i.Requirements.Get(v1.LabelArchStable).Has(v1beta1.ArchitectureAmd64)
				}
			}
			return true
		})

		nodePool.Spec.Template.Spec.Requirements = []v1.NodeSelectorRequirement{
			{
				Key:      v1.LabelArchStable,
				Operator: v1.NodeSelectorOpIn,
				Values:   []string{v1beta1.ArchitectureArm64},
			},
		}
		Expect(len(cloudProvider.InstanceTypes)).To(BeNumerically(">", 0))
		ExpectApplied(ctx, env.Client, nodePool)
		pod := test.UnschedulablePod(
			test.PodOptions{NodeRequirements: []v1.NodeSelectorRequirement{
				{
					Key:      v1.LabelTopologyZone,
					Operator: v1.NodeSelectorOpIn,
					Values:   []string{"test-zone-2"},
				},
			}})
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
		ExpectNotScheduled(ctx, env.Client, pod)
		Expect(cloudProvider.CreateCalls).To(HaveLen(0))
	})
	It("should schedule on an instance with enough resources", func() {
		// this is a pretty thorough exercise of scheduling, so we also check an invariant that scheduling doesn't
		// modify the instance type's Overhead() or Resources() maps so they can return the same map every time instead
		// of re-alllocating a new one per call
		resourceHashes := map[string]uint64{}
		overheadHashes := map[string]uint64{}
		for _, it := range cloudProvider.InstanceTypes {
			var err error
			resourceHashes[it.Name], err = hashstructure.Hash(it.Capacity, hashstructure.FormatV2, nil)
			Expect(err).To(BeNil())
			overheadHashes[it.Name], err = hashstructure.Hash(it.Overhead.Total(), hashstructure.FormatV2, nil)
			Expect(err).To(BeNil())
		}
		ExpectApplied(ctx, env.Client, nodePool)
		// these values are constructed so that three of these pods can always fit on at least one of our instance types
		for _, cpu := range []float64{0.1, 1.0, 2, 2.5, 4, 8, 16} {
			for _, mem := range []float64{0.1, 1.0, 2, 4, 8, 16, 32} {
				cluster.Reset()
				cloudProvider.CreateCalls = nil
				opts := test.PodOptions{
					ResourceRequirements: v1.ResourceRequirements{Requests: map[v1.ResourceName]resource.Quantity{
						v1.ResourceCPU:    resource.MustParse(fmt.Sprintf("%0.1f", cpu)),
						v1.ResourceMemory: resource.MustParse(fmt.Sprintf("%0.1fGi", mem)),
					}}}
				pods := []*v1.Pod{
					test.UnschedulablePod(opts), test.UnschedulablePod(opts), test.UnschedulablePod(opts),
				}
				ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pods...)
				nodeNames := sets.NewString()
				for _, p := range pods {
					node := ExpectScheduled(ctx, env.Client, p)
					nodeNames.Insert(node.Name)
				}
				// should fit on one node
				Expect(nodeNames).To(HaveLen(1))
				totalPodResources := resources.RequestsForPods(pods...)
				for _, it := range supportedInstanceTypes(cloudProvider.CreateCalls[0]) {
					totalReserved := resources.Merge(totalPodResources, it.Overhead.Total())
					// the total pod resources in CPU and memory + instance overhead should always be less than the
					// resources available on every viable instance has
					Expect(totalReserved.Cpu().Cmp(it.Capacity[v1.ResourceCPU])).To(Equal(-1))
					Expect(totalReserved.Memory().Cmp(it.Capacity[v1.ResourceMemory])).To(Equal(-1))
				}
			}
		}
		for _, it := range cloudProvider.InstanceTypes {
			resourceHash, err := hashstructure.Hash(it.Capacity, hashstructure.FormatV2, nil)
			Expect(err).To(BeNil())
			overheadHash, err := hashstructure.Hash(it.Overhead.Total(), hashstructure.FormatV2, nil)
			Expect(err).To(BeNil())
			Expect(resourceHash).To(Equal(resourceHashes[it.Name]), fmt.Sprintf("expected %s Resources() to not be modified by scheduling", it.Name))
			Expect(overheadHash).To(Equal(overheadHashes[it.Name]), fmt.Sprintf("expected %s Overhead() to not be modified by scheduling", it.Name))
		}
	})
	It("should schedule on cheaper on-demand instance even when spot price ordering would place other instance types first", func() {
		cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{
			fake.NewInstanceType(fake.InstanceTypeOptions{
				Name:             "test-instance1",
				Architecture:     "amd64",
				OperatingSystems: sets.New(string(v1.Linux)),
				Resources: v1.ResourceList{
					v1.ResourceCPU:    resource.MustParse("1"),
					v1.ResourceMemory: resource.MustParse("1Gi"),
				},
				Offerings: []cloudprovider.Offering{
					{CapacityType: v1beta1.CapacityTypeOnDemand, Zone: "test-zone-1a", Price: 1.0, Available: true},
					{CapacityType: v1beta1.CapacityTypeSpot, Zone: "test-zone-1a", Price: 0.2, Available: true},
				},
			}),
			fake.NewInstanceType(fake.InstanceTypeOptions{
				Name:             "test-instance2",
				Architecture:     "amd64",
				OperatingSystems: sets.New(string(v1.Linux)),
				Resources: v1.ResourceList{
					v1.ResourceCPU:    resource.MustParse("1"),
					v1.ResourceMemory: resource.MustParse("1Gi"),
				},
				Offerings: []cloudprovider.Offering{
					{CapacityType: v1beta1.CapacityTypeOnDemand, Zone: "test-zone-1a", Price: 1.3, Available: true},
					{CapacityType: v1beta1.CapacityTypeSpot, Zone: "test-zone-1a", Price: 0.1, Available: true},
				},
			}),
		}
		nodePool.Spec.Template.Spec.Requirements = []v1.NodeSelectorRequirement{
			{
				Key:      v1beta1.CapacityTypeLabelKey,
				Operator: v1.NodeSelectorOpIn,
				Values:   []string{"on-demand"},
			},
		}

		ExpectApplied(ctx, env.Client, nodePool)
		pod := test.UnschedulablePod()
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
		node := ExpectScheduled(ctx, env.Client, pod)
		Expect(node.Labels[v1.LabelInstanceTypeStable]).To(Equal("test-instance1"))
	})
})
