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

package provisioning_test

import (
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
	"sigs.k8s.io/karpenter/pkg/controllers/dynamicresources/deviceallocation"
	"sigs.k8s.io/karpenter/pkg/controllers/provisioning"
	"sigs.k8s.io/karpenter/pkg/events"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
)

// The DRA test fixtures are shared across provisioning and disruption suites. Slice/claim builders live in pkg/test;
// instance-type builders live in pkg/cloudprovider/fake (which imports pkg/test). These thin local aliases keep the
// existing call sites in this file terse.
const (
	gpuDriver      = test.GPUDriver
	nicDriver      = test.NICDriver
	capacityMemory = test.CapacityMemory
	counterSetName = "gpu-slices"
)

var (
	clusterWideSlice          = test.ClusterWideSlice
	zonedSlice                = test.ZonedSlice
	sharedCapacitySlice       = test.SharedCapacitySlice
	allocatedClusterWideClaim = test.AllocatedClusterWideClaim
	allocatedSharedClaim      = test.AllocatedSharedClaim
	podConsumer               = test.PodConsumer
	gpuAndNICInstanceType     = fake.GPUAndNICInstanceType
)

//nolint:unparam
func gpuInstanceType(name string, count int) *cloudprovider.InstanceType {
	return fake.GPUInstanceType(name, count)
}

//nolint:unparam
func nodeLocalSlice(node *corev1.Node, driver string, deviceNames ...string) *resourcev1.ResourceSlice {
	return test.NodeLocalSlice(node, driver, deviceNames...)
}

// capacity builds a single-dimension (memory) capacity request map for the given quantity.
func capacity(amount string) map[resourcev1.QualifiedName]resource.Quantity {
	return test.CapacityRequest(amount)
}

//nolint:unparam
func capacityGPUInstanceType(name, totalMemory string) *cloudprovider.InstanceType {
	return fake.CapacityGPUInstanceType(name, totalMemory, nil)
}

func capacityPolicyGPUInstanceType(name, totalMemory string, policy *resourcev1.CapacityRequestPolicy) *cloudprovider.InstanceType {
	return fake.CapacityGPUInstanceType(name, totalMemory, policy)
}

//nolint:unparam
func partitionableGPUInstanceType(name string, budget map[string]resource.Quantity, profiles int, perProfile map[string]resource.Quantity) *cloudprovider.InstanceType {
	return fake.PartitionableGPUInstanceType(name, counterSetName, budget, profiles, perProfile)
}

// quantities is a small convenience for building single- or multi-dimension counter maps inline.
func quantities(pairs ...string) map[string]resource.Quantity {
	m := map[string]resource.Quantity{}
	for i := 0; i+1 < len(pairs); i += 2 {
		m[pairs[i]] = resource.MustParse(pairs[i+1])
	}
	return m
}

// draPod builds an unschedulable pod referencing the named ResourceClaim from its container.
//
//nolint:unparam
func draPod(claimRef, claimName string) *corev1.Pod {
	return draPodForClaims(test.PodResourceClaimReference(claimRef, claimName))
}

// draPodForClaims builds an unschedulable pod that references each of the provided pod resource claims from its
// container.
func draPodForClaims(claims ...corev1.PodResourceClaim) *corev1.Pod {
	return test.UnschedulablePod(test.PodOptions{
		ResourceClaims: claims,
		ContainerResourceClaims: lo.Map(claims, func(c corev1.PodResourceClaim, _ int) corev1.ResourceClaim {
			return corev1.ResourceClaim{Name: c.Name}
		}),
	})
}

