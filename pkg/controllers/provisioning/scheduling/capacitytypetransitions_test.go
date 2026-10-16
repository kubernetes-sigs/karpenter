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

package scheduling_test

import (
	"sync"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
	"sigs.k8s.io/karpenter/pkg/controllers/provisioning/scheduling"
	pscheduling "sigs.k8s.io/karpenter/pkg/scheduling"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
)

var _ = Describe("Capacity-type transitions", func() {
	var nodePool *v1.NodePool
	BeforeEach(func() {
		nodePool = test.NodePool()
		nodePool.Spec.Template.Spec.Requirements = []v1.NodeSelectorRequirementWithMinValues{{Key: v1.CapacityTypeLabelKey, Operator: corev1.NodeSelectorOpIn, Values: []string{v1.CapacityTypeOnDemand, v1.CapacityTypeReserved, v1.CapacityTypeSpot}}}
		cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{fake.NewInstanceType("group-targeted",
			fake.WithOfferings(cloudprovider.Offering{
				Requirements: pscheduling.NewLabelRequirements(map[string]string{v1.CapacityTypeLabelKey: v1.CapacityTypeOnDemand, corev1.LabelTopologyZone: "test-zone-1"}),
				Available:    true, Price: 1.0, CapacityTypeTransitions: []string{v1.CapacityTypeReserved},
			}),
		)}
	})
	DescribeTable("preserves allowed coverage states on claims", func(options test.PodOptions) {
		ExpectApplied(ctx, env.Client, nodePool)
		pod := test.UnschedulablePod(options)
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
		ExpectScheduled(ctx, env.Client, pod)
		Expect(cloudProvider.CreateCalls).To(HaveLen(1))
		claim := cloudProvider.CreateCalls[0]
		capacityType := pscheduling.NewNodeSelectorRequirementsWithMinValues(claim.Spec.Requirements...).Get(v1.CapacityTypeLabelKey)
		Expect(capacityType.Values()).To(ConsistOf(v1.CapacityTypeOnDemand, v1.CapacityTypeReserved))
		Expect(claim.Annotations).To(HaveKeyWithValue(v1.CapacityTypeTransitionsAnnotationKey, "on-demand,reserved"))
	},
		Entry("unconstrained workload", test.PodOptions{}),
		Entry("affinity allowing both states", test.PodOptions{NodeRequirements: []corev1.NodeSelectorRequirement{{Key: v1.CapacityTypeLabelKey, Operator: corev1.NodeSelectorOpIn, Values: []string{v1.CapacityTypeOnDemand, v1.CapacityTypeReserved}}}}),
	)
	DescribeTable("does not launch for incompatible pod capacity selectors", func(capacityType string) {
		ExpectApplied(ctx, env.Client, nodePool)
		pod := test.UnschedulablePod(test.PodOptions{NodeSelector: map[string]string{v1.CapacityTypeLabelKey: capacityType}})
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
		ExpectNotScheduled(ctx, env.Client, pod)
		Expect(cloudProvider.CreateCalls).To(BeEmpty())
	}, Entry("On-Demand", v1.CapacityTypeOnDemand), Entry("reserved", v1.CapacityTypeReserved), Entry("Spot", v1.CapacityTypeSpot))
	It("rejects affinity that excludes reserved capacity", func() {
		ExpectApplied(ctx, env.Client, nodePool)
		pod := test.UnschedulablePod(test.PodOptions{NodeRequirements: []corev1.NodeSelectorRequirement{{Key: v1.CapacityTypeLabelKey, Operator: corev1.NodeSelectorOpNotIn, Values: []string{v1.CapacityTypeReserved}}}})
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
		ExpectNotScheduled(ctx, env.Client, pod)
		Expect(cloudProvider.CreateCalls).To(BeEmpty())
	})
	It("does not use mutable offerings for an On-Demand-only NodePool", func() {
		nodePool.Spec.Template.Spec.Requirements[0].Values = []string{v1.CapacityTypeOnDemand}
		ExpectApplied(ctx, env.Client, nodePool)
		pod := test.UnschedulablePod()
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
		ExpectNotScheduled(ctx, env.Client, pod)
		Expect(cloudProvider.CreateCalls).To(BeEmpty())
	})
	It("does not launch when hard capacity-type topology selects only one coverage state", func() {
		ExpectApplied(ctx, env.Client, nodePool)
		pod := test.UnschedulablePod(test.PodOptions{TopologySpreadConstraints: []corev1.TopologySpreadConstraint{{
			MaxSkew: 1, TopologyKey: v1.CapacityTypeLabelKey, WhenUnsatisfiable: corev1.DoNotSchedule,
			LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "group"}},
		}}})
		pod.Labels["app"] = "group"
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
		ExpectNotScheduled(ctx, env.Client, pod)
		Expect(cloudProvider.CreateCalls).To(BeEmpty())
	})
	It("continues to support zone topology for mutable offerings", func() {
		ExpectApplied(ctx, env.Client, nodePool)
		pod := test.UnschedulablePod(test.PodOptions{TopologySpreadConstraints: []corev1.TopologySpreadConstraint{{
			MaxSkew: 1, TopologyKey: corev1.LabelTopologyZone, WhenUnsatisfiable: corev1.DoNotSchedule,
			LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "group"}},
		}}})
		pod.Labels["app"] = "group"
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
		ExpectScheduled(ctx, env.Client, pod)
	})
	It("preserves ordinary On-Demand-only provisioning", func() {
		cloudProvider.InstanceTypes[0].Offerings[0].CapacityTypeTransitions = nil
		ExpectApplied(ctx, env.Client, nodePool)
		pod := test.UnschedulablePod(test.PodOptions{NodeSelector: map[string]string{v1.CapacityTypeLabelKey: v1.CapacityTypeOnDemand}})
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
		ExpectScheduled(ctx, env.Client, pod)
		Expect(cloudProvider.CreateCalls[0].Annotations).ToNot(HaveKey(v1.CapacityTypeTransitionsAnnotationKey))
	})
	DescribeTable("does not place incompatible pods on existing mutable nodes", func(currentType, requestedType string) {
		ExpectApplied(ctx, env.Client, nodePool)
		initialPod := test.UnschedulablePod()
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, initialPod)
		node := ExpectScheduled(ctx, env.Client, initialPod)
		node.Labels[v1.CapacityTypeLabelKey] = currentType
		ExpectApplied(ctx, env.Client, node)
		Expect(cluster.UpdateNode(ctx, node)).To(Succeed())
		cloudProvider.CreateCalls = nil
		pod := test.UnschedulablePod(test.PodOptions{NodeSelector: map[string]string{v1.CapacityTypeLabelKey: requestedType}})
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
		ExpectNotScheduled(ctx, env.Client, pod)
		Expect(cloudProvider.CreateCalls).To(BeEmpty())
	}, Entry("uncovered", v1.CapacityTypeOnDemand, v1.CapacityTypeOnDemand), Entry("covered", v1.CapacityTypeReserved, v1.CapacityTypeReserved))
	It("still places unconstrained pods on existing mutable nodes", func() {
		ExpectApplied(ctx, env.Client, nodePool)
		initialPod := test.UnschedulablePod()
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, initialPod)
		node := ExpectScheduled(ctx, env.Client, initialPod)
		cloudProvider.CreateCalls = nil
		pod := test.UnschedulablePod()
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
		Expect(ExpectScheduled(ctx, env.Client, pod).Name).To(Equal(node.Name))
		Expect(cloudProvider.CreateCalls).To(BeEmpty())
	})
	It("preserves Spot-only launch behavior", func() {
		cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{fake.NewInstanceType("spot",
			fake.WithOfferings(cloudprovider.Offering{Requirements: pscheduling.NewLabelRequirements(map[string]string{v1.CapacityTypeLabelKey: v1.CapacityTypeSpot, corev1.LabelTopologyZone: "test-zone-1"}), Available: true, Price: 0.5}),
		)}
		ExpectApplied(ctx, env.Client, nodePool)
		pod := test.UnschedulablePod(test.PodOptions{NodeSelector: map[string]string{v1.CapacityTypeLabelKey: v1.CapacityTypeSpot}})
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
		ExpectScheduled(ctx, env.Client, pod)
		Expect(cloudProvider.CreateCalls[0].Annotations).ToNot(HaveKey(v1.CapacityTypeTransitionsAnnotationKey))
	})
	It("rejects fixed reservation metadata on mutable offerings", func() {
		key := cloudprovider.ReservationIDLabel
		Expect(cloudprovider.ReservedCapacityLabels.Has(key)).To(BeTrue())
		reqs := pscheduling.NewRequirements(pscheduling.NewRequirement(key, corev1.NodeSelectorOpDoesNotExist))
		Expect(cloudProvider.InstanceTypes[0].Offerings.Compatible(reqs)).To(BeEmpty())
	})
	It("does not synthesize reserved launch offerings", func() {
		for _, offering := range cloudProvider.InstanceTypes[0].Offerings {
			Expect(offering.CapacityType()).To(Equal(v1.CapacityTypeOnDemand))
			Expect(offering.ReservationCapacity).To(BeZero())
		}
		Expect(sets.New(cloudProvider.InstanceTypes[0].Offerings[0].CapacityTypes()...)).To(HaveLen(2))
	})
	It("does not mutate shared template annotations during parallel claim creation", func() {
		nodePool.Spec.Template.Annotations = map[string]string{v1.CapacityTypeTransitionsAnnotationKey: "reserved"}
		template := scheduling.NewNodeClaimTemplate(nodePool)
		template.IsStaticNodeClaim = true
		claims := make([]*v1.NodeClaim, 16)
		var wg sync.WaitGroup
		for index := range claims {
			copy := *template
			wg.Go(func() { claims[index] = copy.ToNodeClaim() })
		}
		wg.Wait()
		Expect(template.Annotations).To(HaveKeyWithValue(v1.CapacityTypeTransitionsAnnotationKey, "reserved"))
		for _, claim := range claims {
			Expect(claim.Annotations).ToNot(HaveKey(v1.CapacityTypeTransitionsAnnotationKey))
		}
	})
})
