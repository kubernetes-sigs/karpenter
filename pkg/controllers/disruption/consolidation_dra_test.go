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

package disruption_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
	"sigs.k8s.io/karpenter/pkg/controllers/disruption"
	pscheduling "sigs.k8s.io/karpenter/pkg/controllers/provisioning/scheduling"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
	"sigs.k8s.io/karpenter/pkg/utils/pdb"
)

// These tests exercise DRA behavior through the disruption/consolidation SimulateScheduling path. They drive
// SimulateScheduling directly with a consolidation candidate (rather than the full reconcile + queue machinery), which
// is the same approach the suite uses elsewhere and isolates the two DRA-specific concerns:
//   - (A) ResourceSlices owned by a candidate node (simulated for deletion) are not considered scheduling targets.
//   - (B) Devices held by candidate pods (which would be deallocated) are freed and their claims re-allocated.
var _ = Describe("Consolidation/DRA", func() {
	var nodePool *v1.NodePool

	BeforeEach(func() {
		if env.Version.Minor() < 34 {
			Skip("DRA is only available in K8s versions >= 1.34.x")
		}
		// Enable DRA (gated off by default). The suite's BeforeEach resets options, and runs before this one, so this
		// re-enables DRA for the consolidation simulation.
		ctx = options.ToContext(ctx, test.Options(test.OptionsFields{IgnoreDRARequests: lo.ToPtr(false)}))
		nodePool = test.NodePool()
		nodePool.Spec.Disruption.ConsolidationPolicy = v1.ConsolidationPolicyWhenEmptyOrUnderutilized
		nodePool.Spec.Disruption.ConsolidateAfter = v1.MustParseNillableDuration("0s")
	})

	// simulateConsolidation builds a disruption candidate for each given node and runs SimulateScheduling against them,
	// reconciling the deviceallocation controller first so the allocator sees the current allocated-device state. It
	// returns the simulation results, which describe where the candidates' pods would reschedule.
	simulateConsolidation := func(nodes ...*corev1.Node) pscheduling.Results {
		GinkgoHelper()
		ExpectDeviceAllocationReconciled(ctx, env.Client, draController)
		pdbs, err := pdb.NewLimits(ctx, env.Client)
		Expect(err).To(Succeed())
		nodePoolMap, nodePoolToInstanceTypesMap, err := disruption.BuildNodePoolMap(ctx, env.Client, cloudProvider)
		Expect(err).To(Succeed())
		candidates := lo.Map(nodes, func(node *corev1.Node, _ int) *disruption.Candidate {
			stateNode := ExpectStateNodeExists(cluster, node)
			candidate, err := disruption.NewCandidate(ctx, env.Client, recorder, env.Clock, stateNode, pdbs, nodePoolMap, nodePoolToInstanceTypesMap, queue, disruption.GracefulDisruptionClass)
			Expect(err).To(Succeed())
			return candidate
		})
		results, err := disruption.SimulateScheduling(ctx, env.Client, cluster, prov, env.Clock, recorder, []pscheduling.Options{pscheduling.IsConsolidationSimulation}, candidates...)
		Expect(err).To(Succeed())
		return results
	}

	// gpuNodeClaimAndNode builds a managed NodeClaim+Node pair of the given instance type, applies it, and informs
	// cluster state so it is evaluated as an existing node. The node carries a hostname label and a UID so node-local
	// published ResourceSlices can target it.
	gpuNodeClaimAndNode := func(instanceType string) (*v1.NodeClaim, *corev1.Node) {
		GinkgoHelper()
		nodeClaim, node := test.NodeClaimAndNode(v1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1.NodePoolLabelKey:            nodePool.Name,
					corev1.LabelInstanceTypeStable: instanceType,
				},
			},
			Status: v1.NodeClaimStatus{
				Allocatable: corev1.ResourceList{
					corev1.ResourceCPU:  resource.MustParse("16"),
					corev1.ResourcePods: resource.MustParse("100"),
				},
			},
		})
		node.Labels[corev1.LabelHostname] = node.Name
		ExpectApplied(ctx, env.Client, nodeClaim, node)
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, []*corev1.Node{node}, []*v1.NodeClaim{nodeClaim})
		return nodeClaim, ExpectExists(ctx, env.Client, node)
	}

	// gpuPodOnNode builds a reschedulable (ReplicaSet-owned) pod with a GPU ResourceClaim, binds it to the node, and
	// marks the claim allocated to the node's published device and reserved for the pod. This is the state a consolidation
	// candidate node is in: a pod holding a device that must be re-allocated if the node is removed.
	gpuPodOnNode := func(node *corev1.Node, claimName, pool, device string) *corev1.Pod {
		GinkgoHelper()
		rs := test.ReplicaSet()
		ExpectApplied(ctx, env.Client, rs)
		pod := test.Pod(test.PodOptions{
			ObjectMeta: metav1.ObjectMeta{OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1", Kind: "ReplicaSet", Name: rs.Name, UID: rs.UID,
				Controller: lo.ToPtr(true), BlockOwnerDeletion: lo.ToPtr(true),
			}}},
			ResourceClaims:          []corev1.PodResourceClaim{test.PodResourceClaimReference("gpu", claimName)},
			ContainerResourceClaims: []corev1.ResourceClaim{{Name: "gpu"}},
		})
		ExpectApplied(ctx, env.Client, pod)
		ExpectManualBinding(ctx, env.Client, pod, node)
		claim := test.AllocatedClusterWideClaim(claimName, pool, test.GPUDriver, device, test.PodConsumer(pod))
		ExpectApplied(ctx, env.Client, claim)
		return pod
	}

	It("re-allocates a candidate pod's device onto a replacement (B, positive)", func() {
		// One GPU instance type with a single template GPU. The candidate node hosts a pod holding the node's published
		// GPU. Consolidating the candidate must free that device and re-allocate the claim onto a replacement NodeClaim's
		// template GPU — so the simulation produces a replacement and no pod errors.
		cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{fake.GPUInstanceType("gpu-it", 1)}
		ExpectApplied(ctx, env.Client, nodePool, test.DeviceClassWithSelector("gpu", test.GPUDriver))
		_, node := gpuNodeClaimAndNode("gpu-it")
		ExpectApplied(ctx, env.Client, test.NodeLocalSlice(node, test.GPUDriver, "incluster-gpu-0"))
		pod := gpuPodOnNode(node, "gpu-claim", test.NodeLocalPoolName(test.GPUDriver, node.Name), "incluster-gpu-0")

		results := simulateConsolidation(node)

		// The pod reschedules with no error, onto a freshly launched NodeClaim (the candidate's published slice is
		// excluded, so the only way to satisfy the claim is a new node's template GPU).
		Expect(results.PodErrors[pod]).To(BeNil())
		Expect(results.NewNodeClaims).To(HaveLen(1))
	})

	It("does not target a candidate node's published device (A, positive)", func() {
		// Two GPU instance types' worth of nodes: a candidate node C (publishing a GPU) and a second node K with a free
		// published GPU but no spare CPU room is not used — instead we assert the replacement is a NEW node, never C
		// itself, proving C's slice was excluded from the scheduling universe.
		cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{fake.GPUInstanceType("gpu-it", 1)}
		ExpectApplied(ctx, env.Client, nodePool, test.DeviceClassWithSelector("gpu", test.GPUDriver))
		_, node := gpuNodeClaimAndNode("gpu-it")
		ExpectApplied(ctx, env.Client, test.NodeLocalSlice(node, test.GPUDriver, "incluster-gpu-0"))
		pod := gpuPodOnNode(node, "gpu-claim", test.NodeLocalPoolName(test.GPUDriver, node.Name), "incluster-gpu-0")

		results := simulateConsolidation(node)

		Expect(results.PodErrors[pod]).To(BeNil())
		// The replacement is a new NodeClaim, and it does not reuse the candidate node as an existing scheduling target.
		Expect(results.NewNodeClaims).To(HaveLen(1))
		for _, en := range results.ExistingNodes {
			Expect(en.Name()).ToNot(Equal(node.Name), "the candidate node must not be a scheduling target")
		}
	})

	It("does not consolidate when the candidate's claim cannot be satisfied elsewhere (B, negative)", func() {
		// The only instance type that provides the GPU is the candidate's own, and it offers exactly one device which the
		// candidate pod holds. Once freed it can only be re-acquired on a brand-new node of the same type — which is a
		// valid replacement. To make it genuinely unsatisfiable, restrict the NodePool so no instance type provides a GPU
		// template: the claim cannot be re-allocated anywhere, so the pod errors and consolidation cannot proceed.
		cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{fake.NewInstanceType("no-gpu-it")}
		ExpectApplied(ctx, env.Client, nodePool, test.DeviceClassWithSelector("gpu", test.GPUDriver))
		// The candidate node is a gpu-it that no longer exists in the offered instance types; its published slice is
		// excluded as a candidate, and no other node/type can provide the device.
		_, node := gpuNodeClaimAndNode("gpu-it")
		ExpectApplied(ctx, env.Client, test.NodeLocalSlice(node, test.GPUDriver, "incluster-gpu-0"))
		pod := gpuPodOnNode(node, "gpu-claim", test.NodeLocalPoolName(test.GPUDriver, node.Name), "incluster-gpu-0")

		results := simulateConsolidation(node)
		_ = pod

		// The claim can't be re-allocated, so the candidate pod fails to schedule and no replacement is produced —
		// consolidation cannot proceed. (PodErrors is keyed by the scheduler's internal pod copies, so we assert on the
		// pointer-independent signals.)
		Expect(results.NewNodeClaims).To(BeEmpty())
		Expect(results.AllNonPendingPodsScheduled()).To(BeFalse())
	})

	It("does not free a device held by a live pod on a non-candidate node (A, negative)", func() {
		// A single cluster-wide GPU device is held by a LIVE pod on a non-candidate node. The candidate pod also needs a
		// GPU. Because the live pod isn't deleting, its device is neither freed nor its claim reclassified, so there is no
		// second GPU available and the candidate pod cannot reschedule.
		cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{fake.NewInstanceType("no-gpu-it")}
		ExpectApplied(ctx, env.Client, nodePool, test.DeviceClassWithSelector("gpu", test.GPUDriver))
		// One cluster-wide device total.
		ExpectApplied(ctx, env.Client, test.ClusterWideSlice("shared-gpu-pool", test.GPUDriver, "shared-gpu-0"))

		// Live pod on a non-candidate node holds the only device.
		_, liveNode := gpuNodeClaimAndNode("no-gpu-it")
		livePod := test.Pod()
		ExpectApplied(ctx, env.Client, livePod)
		ExpectManualBinding(ctx, env.Client, livePod, liveNode)
		ExpectApplied(ctx, env.Client, test.AllocatedClusterWideClaim("held-claim", "shared-gpu-pool", test.GPUDriver, "shared-gpu-0", test.PodConsumer(livePod)))

		// Candidate node hosts a pod that also needs a GPU.
		_, candidateNode := gpuNodeClaimAndNode("no-gpu-it")
		candidatePod := gpuPodOnNode(candidateNode, "candidate-claim", "shared-gpu-pool", "shared-gpu-0")

		results := simulateConsolidation(candidateNode)
		_ = candidatePod

		// The live pod's device is not available (not freed, claim not reclassified), so the candidate pod can't
		// reschedule and consolidation cannot proceed.
		Expect(results.NewNodeClaims).To(BeEmpty())
		Expect(results.AllNonPendingPodsScheduled()).To(BeFalse())
	})

	It("re-allocates devices from multiple candidate nodes onto one replacement (B, multi-candidate)", func() {
		// Two candidate nodes, each hosting a GPU pod holding its node's published device. A single replacement instance
		// type with two template GPUs can host both pods. Multi-node consolidation must free both candidates' devices
		// (B1 over all candidates) and re-allocate both claims (B2) onto the one replacement.
		cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{fake.GPUInstanceType("gpu-it", 2)}
		ExpectApplied(ctx, env.Client, nodePool, test.DeviceClassWithSelector("gpu", test.GPUDriver))
		_, node1 := gpuNodeClaimAndNode("gpu-it")
		_, node2 := gpuNodeClaimAndNode("gpu-it")
		ExpectApplied(ctx, env.Client, test.NodeLocalSlice(node1, test.GPUDriver, "incluster-gpu-0"))
		ExpectApplied(ctx, env.Client, test.NodeLocalSlice(node2, test.GPUDriver, "incluster-gpu-0"))
		pod1 := gpuPodOnNode(node1, "gpu-claim-1", test.NodeLocalPoolName(test.GPUDriver, node1.Name), "incluster-gpu-0")
		pod2 := gpuPodOnNode(node2, "gpu-claim-2", test.NodeLocalPoolName(test.GPUDriver, node2.Name), "incluster-gpu-0")

		results := simulateConsolidation(node1, node2)

		// Both pods reschedule with no error onto a single replacement NodeClaim (its two template GPUs satisfy both
		// reclassified claims). Neither candidate node is reused as a scheduling target.
		Expect(results.PodErrors[pod1]).To(BeNil())
		Expect(results.PodErrors[pod2]).To(BeNil())
		Expect(results.NewNodeClaims).To(HaveLen(1))
		Expect(results.AllNonPendingPodsScheduled()).To(BeTrue())
		for _, en := range results.ExistingNodes {
			Expect([]string{node1.Name, node2.Name}).ToNot(ContainElement(en.Name()), "candidate nodes must not be scheduling targets")
		}
	})

	It("reclaims only the candidate's share of a shared device, leaving the live pod's share (B, partial)", func() {
		// Multi-allocatable devices / consumable capacity require the DRAConsumableCapacity feature, GA in k8s 1.36.
		if env.Version.Minor() < 36 {
			Skip("Consumable capacity requires K8s versions >= 1.36.x (DRAConsumableCapacity feature gate)")
		}
		// A cluster-wide multi-allocatable device with 16Gi capacity, shared between a live pod (4Gi) on a surviving node
		// and a candidate pod (10Gi). Consolidating the candidate must reclaim only its 10Gi share — the device stays
		// pinned for the live pod — and re-allocate the candidate's claim back onto the same shared device.
		cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{fake.NewInstanceType("no-gpu-it")}
		ExpectApplied(ctx, env.Client, nodePool, test.DeviceClassWithSelector("gpu", test.GPUDriver))
		ExpectApplied(ctx, env.Client, test.SharedCapacitySlice("shared-gpu-pool", test.GPUDriver, "shared-gpu-0", "16Gi"))

		// Live pod on a surviving (non-candidate) node holds 4Gi — this share must NOT be reclaimed.
		_, liveNode := gpuNodeClaimAndNode("no-gpu-it")
		livePod := test.Pod()
		ExpectApplied(ctx, env.Client, livePod)
		ExpectManualBinding(ctx, env.Client, livePod, liveNode)
		ExpectApplied(ctx, env.Client, test.AllocatedSharedClaim("live-claim", "shared-gpu-pool", test.GPUDriver, "shared-gpu-0", test.CapacityRequest("4Gi"), test.PodConsumer(livePod)))

		// Candidate pod holds 10Gi of the same shared device.
		rs := test.ReplicaSet()
		ExpectApplied(ctx, env.Client, rs)
		_, candidateNode := gpuNodeClaimAndNode("no-gpu-it")
		candidatePod := test.Pod(test.PodOptions{
			ObjectMeta: metav1.ObjectMeta{OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1", Kind: "ReplicaSet", Name: rs.Name, UID: rs.UID,
				Controller: lo.ToPtr(true), BlockOwnerDeletion: lo.ToPtr(true),
			}}},
			ResourceClaims:          []corev1.PodResourceClaim{test.PodResourceClaimReference("gpu", "candidate-claim")},
			ContainerResourceClaims: []corev1.ResourceClaim{{Name: "gpu"}},
		})
		ExpectApplied(ctx, env.Client, candidatePod)
		ExpectManualBinding(ctx, env.Client, candidatePod, candidateNode)
		ExpectApplied(ctx, env.Client, test.AllocatedSharedClaim("candidate-claim", "shared-gpu-pool", test.GPUDriver, "shared-gpu-0", test.CapacityRequest("10Gi"), test.PodConsumer(candidatePod)))

		results := simulateConsolidation(candidateNode)

		// The candidate pod reschedules: its 10Gi share is reclaimed (B1 partial subtraction) and its claim reclassified
		// (B2), re-allocating onto the shared device whose remaining capacity (16 - 4 live = 12Gi) now fits 10Gi. The
		// live pod's 4Gi is untouched. no-gpu-it provides no template GPU, so the cluster-wide device is the only option.
		Expect(results.PodErrors[candidatePod]).To(BeNil())
		Expect(results.AllNonPendingPodsScheduled()).To(BeTrue())
	})
})