var _ = Describe("Dynamic Resource Allocation", func() {
	var nodePool *v1.NodePool
	var draProvisioner *provisioning.Provisioner
	var draController *deviceallocation.Controller

	BeforeEach(func() {
		if env.Version.Minor() < 34 {
			Skip("DRA is only available in K8s versions >= 1.34.x")
		}
		// DRA support is gated off by default; enable it for these tests.
		ctx = options.ToContext(ctx, test.Options(test.OptionsFields{IgnoreDRARequests: lo.ToPtr(false)}))
		nodePool = test.NodePool()
		// Use a provisioner backed by a fresh deviceallocation controller so each test starts with clean
		// allocated-device tracking state. The controller must be reconciled (hydrated) before a provisioning round
		// that relies on the in-cluster allocated-device set — see provisionDRA.
		draController = deviceallocation.NewController(env.Client)
		draProvisioner = provisioning.NewProvisioner(env.Client, events.NewRecorder(&record.FakeRecorder{}), cloudProvider, cluster, env.Clock, draController)
	})

	// provisionDRA reconciles the deviceallocation controller (so the allocator sees the current in-cluster allocated
	// devices) and then runs a provisioning round.
	provisionDRA := func(pods ...*corev1.Pod) ProvisioningResult {
		GinkgoHelper()
		ExpectDeviceAllocationReconciled(ctx, env.Client, draController)
		return ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, draProvisioner, pods...)
	}

	// existingNode creates a managed node belonging to nodePool with the given instance type and capacity, applies it,
	// optionally marks it initialized, and registers it in cluster state so the scheduler evaluates it as an existing
	// node. The returned node carries a UID (for ResourceSlice owner references) and a hostname label (so node-local
	// published slices can target it via a NodeSelector).
	existingNode := func(instanceType string, initialized bool, capacity corev1.ResourceList) *corev1.Node {
		GinkgoHelper()
		node := test.Node(test.NodeOptions{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1.NodePoolLabelKey:            nodePool.Name,
					corev1.LabelInstanceTypeStable: instanceType,
					v1.NodeRegisteredLabelKey:      "true",
				},
				Finalizers: []string{v1.TerminationFinalizer},
			},
			// A provider ID is required for the node to be tracked in cluster state and evaluated as an existing node.
			ProviderID:  test.RandomProviderID(),
			Capacity:    capacity,
			Allocatable: capacity,
		})
		node.Labels[corev1.LabelHostname] = node.Name
		ExpectApplied(ctx, env.Client, node)
		if initialized {
			ExpectMakeNodesInitialized(ctx, env.Client, env.Clock, node)
		}
		node = ExpectExists(ctx, env.Client, node)
		ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))
		return node
	}

	Context("Single unallocated claim (A)", func() {
		It("should provision a new NodeClaim, allocate the claim, and annotate the drivers (A1)", func() {
			cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{gpuInstanceType("gpu-it", 2)}
			ExpectApplied(ctx, env.Client, nodePool, test.DeviceClassWithSelector("gpu", gpuDriver))
			claim := test.ResourceClaimForRequests("gpu-claim", test.ExactDeviceRequest("req", "gpu", 1))
			ExpectApplied(ctx, env.Client, claim)

			pod := draPod("gpu", "gpu-claim")
			provisionDRA(pod)

			node := ExpectScheduled(ctx, env.Client, pod)
			Expect(node.Labels[corev1.LabelInstanceTypeStable]).To(Equal("gpu-it"))

			nodeClaims := ExpectNodeClaims(ctx, env.Client)
			Expect(nodeClaims).To(HaveLen(1))
			ExpectNodeClaimDRADrivers(nodeClaims[0], gpuDriver)
			ExpectResourceClaimAllocated(ctx, env.Client, claim.Namespace, claim.Name, gpuDriver)
		})
		It("should restrict instance types to those providing the requested device (A2)", func() {
			// Only gpu-it provides the GPU device; no-gpu-it does not.
			cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{
				gpuInstanceType("gpu-it", 1),
				fake.NewInstanceType("no-gpu-it"),
			}
			ExpectApplied(ctx, env.Client, nodePool, test.DeviceClassWithSelector("gpu", gpuDriver))
			ExpectApplied(ctx, env.Client, test.ResourceClaimForRequests("gpu-claim", test.ExactDeviceRequest("req", "gpu", 1)))

			pod := draPod("gpu", "gpu-claim")
			provisionDRA(pod)

			ExpectScheduled(ctx, env.Client, pod)
			nodeClaims := ExpectNodeClaims(ctx, env.Client)
			Expect(nodeClaims).To(HaveLen(1))
			Expect(nodeClaims[0].Status.NodeName).ToNot(BeEmpty())
			Expect(nodeClaims[0].Labels[corev1.LabelInstanceTypeStable]).To(Equal("gpu-it"))
		})
		It("should not annotate or allocate for a non-DRA pod (A3)", func() {
			cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{gpuInstanceType("gpu-it", 1)}
			ExpectApplied(ctx, env.Client, nodePool)

			pod := test.UnschedulablePod()
			provisionDRA(pod)

			ExpectScheduled(ctx, env.Client, pod)
			nodeClaims := ExpectNodeClaims(ctx, env.Client)
			Expect(nodeClaims).To(HaveLen(1))
			Expect(nodeClaims[0].Annotations).ToNot(HaveKey(v1.DRADriversAnnotationKey))
		})
		It("should resolve and allocate a claim generated from a ResourceClaimTemplate (A4)", func() {
			cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{gpuInstanceType("gpu-it", 1)}
			ExpectApplied(ctx, env.Client, nodePool, test.DeviceClassWithSelector("gpu", gpuDriver))
			// The pod references a ResourceClaimTemplate; the framework generates the per-pod claim (the integration
			// analog of the in-cluster claim-template controller) before scheduling.
			ExpectApplied(ctx, env.Client, test.ResourceClaimTemplate(resourcev1.ResourceClaimTemplate{
				ObjectMeta: metav1.ObjectMeta{Name: "gpu-template", Namespace: "default"},
				Spec: resourcev1.ResourceClaimTemplateSpec{Spec: resourcev1.ResourceClaimSpec{
					Devices: resourcev1.DeviceClaim{Requests: []resourcev1.DeviceRequest{test.ExactDeviceRequest("req", "gpu", 1)}},
				}},
			}))

			pod := draPodForClaims(test.PodResourceClaimTemplateReference("gpu", "gpu-template"))
			provisionDRA(pod)

			ExpectScheduled(ctx, env.Client, pod)
			nodeClaims := ExpectNodeClaims(ctx, env.Client)
			Expect(nodeClaims).To(HaveLen(1))
			ExpectNodeClaimDRADrivers(nodeClaims[0], gpuDriver)
			// The generated claim is named "<pod>-<claimRef>" by the processing expectation and ends up allocated.
			ExpectResourceClaimAllocated(ctx, env.Client, pod.Namespace, fmt.Sprintf("%s-gpu", pod.Name), gpuDriver)
		})
	})

	Context("Template (in-flight) device allocation (B)", func() {
		It("should pack multiple pods onto one NodeClaim using distinct devices (B1)", func() {
			cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{gpuInstanceType("gpu-it", 2)}
			ExpectApplied(ctx, env.Client, nodePool, test.DeviceClassWithSelector("gpu", gpuDriver))
			ExpectApplied(ctx, env.Client,
				test.ResourceClaimForRequests("claim-a", test.ExactDeviceRequest("req", "gpu", 1)),
				test.ResourceClaimForRequests("claim-b", test.ExactDeviceRequest("req", "gpu", 1)),
			)
			podA := draPod("gpu", "claim-a")
			podB := draPod("gpu", "claim-b")
			provisionDRA(podA, podB)

			nodeA := ExpectScheduled(ctx, env.Client, podA)
			nodeB := ExpectScheduled(ctx, env.Client, podB)
			Expect(nodeA.Name).To(Equal(nodeB.Name), "both pods should share a single NodeClaim's two devices")
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(1))

			devicesA := ExpectResourceClaimAllocated(ctx, env.Client, "default", "claim-a", gpuDriver)
			devicesB := ExpectResourceClaimAllocated(ctx, env.Client, "default", "claim-b", gpuDriver)
			Expect(devicesA).ToNot(ConsistOf(devicesB), "claims must not share a device")
		})
		It("should create a second NodeClaim when per-node device capacity is exhausted (B2)", func() {
			cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{gpuInstanceType("gpu-it", 1)}
			ExpectApplied(ctx, env.Client, nodePool, test.DeviceClassWithSelector("gpu", gpuDriver))
			ExpectApplied(ctx, env.Client,
				test.ResourceClaimForRequests("claim-a", test.ExactDeviceRequest("req", "gpu", 1)),
				test.ResourceClaimForRequests("claim-b", test.ExactDeviceRequest("req", "gpu", 1)),
			)
			podA := draPod("gpu", "claim-a")
			podB := draPod("gpu", "claim-b")
			provisionDRA(podA, podB)

			nodeA := ExpectScheduled(ctx, env.Client, podA)
			nodeB := ExpectScheduled(ctx, env.Client, podB)
			Expect(nodeA.Name).ToNot(Equal(nodeB.Name), "single-device nodes can't host both pods")
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(2))
		})
	})

	Context("Allocation modes & constraints (F)", func() {
		It("should allocate every matching device in All mode (F1)", func() {
			cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{gpuInstanceType("gpu-it", 3)}
			ExpectApplied(ctx, env.Client, nodePool, test.DeviceClassWithSelector("gpu", gpuDriver))
			ExpectApplied(ctx, env.Client, test.ResourceClaimForRequests("gpu-claim", test.AllDeviceRequest("req", "gpu")))

			pod := draPod("gpu", "gpu-claim")
			provisionDRA(pod)

			ExpectScheduled(ctx, env.Client, pod)
			devices := ExpectResourceClaimAllocated(ctx, env.Client, "default", "gpu-claim", gpuDriver)
			Expect(devices).To(HaveLen(3), "All mode should allocate every device on the instance type")
		})
		It("should satisfy a MatchAttribute constraint across two requests (F2)", func() {
			// An instance type whose two GPU template devices share a concrete pcieRoot attribute. A claim with two
			// requests and a MatchAttribute constraint must pick devices sharing that attribute value. This is an
			// integration smoke test; deep constraint logic lives in the allocator unit tests. The attribute name must be
			// a domain-qualified C identifier per the resource.k8s.io API.
			const pcieRoot = gpuDriver + "/pcieRoot"
			it := fake.NewInstanceType("gpu-it", fake.WithResourceSliceTemplates(
				fake.ResourceSliceTemplate(gpuDriver, "gpu-it-pool",
					fake.Device("gpu-0", map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{pcieRoot: test.StringAttribute("root-a")}),
					fake.Device("gpu-1", map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{pcieRoot: test.StringAttribute("root-a")}),
				),
			))
			cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{it}
			ExpectApplied(ctx, env.Client, nodePool, test.DeviceClassWithSelector("gpu", gpuDriver))
			ExpectApplied(ctx, env.Client, test.ResourceClaim(resourcev1.ResourceClaim{
				ObjectMeta: metav1.ObjectMeta{Name: "gpu-claim", Namespace: "default"},
				Spec: resourcev1.ResourceClaimSpec{Devices: resourcev1.DeviceClaim{
					Requests: []resourcev1.DeviceRequest{
						test.ExactDeviceRequest("req-a", "gpu", 1),
						test.ExactDeviceRequest("req-b", "gpu", 1),
					},
					Constraints: []resourcev1.DeviceConstraint{test.MatchAttributeConstraint(pcieRoot)},
				}},
			}))

			pod := draPod("gpu", "gpu-claim")
			provisionDRA(pod)

			ExpectScheduled(ctx, env.Client, pod)
			devices := ExpectResourceClaimAllocated(ctx, env.Client, "default", "gpu-claim", gpuDriver)
			Expect(devices).To(HaveLen(2), "both requests are satisfied by attribute-sharing devices")
		})
	})

	Context("requested-dra-drivers annotation (G)", func() {
		It("should list every driver whose devices were allocated (G1)", func() {
			cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{gpuAndNICInstanceType("multi-it")}
			ExpectApplied(ctx, env.Client, nodePool,
				test.DeviceClassWithSelector("gpu", gpuDriver),
				test.DeviceClassWithSelector("nic", nicDriver),
			)
			ExpectApplied(ctx, env.Client,
				test.ResourceClaimForRequests("gpu-claim", test.ExactDeviceRequest("req", "gpu", 1)),
				test.ResourceClaimForRequests("nic-claim", test.ExactDeviceRequest("req", "nic", 1)),
			)
			pod := draPodForClaims(
				test.PodResourceClaimReference("gpu", "gpu-claim"),
				test.PodResourceClaimReference("nic", "nic-claim"),
			)
			provisionDRA(pod)

			ExpectScheduled(ctx, env.Client, pod)
			nodeClaims := ExpectNodeClaims(ctx, env.Client)
			Expect(nodeClaims).To(HaveLen(1))
			ExpectNodeClaimDRADrivers(nodeClaims[0], gpuDriver, nicDriver)
		})
	})

	Context("Multiple provisioner runs (H)", func() {
		It("should not double-allocate a device across runs (H1)", func() {
			cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{gpuInstanceType("gpu-it", 1)}
			ExpectApplied(ctx, env.Client, nodePool, test.DeviceClassWithSelector("gpu", gpuDriver))
			ExpectApplied(ctx, env.Client,
				test.ResourceClaimForRequests("claim-a", test.ExactDeviceRequest("req", "gpu", 1)),
				test.ResourceClaimForRequests("claim-b", test.ExactDeviceRequest("req", "gpu", 1)),
			)
			// Run 1: provision for the first pod, allocating gpu-it's single device on its node.
			podA := draPod("gpu", "claim-a")
			provisionDRA(podA)
			nodeA := ExpectScheduled(ctx, env.Client, podA)
			devicesA := ExpectResourceClaimAllocated(ctx, env.Client, "default", "claim-a", gpuDriver)

			// Initialize nodeA so its published in-cluster ResourceSlice (rather than templates) is gathered, and its
			// allocated device is tracked by the deviceallocation controller as in-use.
			ExpectMakeNodesInitialized(ctx, env.Client, env.Clock, nodeA)
			ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(nodeA))

			// Run 2: a second pod must not reuse the device already published+allocated on nodeA; it provisions a new node.
			podB := draPod("gpu", "claim-b")
			provisionDRA(podB)
			nodeB := ExpectScheduled(ctx, env.Client, podB)
			Expect(nodeB.Name).ToNot(Equal(nodeA.Name))
			devicesB := ExpectResourceClaimAllocated(ctx, env.Client, "default", "claim-b", gpuDriver)
			Expect(devicesB).ToNot(ConsistOf(devicesA), "run 2 must not reallocate run 1's device")
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(2))
		})
		It("should treat a claim allocated in a prior run as already-allocated (H2)", func() {
			cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{gpuInstanceType("gpu-it", 2)}
			ExpectApplied(ctx, env.Client, nodePool, test.DeviceClassWithSelector("gpu", gpuDriver))
			ExpectApplied(ctx, env.Client, test.ResourceClaimForRequests("shared-claim", test.ExactDeviceRequest("req", "gpu", 1)))

			// Run 1 allocates shared-claim and persists its status.
			podA := draPod("gpu", "shared-claim")
			provisionDRA(podA)
			nodeA := ExpectScheduled(ctx, env.Client, podA)
			devicesRun1 := ExpectResourceClaimAllocated(ctx, env.Client, "default", "shared-claim", gpuDriver)

			// Run 2: a different pod references the same already-allocated claim. No new allocation/DFS occurs and the
			// claim's allocation is unchanged.
			podB := draPod("gpu", "shared-claim")
			provisionDRA(podB)
			ExpectScheduled(ctx, env.Client, podB)
			devicesRun2 := ExpectResourceClaimAllocated(ctx, env.Client, "default", "shared-claim", gpuDriver)
			Expect(devicesRun2).To(ConsistOf(devicesRun1), "claim allocation must be stable across runs")
			_ = nodeA
		})
		It("should schedule on a later run once a missing claim is created (H3)", func() {
			cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{gpuInstanceType("gpu-it", 1)}
			ExpectApplied(ctx, env.Client, nodePool, test.DeviceClassWithSelector("gpu", gpuDriver))

			// Run 1: the referenced claim doesn't exist yet — pod is deferred, no NodeClaim.
			pod := draPod("gpu", "late-claim")
			ExpectApplied(ctx, env.Client, pod)
			ExpectDeviceAllocationReconciled(ctx, env.Client, draController)
			ExpectProvisionedNoBinding(ctx, env.Client, cluster, cloudProvider, draProvisioner)
			ExpectNotScheduled(ctx, env.Client, pod)
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(0))

			// Run 2: create the claim; the pod now schedules.
			ExpectApplied(ctx, env.Client, test.ResourceClaim(resourcev1.ResourceClaim{
				ObjectMeta: metav1.ObjectMeta{Name: "late-claim", Namespace: "default"},
				Spec:       resourcev1.ResourceClaimSpec{Devices: resourcev1.DeviceClaim{Requests: []resourcev1.DeviceRequest{test.ExactDeviceRequest("req", "gpu", 1)}}},
			}))
			provisionDRA(pod)
			ExpectScheduled(ctx, env.Client, pod)
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(1))
		})
	})

	Context("Enablement gating (N)", func() {
		It("should reject DRA pods with a DRAError when IgnoreDRARequests is enabled (N1)", func() {
			ctx = options.ToContext(ctx, test.Options(test.OptionsFields{IgnoreDRARequests: lo.ToPtr(true)}))
			cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{gpuInstanceType("gpu-it", 1)}
			ExpectApplied(ctx, env.Client, nodePool, test.DeviceClassWithSelector("gpu", gpuDriver))
			ExpectApplied(ctx, env.Client, test.ResourceClaimForRequests("gpu-claim", test.ExactDeviceRequest("req", "gpu", 1)))

			pod := draPod("gpu", "gpu-claim")
			// The provisioner ignores DRA entirely under this flag, so the device controller isn't consulted.
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectNotScheduled(ctx, env.Client, pod)
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(0))
		})
	})

	Context("Unsatisfiable / deferred (U)", func() {
		It("should defer a pod whose ResourceClaim does not exist yet (U1)", func() {
			cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{gpuInstanceType("gpu-it", 1)}
			ExpectApplied(ctx, env.Client, nodePool, test.DeviceClassWithSelector("gpu", gpuDriver))

			pod := draPod("gpu", "missing-claim")
			ExpectApplied(ctx, env.Client, pod)
			ExpectDeviceAllocationReconciled(ctx, env.Client, draController)
			ExpectProvisionedNoBinding(ctx, env.Client, cluster, cloudProvider, draProvisioner)
			ExpectNotScheduled(ctx, env.Client, pod)
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(0))
		})
		It("should fail to schedule when no instance type provides the requested device (U2)", func() {
			cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{fake.NewInstanceType("no-gpu-it")}
			ExpectApplied(ctx, env.Client, nodePool, test.DeviceClassWithSelector("gpu", gpuDriver))
			ExpectApplied(ctx, env.Client, test.ResourceClaimForRequests("gpu-claim", test.ExactDeviceRequest("req", "gpu", 1)))

			pod := draPod("gpu", "gpu-claim")
			provisionDRA(pod)
			ExpectNotScheduled(ctx, env.Client, pod)
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(0))
		})
		It("should fail to schedule when the DeviceClass is missing (U3)", func() {
			cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{gpuInstanceType("gpu-it", 1)}
			ExpectApplied(ctx, env.Client, nodePool)
			// Note: no DeviceClass applied.
			ExpectApplied(ctx, env.Client, test.ResourceClaimForRequests("gpu-claim", test.ExactDeviceRequest("req", "gpu", 1)))

			pod := draPod("gpu", "gpu-claim")
			provisionDRA(pod)
			ExpectNotScheduled(ctx, env.Client, pod)
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(0))
		})
		It("should skip a claim-template reference whose generated name is nil (U4)", func() {
			// A ResourceClaimTemplate reference whose status entry reports no generated claim name (claim generation was
			// unnecessary). The allocator must skip it without panicking or allocating, and the pod schedules normally.
			cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{gpuInstanceType("gpu-it", 1)}
			ExpectApplied(ctx, env.Client, nodePool, test.DeviceClassWithSelector("gpu", gpuDriver))

			pod := draPodForClaims(test.PodResourceClaimTemplateReference("gpu", "gpu-template"))
			// Pre-populate the status with a nil generated name so the processing expectation leaves it untouched and the
			// allocator exercises the "no claim needed" skip branch.
			pod.Status.ResourceClaimStatuses = []corev1.PodResourceClaimStatus{{Name: "gpu", ResourceClaimName: nil}}
			provisionDRA(pod)

			// No claim to allocate, so the pod schedules and no DRA driver annotation is recorded.
			ExpectScheduled(ctx, env.Client, pod)
			nodeClaims := ExpectNodeClaims(ctx, env.Client)
			Expect(nodeClaims).To(HaveLen(1))
			Expect(nodeClaims[0].Annotations).ToNot(HaveKey(v1.DRADriversAnnotationKey))
		})
	})

	Context("Conflicts & contention (X)", func() {
		It("should fail to schedule a pod whose two claims require incompatible zones (X1)", func() {
			// plain-it provides no template devices, so both claims must be satisfied by the zoned in-cluster pools —
			// forcing the genuine zone conflict (an instance type with a template GPU would let the gpu claim escape the
			// zone constraint).
			cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{fake.NewInstanceType("plain-it")}
			ExpectApplied(ctx, env.Client, nodePool,
				test.DeviceClassWithSelector("gpu", gpuDriver),
				test.DeviceClassWithSelector("nic", nicDriver),
			)
			ExpectApplied(ctx, env.Client,
				test.ResourceClaimForRequests("gpu-claim", test.ExactDeviceRequest("req", "gpu", 1)),
				test.ResourceClaimForRequests("nic-claim", test.ExactDeviceRequest("req", "nic", 1)),
			)
			// The two pools are pinned to different zones, so no single NodeClaim can satisfy both claims.
			ExpectApplied(ctx, env.Client,
				zonedSlice("zoned-gpu-pool", gpuDriver, "test-zone-1", "zoned-gpu-0"),
				zonedSlice("zoned-nic-pool", nicDriver, "test-zone-2", "zoned-nic-0"),
			)
			pod := draPodForClaims(
				test.PodResourceClaimReference("gpu", "gpu-claim"),
				test.PodResourceClaimReference("nic", "nic-claim"),
			)
			provisionDRA(pod)

			ExpectNotScheduled(ctx, env.Client, pod)
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(0))
		})
		It("should fail when DRA prunes all instance types that otherwise fit (X3)", func() {
			// Only the arm instance type provides the GPU device, but the pod requires amd64, so after DRA pruning no
			// instance type satisfies both constraints.
			cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{
				fake.NewInstanceType("arm-gpu-it",
					fake.WithArchitecture("arm64"),
					fake.WithResourceSliceTemplates(fake.ResourceSliceTemplate(gpuDriver, "arm-gpu-pool", fake.Devices("arm-gpu-0")...)),
				),
				fake.NewInstanceType("amd-no-gpu-it", fake.WithArchitecture("amd64")),
			}
			ExpectApplied(ctx, env.Client, nodePool, test.DeviceClassWithSelector("gpu", gpuDriver))
			ExpectApplied(ctx, env.Client, test.ResourceClaimForRequests("gpu-claim", test.ExactDeviceRequest("req", "gpu", 1)))

			pod := test.UnschedulablePod(test.PodOptions{
				ResourceClaims:          []corev1.PodResourceClaim{test.PodResourceClaimReference("gpu", "gpu-claim")},
				ContainerResourceClaims: []corev1.ResourceClaim{{Name: "gpu"}},
				NodeRequirements: []corev1.NodeSelectorRequirement{
					{Key: corev1.LabelArchStable, Operator: corev1.NodeSelectorOpIn, Values: []string{"amd64"}},
				},
			})
			provisionDRA(pod)
			ExpectNotScheduled(ctx, env.Client, pod)
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(0))
		})
	})

	Context("Cross-cutting / regression (R)", func() {
		It("should handle a mixed batch of DRA and non-DRA pods (R1)", func() {
			cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{gpuInstanceType("gpu-it", 1)}
			ExpectApplied(ctx, env.Client, nodePool, test.DeviceClassWithSelector("gpu", gpuDriver))
			ExpectApplied(ctx, env.Client, test.ResourceClaimForRequests("gpu-claim", test.ExactDeviceRequest("req", "gpu", 1)))

			draPod := draPod("gpu", "gpu-claim")
			plainPod := test.UnschedulablePod()
			provisionDRA(draPod, plainPod)

			ExpectScheduled(ctx, env.Client, draPod)
			ExpectScheduled(ctx, env.Client, plainPod)
			ExpectResourceClaimAllocated(ctx, env.Client, "default", "gpu-claim", gpuDriver)
		})
		It("should surface DRA allocation metadata keyed by claim namespaced name (R2)", func() {
			cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{gpuInstanceType("gpu-it", 1)}
			ExpectApplied(ctx, env.Client, nodePool, test.DeviceClassWithSelector("gpu", gpuDriver))
			ExpectApplied(ctx, env.Client, test.ResourceClaimForRequests("gpu-claim", test.ExactDeviceRequest("req", "gpu", 1)))

			pod := draPod("gpu", "gpu-claim")
			ExpectApplied(ctx, env.Client, pod)
			ExpectResourceClaimsProcessed(ctx, env.Client, pod)
			ExpectDeviceAllocationReconciled(ctx, env.Client, draController)
			results := ExpectProvisionedResults(ctx, env.Client, cluster, cloudProvider, draProvisioner, pod)
			Expect(results.DRAClaimAllocationMetadata).To(HaveKey(types.NamespacedName{Namespace: "default", Name: "gpu-claim"}))
		})
	})

	Context("In-cluster device allocation against existing nodes (C)", func() {
		It("should bind to an existing initialized node whose published device satisfies the claim (C1)", func() {
			cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{gpuInstanceType("gpu-it", 1)}
			ExpectApplied(ctx, env.Client, nodePool, test.DeviceClassWithSelector("gpu", gpuDriver))
			ExpectApplied(ctx, env.Client, test.ResourceClaimForRequests("gpu-claim", test.ExactDeviceRequest("req", "gpu", 1)))

			// An existing initialized node publishes a GPU device in-cluster.
			node := existingNode("gpu-it", true, corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("4Gi"), corev1.ResourcePods: resource.MustParse("10"),
			})
			ExpectApplied(ctx, env.Client, nodeLocalSlice(node, gpuDriver, "incluster-gpu-0"))

			pod := draPod("gpu", "gpu-claim")
			provisionDRA(pod)

			// The pod schedules to the existing node; no new NodeClaim is created.
			scheduled := ExpectScheduled(ctx, env.Client, pod)
			Expect(scheduled.Name).To(Equal(node.Name))
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(0))
			devices := ExpectResourceClaimAllocated(ctx, env.Client, "default", "gpu-claim", gpuDriver)
			Expect(devices).To(ConsistOf(test.NodeLocalPoolName(gpuDriver, node.Name) + "/incluster-gpu-0"))
		})
		It("should prefer an existing node's published device over launching a new node (C2)", func() {
			cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{gpuInstanceType("gpu-it", 1)}
			ExpectApplied(ctx, env.Client, nodePool, test.DeviceClassWithSelector("gpu", gpuDriver))
			ExpectApplied(ctx, env.Client, test.ResourceClaimForRequests("gpu-claim", test.ExactDeviceRequest("req", "gpu", 1)))

			// Existing node could satisfy the claim, and a new NodeClaim's template could too; the existing node wins.
			node := existingNode("gpu-it", true, corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("4Gi"), corev1.ResourcePods: resource.MustParse("10"),
			})
			ExpectApplied(ctx, env.Client, nodeLocalSlice(node, gpuDriver, "incluster-gpu-0"))

			pod := draPod("gpu", "gpu-claim")
			provisionDRA(pod)

			scheduled := ExpectScheduled(ctx, env.Client, pod)
			Expect(scheduled.Name).To(Equal(node.Name))
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(0))
		})
	})

	Context("Topology propagation from node-local devices (D)", func() {
		It("should tighten a new NodeClaim to the zone of a zoned in-cluster device (D1)", func() {
			cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{gpuInstanceType("gpu-it", 1)}
			ExpectApplied(ctx, env.Client, nodePool, test.DeviceClassWithSelector("gpu", gpuDriver))
			ExpectApplied(ctx, env.Client, test.ResourceClaimForRequests("gpu-claim", test.ExactDeviceRequest("req", "gpu", 1)))
			// A cluster-managed pool restricts its device to test-zone-2.
			ExpectApplied(ctx, env.Client, zonedSlice("zoned-gpu-pool", gpuDriver, "test-zone-2", "zoned-gpu-0"))

			pod := draPod("gpu", "gpu-claim")
			provisionDRA(pod)

			ExpectScheduled(ctx, env.Client, pod)
			nodeClaims := ExpectNodeClaims(ctx, env.Client)
			Expect(nodeClaims).To(HaveLen(1))
			// The zoned device's topology requirement is propagated onto the NodeClaim, constraining it to that zone.
			req, found := lo.Find(nodeClaims[0].Spec.Requirements, func(r v1.NodeSelectorRequirementWithMinValues) bool {
				return r.Key == corev1.LabelTopologyZone
			})
			Expect(found).To(BeTrue())
			Expect(req.Values).To(ConsistOf("test-zone-2"))
		})
		It("should collapse a NodeClaim to the shared zone of two compatibly-zoned claims (D2)", func() {
			// plain-it provides no templates, so both claims are satisfied by the zoned in-cluster pools and both
			// contribute their zone requirement — exercising the intersection of two device topologies.
			cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{fake.NewInstanceType("plain-it")}
			ExpectApplied(ctx, env.Client, nodePool,
				test.DeviceClassWithSelector("gpu", gpuDriver),
				test.DeviceClassWithSelector("nic", nicDriver),
			)
			ExpectApplied(ctx, env.Client,
				test.ResourceClaimForRequests("gpu-claim", test.ExactDeviceRequest("req", "gpu", 1)),
				test.ResourceClaimForRequests("nic-claim", test.ExactDeviceRequest("req", "nic", 1)),
			)
			// Two cluster-managed pools, both constrained to test-zone-2 — their topology requirements intersect.
			ExpectApplied(ctx, env.Client,
				zonedSlice("zoned-gpu-pool", gpuDriver, "test-zone-2", "zoned-gpu-0"),
				zonedSlice("zoned-nic-pool", nicDriver, "test-zone-2", "zoned-nic-0"),
			)
			pod := draPodForClaims(
				test.PodResourceClaimReference("gpu", "gpu-claim"),
				test.PodResourceClaimReference("nic", "nic-claim"),
			)
			provisionDRA(pod)

			ExpectScheduled(ctx, env.Client, pod)
			nodeClaims := ExpectNodeClaims(ctx, env.Client)
			Expect(nodeClaims).To(HaveLen(1))
			req, found := lo.Find(nodeClaims[0].Spec.Requirements, func(r v1.NodeSelectorRequirementWithMinValues) bool {
				return r.Key == corev1.LabelTopologyZone
			})
			Expect(found).To(BeTrue())
			Expect(req.Values).To(ConsistOf("test-zone-2"))
		})
	})

	Context("Cross-loop in-memory claim reuse (E)", func() {
		It("should allocate a shared in-cluster claim once and reuse it within a single loop (E1)", func() {
			// Two pods reference the same claim in one provisioning round. The claim is satisfied by a cluster-wide
			// in-cluster device (not a template), so it is allocated once and the second pod reuses that in-memory
			// allocation rather than re-running the DFS or allocating a second device.
			cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{fake.NewInstanceType("plain-it")}
			ExpectApplied(ctx, env.Client, nodePool, test.DeviceClassWithSelector("gpu", gpuDriver))
			ExpectApplied(ctx, env.Client, clusterWideSlice("shared-gpu-pool", gpuDriver, "shared-gpu-0", "shared-gpu-1"))
			ExpectApplied(ctx, env.Client, test.ResourceClaimForRequests("shared-claim", test.ExactDeviceRequest("req", "gpu", 1)))

			podA := draPod("gpu", "shared-claim")
			podB := draPod("gpu", "shared-claim")
			provisionDRA(podA, podB)

			ExpectScheduled(ctx, env.Client, podA)
			ExpectScheduled(ctx, env.Client, podB)
			// The shared claim is allocated exactly one device, shared by both pods.
			devices := ExpectResourceClaimAllocated(ctx, env.Client, "default", "shared-claim", gpuDriver)
			Expect(devices).To(HaveLen(1))
		})
		It("should land a second pod on the same NodeClaim for a template-allocated shared claim (E2)", func() {
			cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{gpuInstanceType("gpu-it", 2)}
			ExpectApplied(ctx, env.Client, nodePool, test.DeviceClassWithSelector("gpu", gpuDriver))
			ExpectApplied(ctx, env.Client, test.ResourceClaimForRequests("shared-claim", test.ExactDeviceRequest("req", "gpu", 1)))

			// Both pods reference the same claim within a single provisioning round. The claim is template-allocated
			// (node-local), so the second pod must land on the same NodeClaim as the first.
			podA := draPod("gpu", "shared-claim")
			podB := draPod("gpu", "shared-claim")
			provisionDRA(podA, podB)

			nodeA := ExpectScheduled(ctx, env.Client, podA)
			nodeB := ExpectScheduled(ctx, env.Client, podB)
			Expect(nodeA.Name).To(Equal(nodeB.Name))
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(1))
		})
	})

	Context("Device availability via the deviceallocation controller (V)", func() {
		It("should not reallocate a cluster-wide device held by a live pod (V2)", func() {
			// No instance type provides template GPUs, so the only GPU devices are the two cluster-wide ones below. A
			// live pod holds device-0, so a new claim must be given device-1 — never the held device.
			cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{fake.NewInstanceType("plain-it")}
			ExpectApplied(ctx, env.Client, nodePool, test.DeviceClassWithSelector("gpu", gpuDriver))
			ExpectApplied(ctx, env.Client, clusterWideSlice("shared-gpu-pool", gpuDriver, "shared-gpu-0", "shared-gpu-1"))

			// A live pod holds shared-gpu-0 via an allocated claim.
			livePod := test.Pod(test.PodOptions{ObjectMeta: metav1.ObjectMeta{Name: "live-pod"}})
			ExpectApplied(ctx, env.Client, livePod)
			ExpectApplied(ctx, env.Client, allocatedClusterWideClaim("held-claim", "shared-gpu-pool", gpuDriver, "shared-gpu-0", podConsumer(livePod)))

			newClaim := test.ResourceClaimForRequests("new-claim", test.ExactDeviceRequest("req", "gpu", 1))
			ExpectApplied(ctx, env.Client, newClaim)
			pod := draPod("gpu", "new-claim")
			provisionDRA(pod)

			ExpectScheduled(ctx, env.Client, pod)
			devices := ExpectResourceClaimAllocated(ctx, env.Client, "default", "new-claim", gpuDriver)
			Expect(devices).To(ConsistOf("shared-gpu-pool/shared-gpu-1"), "must not reallocate the device held by a live pod")
		})
		It("should reclaim a cluster-wide device whose only consumer is a deleting pod (V1)", func() {
			// A single cluster-wide GPU device, held by a pod on a deleting node. Because the consuming pod is on a
			// node being deleted, the allocator treats the device as available and the new claim reclaims it.
			cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{fake.NewInstanceType("plain-it")}
			ExpectApplied(ctx, env.Client, nodePool, test.DeviceClassWithSelector("gpu", gpuDriver))
			ExpectApplied(ctx, env.Client, clusterWideSlice("shared-gpu-pool", gpuDriver, "shared-gpu-0"))

			// Create a deleting node hosting the consuming pod, so the pod is sourced as a deleting-node pod in Schedule().
			deletingNode := existingNode("plain-it", true, corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("4Gi"), corev1.ResourcePods: resource.MustParse("10"),
			})
			consumerPod := test.Pod(test.PodOptions{ObjectMeta: metav1.ObjectMeta{Name: "consumer-pod"}})
			ExpectApplied(ctx, env.Client, consumerPod)
			ExpectManualBinding(ctx, env.Client, consumerPod, deletingNode)
			ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(deletingNode))
			ExpectApplied(ctx, env.Client, allocatedClusterWideClaim("held-claim", "shared-gpu-pool", gpuDriver, "shared-gpu-0", podConsumer(consumerPod)))

			// Mark the node for deletion; its pod becomes a deleting-node pod that the provisioner reschedules.
			Expect(env.Client.Delete(ctx, deletingNode)).To(Succeed())
			ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(deletingNode))

			newClaim := test.ResourceClaimForRequests("new-claim", test.ExactDeviceRequest("req", "gpu", 1))
			ExpectApplied(ctx, env.Client, newClaim)
			pod := draPod("gpu", "new-claim")
			provisionDRA(pod)

			// The device was reclaimable (its only consumer is on a deleting node), so the new claim takes it.
			ExpectScheduled(ctx, env.Client, pod)
			devices := ExpectResourceClaimAllocated(ctx, env.Client, "default", "new-claim", gpuDriver)
			Expect(devices).To(ConsistOf("shared-gpu-pool/shared-gpu-0"))
		})
	})

	Context("Slice sourcing / lifecycle edges (S)", func() {
		It("should exclude published slices of an uninitialized node (S1)", func() {
			cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{gpuInstanceType("gpu-it", 1)}
			ExpectApplied(ctx, env.Client, nodePool, test.DeviceClassWithSelector("gpu", gpuDriver))
			ExpectApplied(ctx, env.Client, test.ResourceClaimForRequests("gpu-claim", test.ExactDeviceRequest("req", "gpu", 1)))

			// An uninitialized node has a published slice, but it must be excluded (the node is represented by templates).
			node := existingNode("gpu-it", false, corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("4Gi"), corev1.ResourcePods: resource.MustParse("10"),
			})
			ExpectApplied(ctx, env.Client, nodeLocalSlice(node, gpuDriver, "incluster-gpu-0"))

			pod := draPod("gpu", "gpu-claim")
			provisionDRA(pod)

			// The pod is satisfied via a new NodeClaim's template device (the uninitialized node's published slice is
			// excluded), and the claim's allocated device comes from the new node's template pool, not the existing
			// node's in-cluster pool.
			scheduled := ExpectScheduled(ctx, env.Client, pod)
			Expect(scheduled.Name).ToNot(Equal(node.Name))
			devices := ExpectResourceClaimAllocated(ctx, env.Client, "default", "gpu-claim", gpuDriver)
			Expect(devices).ToNot(ConsistOf(test.NodeLocalPoolName(gpuDriver, node.Name) + "/incluster-gpu-0"))
		})
		It("should exclude slices owned by a node not in cluster state (S2)", func() {
			cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{gpuInstanceType("gpu-it", 1)}
			ExpectApplied(ctx, env.Client, nodePool, test.DeviceClassWithSelector("gpu", gpuDriver))
			ExpectApplied(ctx, env.Client, test.ResourceClaimForRequests("gpu-claim", test.ExactDeviceRequest("req", "gpu", 1)))

			// A node-owned slice whose owning node was never registered in cluster state must be dropped from the pool.
			orphanNode := test.Node(test.NodeOptions{ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{corev1.LabelHostname: "orphan-node"},
			}})
			orphanNode.Name = "orphan-node"
			ExpectApplied(ctx, env.Client, orphanNode)
			orphanNode = ExpectExists(ctx, env.Client, orphanNode)
			ExpectApplied(ctx, env.Client, nodeLocalSlice(orphanNode, gpuDriver, "orphan-gpu-0"))

			pod := draPod("gpu", "gpu-claim")
			provisionDRA(pod)

			// The orphan slice is excluded, so the pod is satisfied via a new NodeClaim's template device.
			ExpectScheduled(ctx, env.Client, pod)
			devices := ExpectResourceClaimAllocated(ctx, env.Client, "default", "gpu-claim", gpuDriver)
			Expect(devices).ToNot(ConsistOf(test.NodeLocalPoolName(gpuDriver, orphanNode.Name) + "/orphan-gpu-0"))
		})
	})

	Context("Partially-initialized nodes (I)", func() {
		It("should satisfy a pod via template devices for a pre-initialized node (I1)", func() {
			cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{gpuInstanceType("gpu-it", 1)}
			ExpectApplied(ctx, env.Client, nodePool, test.DeviceClassWithSelector("gpu", gpuDriver))
			ExpectApplied(ctx, env.Client, test.ResourceClaimForRequests("gpu-claim", test.ExactDeviceRequest("req", "gpu", 1)))

			// A pre-initialized node exists with a published slice, but since it isn't initialized its devices come from
			// templates and its published slice is excluded.
			node := existingNode("gpu-it", false, corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("4Gi"), corev1.ResourcePods: resource.MustParse("10"),
			})
			ExpectApplied(ctx, env.Client, nodeLocalSlice(node, gpuDriver, "incluster-gpu-0"))

			pod := draPod("gpu", "gpu-claim")
			provisionDRA(pod)

			// The pod schedules and its device is not the excluded published device.
			ExpectScheduled(ctx, env.Client, pod)
			devices := ExpectResourceClaimAllocated(ctx, env.Client, "default", "gpu-claim", gpuDriver)
			Expect(devices).ToNot(ConsistOf(test.NodeLocalPoolName(gpuDriver, node.Name) + "/incluster-gpu-0"))
		})
		It("should not double-count a node's devices across the uninitialized→initialized transition (I2)", func() {
			cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{gpuInstanceType("gpu-it", 1)}
			ExpectApplied(ctx, env.Client, nodePool, test.DeviceClassWithSelector("gpu", gpuDriver))

			// A pre-initialized node publishes its single GPU device in-cluster, but while uninitialized that device is
			// represented by the node's template (published slice excluded).
			node := existingNode("gpu-it", false, corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("4Gi"), corev1.ResourcePods: resource.MustParse("10"),
			})
			ExpectApplied(ctx, env.Client, nodeLocalSlice(node, gpuDriver, "incluster-gpu-0"))

			// Run 1 (uninitialized): the pod is satisfied via templates, not the existing node's published slice.
			ExpectApplied(ctx, env.Client, test.ResourceClaimForRequests("claim-a", test.ExactDeviceRequest("req", "gpu", 1)))
			podA := draPod("gpu", "claim-a")
			provisionDRA(podA)
			ExpectScheduled(ctx, env.Client, podA)
			devicesA := ExpectResourceClaimAllocated(ctx, env.Client, "default", "claim-a", gpuDriver)
			Expect(devicesA).ToNot(ConsistOf(test.NodeLocalPoolName(gpuDriver, node.Name) + "/incluster-gpu-0"))

			// Initialize the node so its published slice becomes authoritative for subsequent runs.
			ExpectMakeNodesInitialized(ctx, env.Client, env.Clock, node)
			ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))

			// Run 2 (initialized): the node's published device is now its single device. A second claim that requires the
			// node's device must not double-count — since run 1 consumed the node's only template device via a separate
			// NodeClaim, the published device is still free and can be allocated exactly once here.
			ExpectApplied(ctx, env.Client, test.ResourceClaimForRequests("claim-b", test.ExactDeviceRequest("req", "gpu", 1)))
			podB := draPod("gpu", "claim-b")
			provisionDRA(podB)
			ExpectScheduled(ctx, env.Client, podB)
			devicesB := ExpectResourceClaimAllocated(ctx, env.Client, "default", "claim-b", gpuDriver)
			Expect(devicesB).To(HaveLen(1), "the initialized node's published device is allocated exactly once")
		})
	})

	Context("Consumable capacity (P)", func() {
		BeforeEach(func() {
			if env.Version.Minor() < 36 {
				Skip("Consumable capacity requires K8s versions >= 1.36.x (DRAConsumableCapacity feature gate)")
			}
		})

		It("should provision a node and allocate a capacity slice of a multi-allocatable device (P1)", func() {
			cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{capacityGPUInstanceType("gpu-it", "16Gi")}
			ExpectApplied(ctx, env.Client, nodePool, test.DeviceClassWithSelector("gpu", gpuDriver))
			ExpectApplied(ctx, env.Client, test.ResourceClaim(resourcev1.ResourceClaim{
				ObjectMeta: metav1.ObjectMeta{Name: "gpu-claim", Namespace: "default"},
				Spec: resourcev1.ResourceClaimSpec{Devices: resourcev1.DeviceClaim{Requests: []resourcev1.DeviceRequest{
					test.ExactDeviceRequestWithCapacity("req", "gpu", 1, capacity("4Gi")),
				}}},
			}))

			pod := draPod("gpu", "gpu-claim")
			provisionDRA(pod)

			ExpectScheduled(ctx, env.Client, pod)
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(1))
			ExpectResourceClaimAllocated(ctx, env.Client, "default", "gpu-claim", gpuDriver)
		})

		It("should pack multiple pods onto one multi-allocatable device via capacity (P2)", func() {
			// A single multi-allocatable device with 16Gi; three pods each consume 4Gi (12Gi <= 16Gi), so all three
			// share the one device on a single NodeClaim — the core consumable-capacity packing win.
			cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{capacityGPUInstanceType("gpu-it", "16Gi")}
			ExpectApplied(ctx, env.Client, nodePool, test.DeviceClassWithSelector("gpu", gpuDriver))
			pods := lo.Times(3, func(i int) *corev1.Pod {
				name := fmt.Sprintf("cap-claim-%d", i)
				ExpectApplied(ctx, env.Client, test.ResourceClaim(resourcev1.ResourceClaim{
					ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
					Spec: resourcev1.ResourceClaimSpec{Devices: resourcev1.DeviceClaim{Requests: []resourcev1.DeviceRequest{
						test.ExactDeviceRequestWithCapacity("req", "gpu", 1, capacity("4Gi")),
					}}},
				}))
				return draPod("gpu", name)
			})
			provisionDRA(pods...)

			nodeNames := sets.New[string]()
			for _, pod := range pods {
				nodeNames.Insert(ExpectScheduled(ctx, env.Client, pod).Name)
			}
			Expect(nodeNames.Len()).To(Equal(1), "all three capacity slices fit on one shared device on one node")
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(1))
		})

		It("should create a second NodeClaim when device capacity is exhausted (P3)", func() {
			// 16Gi device per node. Three 4Gi pods fit (12Gi); a fourth 8Gi pod (20Gi total) cannot share, forcing a
			// second node.
			cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{capacityGPUInstanceType("gpu-it", "16Gi")}
			ExpectApplied(ctx, env.Client, nodePool, test.DeviceClassWithSelector("gpu", gpuDriver))
			var pods []*corev1.Pod
			for i, amount := range []string{"4Gi", "4Gi", "4Gi", "8Gi"} {
				name := fmt.Sprintf("cap-claim-%d", i)
				ExpectApplied(ctx, env.Client, test.ResourceClaim(resourcev1.ResourceClaim{
					ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
					Spec: resourcev1.ResourceClaimSpec{Devices: resourcev1.DeviceClaim{Requests: []resourcev1.DeviceRequest{
						test.ExactDeviceRequestWithCapacity("req", "gpu", 1, capacity(amount)),
					}}},
				}))
				pods = append(pods, draPod("gpu", name))
			}
			provisionDRA(pods...)

			nodeNames := sets.New[string]()
			for _, pod := range pods {
				nodeNames.Insert(ExpectScheduled(ctx, env.Client, pod).Name)
			}
			Expect(nodeNames.Len()).To(Equal(2), "20Gi of requests cannot fit on a single 16Gi device")
		})

		It("should reclaim a deleting pod's capacity share of a shared device (P4)", func() {
			// A cluster-wide multi-allocatable device with 16Gi, partially consumed: a live pod holds 4Gi and a pod on a
			// deleting node holds 10Gi. Only 2Gi is nominally free, but the deleting pod's 10Gi must be reclaimed, so a
			// new 10Gi claim fits on the existing device (exercises B2 partial subtraction end to end).
			cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{fake.NewInstanceType("plain-it")}
			ExpectApplied(ctx, env.Client, nodePool, test.DeviceClassWithSelector("gpu", gpuDriver))
			ExpectApplied(ctx, env.Client, sharedCapacitySlice("shared-gpu-pool", gpuDriver, "shared-gpu-0", "16Gi"))

			// A live pod holds 4Gi of the shared device — this share must NOT be reclaimed.
			livePod := test.Pod(test.PodOptions{ObjectMeta: metav1.ObjectMeta{Name: "live-pod"}})
			ExpectApplied(ctx, env.Client, livePod)
			ExpectApplied(ctx, env.Client, allocatedSharedClaim("live-claim", "shared-gpu-pool", gpuDriver, "shared-gpu-0", capacity("4Gi"), podConsumer(livePod)))

			// A pod on a deleting node holds 10Gi — its share must be reclaimed when the node is deleted.
			deletingNode := existingNode("plain-it", true, corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("4Gi"), corev1.ResourcePods: resource.MustParse("10"),
			})
			deletingPod := test.Pod(test.PodOptions{ObjectMeta: metav1.ObjectMeta{Name: "deleting-pod"}})
			ExpectApplied(ctx, env.Client, deletingPod)
			ExpectManualBinding(ctx, env.Client, deletingPod, deletingNode)
			ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(deletingNode))
			ExpectApplied(ctx, env.Client, allocatedSharedClaim("deleting-claim", "shared-gpu-pool", gpuDriver, "shared-gpu-0", capacity("10Gi"), podConsumer(deletingPod)))

			// Mark the node for deletion so its pod's 10Gi share is freed.
			Expect(env.Client.Delete(ctx, deletingNode)).To(Succeed())
			ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(deletingNode))

			// A new 10Gi claim only fits if the deleting pod's share was reclaimed (16 - 4 live = 12Gi available).
			ExpectApplied(ctx, env.Client, test.ResourceClaim(resourcev1.ResourceClaim{
				ObjectMeta: metav1.ObjectMeta{Name: "new-claim", Namespace: "default"},
				Spec: resourcev1.ResourceClaimSpec{Devices: resourcev1.DeviceClaim{Requests: []resourcev1.DeviceRequest{
					test.ExactDeviceRequestWithCapacity("req", "gpu", 1, capacity("10Gi")),
				}}},
			}))
			pod := draPod("gpu", "new-claim")
			provisionDRA(pod)

			// plain-it provides no template GPUs, so the cluster-wide shared device is the only thing that can satisfy the
			// 10Gi claim — and only if the deleting pod's 10Gi share was reclaimed (16 - 4 live = 12Gi available). The
			// allocation to shared-gpu-0 is the proof of partial reclaim; the pod runs on a fresh node that the AllNodes
			// device attaches to.
			ExpectScheduled(ctx, env.Client, pod)
			devices := ExpectResourceClaimAllocated(ctx, env.Client, "default", "new-claim", gpuDriver)
			Expect(devices).To(ConsistOf("shared-gpu-pool/shared-gpu-0"))
		})

		It("should fail to schedule when a single request exceeds total device capacity (P6)", func() {
			cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{capacityGPUInstanceType("gpu-it", "16Gi")}
			ExpectApplied(ctx, env.Client, nodePool, test.DeviceClassWithSelector("gpu", gpuDriver))
			ExpectApplied(ctx, env.Client, test.ResourceClaim(resourcev1.ResourceClaim{
				ObjectMeta: metav1.ObjectMeta{Name: "gpu-claim", Namespace: "default"},
				Spec: resourcev1.ResourceClaimSpec{Devices: resourcev1.DeviceClaim{Requests: []resourcev1.DeviceRequest{
					test.ExactDeviceRequestWithCapacity("req", "gpu", 1, capacity("32Gi")),
				}}},
			}))

			pod := draPod("gpu", "gpu-claim")
			provisionDRA(pod)
			ExpectNotScheduled(ctx, env.Client, pod)
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(0))
		})

		It("should fail to schedule when a requested capacity dimension is not offered (P8)", func() {
			// The device offers only "memory" capacity; a request for "bandwidth" can't be satisfied by any device.
			cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{capacityGPUInstanceType("gpu-it", "16Gi")}
			ExpectApplied(ctx, env.Client, nodePool, test.DeviceClassWithSelector("gpu", gpuDriver))
			ExpectApplied(ctx, env.Client, test.ResourceClaim(resourcev1.ResourceClaim{
				ObjectMeta: metav1.ObjectMeta{Name: "gpu-claim", Namespace: "default"},
				Spec: resourcev1.ResourceClaimSpec{Devices: resourcev1.DeviceClaim{Requests: []resourcev1.DeviceRequest{
					test.ExactDeviceRequestWithCapacity("req", "gpu", 1, map[resourcev1.QualifiedName]resource.Quantity{
						capacityMemory: resource.MustParse("4Gi"),
						"bandwidth":    resource.MustParse("10"),
					}),
				}}},
			}))

			pod := draPod("gpu", "gpu-claim")
			provisionDRA(pod)
			ExpectNotScheduled(ctx, env.Client, pod)
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(0))
		})

		It("should reject a request that violates a ValidValues RequestPolicy (P9)", func() {
			// Device capacity restricted to {4Gi,8Gi,16Gi}; a 32Gi request exceeds all valid values and is rejected.
			cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{capacityPolicyGPUInstanceType("gpu-it", "16Gi",
				&resourcev1.CapacityRequestPolicy{
					Default:     lo.ToPtr(resource.MustParse("4Gi")),
					ValidValues: []resource.Quantity{resource.MustParse("4Gi"), resource.MustParse("8Gi"), resource.MustParse("16Gi")},
				})}
			ExpectApplied(ctx, env.Client, nodePool, test.DeviceClassWithSelector("gpu", gpuDriver))
			ExpectApplied(ctx, env.Client, test.ResourceClaim(resourcev1.ResourceClaim{
				ObjectMeta: metav1.ObjectMeta{Name: "gpu-claim", Namespace: "default"},
				Spec: resourcev1.ResourceClaimSpec{Devices: resourcev1.DeviceClaim{Requests: []resourcev1.DeviceRequest{
					test.ExactDeviceRequestWithCapacity("req", "gpu", 1, capacity("32Gi")),
				}}},
			}))

			pod := draPod("gpu", "gpu-claim")
			provisionDRA(pod)
			ExpectNotScheduled(ctx, env.Client, pod)
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(0))
		})

		It("should apply RequestPolicy.Default for an unspecified dimension, enabling packing (P10)", func() {
			// Device has 16Gi with a Default consumption of 4Gi. Three pods that request the device WITHOUT naming the
			// capacity dimension each consume the 4Gi default (not the whole device), so all three pack onto one device.
			cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{capacityPolicyGPUInstanceType("gpu-it", "16Gi",
				&resourcev1.CapacityRequestPolicy{Default: lo.ToPtr(resource.MustParse("4Gi"))})}
			ExpectApplied(ctx, env.Client, nodePool, test.DeviceClassWithSelector("gpu", gpuDriver))
			pods := lo.Times(3, func(i int) *corev1.Pod {
				name := fmt.Sprintf("default-claim-%d", i)
				// No capacity request — the device's RequestPolicy.Default (4Gi) governs consumption.
				ExpectApplied(ctx, env.Client, test.ResourceClaimForRequests(name, test.ExactDeviceRequest("req", "gpu", 1)))
				return draPod("gpu", name)
			})
			provisionDRA(pods...)

			nodeNames := sets.New[string]()
			for _, pod := range pods {
				nodeNames.Insert(ExpectScheduled(ctx, env.Client, pod).Name)
			}
			Expect(nodeNames.Len()).To(Equal(1), "default 4Gi consumption lets three pods share one 16Gi device")
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(1))
		})
	})

	Context("Partitionable devices (K)", func() {
		BeforeEach(func() {
			if env.Version.Minor() < 36 {
				Skip("Partitionable devices require K8s versions >= 1.36.x (DRAConsumableCapacity feature gate)")
			}
		})

		It("should pack counter-constrained profiles onto one NodeClaim within budget (K1)", func() {
			// Budget memory:80Gi; two profiles each consume 40Gi → both fit on one node's pool.
			cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{
				partitionableGPUInstanceType("gpu-it", quantities("memory", "80Gi"), 2, quantities("memory", "40Gi")),
			}
			ExpectApplied(ctx, env.Client, nodePool, test.DeviceClassWithSelector("gpu", gpuDriver))
			ExpectApplied(ctx, env.Client,
				test.ResourceClaimForRequests("claim-a", test.ExactDeviceRequest("req", "gpu", 1)),
				test.ResourceClaimForRequests("claim-b", test.ExactDeviceRequest("req", "gpu", 1)),
			)
			podA := draPod("gpu", "claim-a")
			podB := draPod("gpu", "claim-b")
			provisionDRA(podA, podB)

			nodeA := ExpectScheduled(ctx, env.Client, podA)
			nodeB := ExpectScheduled(ctx, env.Client, podB)
			Expect(nodeA.Name).To(Equal(nodeB.Name), "both 40Gi profiles fit within the 80Gi counter budget on one node")
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(1))
		})

		It("should create a second NodeClaim when the counter budget is exhausted (K2)", func() {
			// Budget 80Gi; three 40Gi profiles (120Gi) cannot all draw from one pool → a second node is required.
			cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{
				partitionableGPUInstanceType("gpu-it", quantities("memory", "80Gi"), 3, quantities("memory", "40Gi")),
			}
			ExpectApplied(ctx, env.Client, nodePool, test.DeviceClassWithSelector("gpu", gpuDriver))
			pods := lo.Times(3, func(i int) *corev1.Pod {
				name := fmt.Sprintf("claim-%d", i)
				ExpectApplied(ctx, env.Client, test.ResourceClaimForRequests(name, test.ExactDeviceRequest("req", "gpu", 1)))
				return draPod("gpu", name)
			})
			provisionDRA(pods...)

			nodeNames := sets.New[string]()
			for _, pod := range pods {
				nodeNames.Insert(ExpectScheduled(ctx, env.Client, pod).Name)
			}
			Expect(nodeNames.Len()).To(Equal(2), "120Gi of profiles cannot fit in a single 80Gi counter budget")
		})

		It("should track multiple counter dimensions independently (K3)", func() {
			// Budget memory:80Gi, compute:100. Two profiles consume memory:40Gi + compute:50 each → both fit.
			cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{
				partitionableGPUInstanceType("gpu-it", quantities("memory", "80Gi", "compute", "100"), 2, quantities("memory", "40Gi", "compute", "50")),
			}
			ExpectApplied(ctx, env.Client, nodePool, test.DeviceClassWithSelector("gpu", gpuDriver))
			ExpectApplied(ctx, env.Client,
				test.ResourceClaimForRequests("claim-a", test.ExactDeviceRequest("req", "gpu", 1)),
				test.ResourceClaimForRequests("claim-b", test.ExactDeviceRequest("req", "gpu", 1)),
			)
			podA := draPod("gpu", "claim-a")
			podB := draPod("gpu", "claim-b")
			provisionDRA(podA, podB)

			nodeA := ExpectScheduled(ctx, env.Client, podA)
			nodeB := ExpectScheduled(ctx, env.Client, podB)
			Expect(nodeA.Name).To(Equal(nodeB.Name), "both profiles fit within memory and compute budgets on one node")
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(1))
		})

		It("should fail to schedule when a single profile exceeds the counter budget (K5)", func() {
			// Budget memory:40Gi; the single profile consumes 80Gi — over budget, so no device is allocatable.
			cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{
				partitionableGPUInstanceType("gpu-it", quantities("memory", "40Gi"), 1, quantities("memory", "80Gi")),
			}
			ExpectApplied(ctx, env.Client, nodePool, test.DeviceClassWithSelector("gpu", gpuDriver))
			ExpectApplied(ctx, env.Client, test.ResourceClaimForRequests("gpu-claim", test.ExactDeviceRequest("req", "gpu", 1)))

			pod := draPod("gpu", "gpu-claim")
			provisionDRA(pod)
			ExpectNotScheduled(ctx, env.Client, pod)
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(0))
		})

		It("should reject a pool whose device references a non-existent counter set (K6)", func() {
			// The device consumes from a counter set name that the pool's SharedCounters doesn't define. The allocator
			// marks the pool invalid (getAndValidateCounterSets / validateDeviceCounterConsumption), so no device is
			// allocatable and the pod can't schedule.
			cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{
				fake.NewInstanceType("gpu-it", fake.WithResourceSliceTemplates(
					fake.ResourceSliceTemplateWithCounters(gpuDriver, "gpu-it-pool", fake.CounterSet(counterSetName, quantities("memory", "80Gi"))),
					fake.ResourceSliceTemplate(gpuDriver, "gpu-it-pool",
						fake.DeviceWith("gpu-it-profile-0",
							fake.WithConsumesCounters(fake.CounterConsumption("nonexistent-counter-set", quantities("memory", "40Gi"))),
						),
					),
				)),
			}
			ExpectApplied(ctx, env.Client, nodePool, test.DeviceClassWithSelector("gpu", gpuDriver))
			ExpectApplied(ctx, env.Client, test.ResourceClaimForRequests("gpu-claim", test.ExactDeviceRequest("req", "gpu", 1)))

			pod := draPod("gpu", "gpu-claim")
			provisionDRA(pod)
			ExpectNotScheduled(ctx, env.Client, pod)
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(0))
		})

		It("should never co-locate profiles whose combined consumption exceeds a dimension budget (K7)", func() {
			// Budget compute:100; two profiles each consume compute:60 (120 > 100). They must land on separate nodes.
			cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{
				partitionableGPUInstanceType("gpu-it", quantities("compute", "100"), 2, quantities("compute", "60")),
			}
			ExpectApplied(ctx, env.Client, nodePool, test.DeviceClassWithSelector("gpu", gpuDriver))
			ExpectApplied(ctx, env.Client,
				test.ResourceClaimForRequests("claim-a", test.ExactDeviceRequest("req", "gpu", 1)),
				test.ResourceClaimForRequests("claim-b", test.ExactDeviceRequest("req", "gpu", 1)),
			)
			podA := draPod("gpu", "claim-a")
			podB := draPod("gpu", "claim-b")
			provisionDRA(podA, podB)

			nodeA := ExpectScheduled(ctx, env.Client, podA)
			nodeB := ExpectScheduled(ctx, env.Client, podB)
			Expect(nodeA.Name).ToNot(Equal(nodeB.Name), "60+60 compute exceeds the 100 budget, so the profiles can't share a pool")
			Expect(ExpectNodeClaims(ctx, env.Client)).To(HaveLen(2))
		})
	})
})
