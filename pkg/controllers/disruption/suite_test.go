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
	"context"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/sets"
	clock "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"sigs.k8s.io/karpenter/pkg/utils/pdb"
	. "sigs.k8s.io/karpenter/pkg/utils/testing"

	coreapis "sigs.k8s.io/karpenter/pkg/apis"
	"sigs.k8s.io/karpenter/pkg/apis/v1beta1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
	"sigs.k8s.io/karpenter/pkg/controllers/disruption"
	"sigs.k8s.io/karpenter/pkg/controllers/disruption/orchestration"
	"sigs.k8s.io/karpenter/pkg/controllers/provisioning"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/controllers/state/informer"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/scheduling"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
	disruptionutils "sigs.k8s.io/karpenter/pkg/utils/disruption"
)

var ctx context.Context
var env *test.Environment
var cluster *state.Cluster
var disruptionController *disruption.Controller
var prov *provisioning.Provisioner
var cloudProvider *fake.CloudProvider
var nodeStateController *informer.NodeController
var nodeClaimStateController *informer.NodeClaimController
var fakeClock *clock.FakeClock
var recorder *test.EventRecorder
var queue *orchestration.Queue

var onDemandInstances []*cloudprovider.InstanceType
var spotInstances []*cloudprovider.InstanceType
var leastExpensiveInstance, mostExpensiveInstance *cloudprovider.InstanceType
var leastExpensiveOffering, mostExpensiveOffering cloudprovider.Offering
var leastExpensiveSpotInstance, mostExpensiveSpotInstance *cloudprovider.InstanceType
var leastExpensiveSpotOffering, mostExpensiveSpotOffering cloudprovider.Offering

func TestAPIs(t *testing.T) {
	ctx = TestContextWithLogger(t)
	RegisterFailHandler(Fail)
	RunSpecs(t, "Disruption")
}

var _ = BeforeSuite(func() {
	env = test.NewEnvironment(test.WithCRDs(coreapis.CRDs...))
	ctx = options.ToContext(ctx, test.Options())
	cloudProvider = fake.NewCloudProvider()
	fakeClock = clock.NewFakeClock(time.Now())
	cluster = state.NewCluster(fakeClock, env.Client, cloudProvider)
	nodeStateController = informer.NewNodeController(env.Client, cluster)
	nodeClaimStateController = informer.NewNodeClaimController(env.Client, cluster)
	recorder = test.NewEventRecorder()
	prov = provisioning.NewProvisioner(env.Client, recorder, cloudProvider, cluster)
	queue = orchestration.NewTestingQueue(env.Client, recorder, cluster, fakeClock, prov)
	disruptionController = disruption.NewController(fakeClock, env.Client, prov, cloudProvider, recorder, cluster, queue)
})

var _ = AfterSuite(func() {
	Expect(env.Stop()).To(Succeed(), "Failed to stop environment")
})

var _ = BeforeEach(func() {
	cloudProvider.Reset()
	cloudProvider.InstanceTypes = fake.InstanceTypesAssorted()

	recorder.Reset() // Reset the events that we captured during the run

	// ensure any waiters on our clock are allowed to proceed before resetting our clock time
	for fakeClock.HasWaiters() {
		fakeClock.Step(1 * time.Minute)
	}
	fakeClock.SetTime(time.Now())
	cluster.Reset()
	queue.Reset()
	cluster.MarkUnconsolidated()

	// Reset Feature Flags to test defaults
	ctx = options.ToContext(ctx, test.Options())

	onDemandInstances = lo.Filter(cloudProvider.InstanceTypes, func(i *cloudprovider.InstanceType, _ int) bool {
		for _, o := range i.Offerings.Available() {
			if o.Requirements.Get(v1beta1.CapacityTypeLabelKey).Any() == v1beta1.CapacityTypeOnDemand {
				return true
			}
		}
		return false
	})
	// Sort the on-demand instances by pricing from low to high
	sort.Slice(onDemandInstances, func(i, j int) bool {
		return onDemandInstances[i].Offerings.Cheapest().Price < onDemandInstances[j].Offerings.Cheapest().Price
	})
	leastExpensiveInstance, mostExpensiveInstance = onDemandInstances[0], onDemandInstances[len(onDemandInstances)-1]
	leastExpensiveOffering, mostExpensiveOffering = leastExpensiveInstance.Offerings[0], mostExpensiveInstance.Offerings[0]
	spotInstances = lo.Filter(cloudProvider.InstanceTypes, func(i *cloudprovider.InstanceType, _ int) bool {
		for _, o := range i.Offerings.Available() {
			if o.Requirements.Get(v1beta1.CapacityTypeLabelKey).Any() == v1beta1.CapacityTypeSpot {
				return true
			}
		}
		return false
	})
	// Sort the spot instances by pricing from low to high
	sort.Slice(spotInstances, func(i, j int) bool {
		return spotInstances[i].Offerings.Cheapest().Price < spotInstances[j].Offerings.Cheapest().Price
	})
	leastExpensiveSpotInstance, mostExpensiveSpotInstance = spotInstances[0], spotInstances[len(spotInstances)-1]
	leastExpensiveSpotOffering, mostExpensiveSpotOffering = leastExpensiveSpotInstance.Offerings[0], mostExpensiveSpotInstance.Offerings[0]
})

var _ = AfterEach(func() {
	ExpectCleanedUp(ctx, env.Client)

	// Reset the metrics collectors
	disruption.ActionsPerformedCounter.Reset()
	disruption.NodesDisruptedCounter.Reset()
	disruption.PodsDisruptedCounter.Reset()
})

var _ = Describe("Simulate Scheduling", func() {
	var nodePool *v1beta1.NodePool
	BeforeEach(func() {
		nodePool = test.NodePool()
	})
	It("should allow pods on deleting nodes to reschedule to uninitialized nodes", func() {
		numNodes := 10
		nodeClaims, nodes := test.NodeClaimsAndNodes(numNodes, v1beta1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Finalizers: []string{"karpenter.sh/test-finalizer"},
				Labels: map[string]string{
					v1beta1.NodePoolLabelKey:     nodePool.Name,
					v1.LabelInstanceTypeStable:   mostExpensiveInstance.Name,
					v1beta1.CapacityTypeLabelKey: mostExpensiveOffering.Requirements.Get(v1beta1.CapacityTypeLabelKey).Any(),
					v1.LabelTopologyZone:         mostExpensiveOffering.Requirements.Get(v1.LabelTopologyZone).Any(),
				},
			},
			Status: v1beta1.NodeClaimStatus{
				Allocatable: map[v1.ResourceName]resource.Quantity{
					v1.ResourceCPU:  resource.MustParse("3"),
					v1.ResourcePods: resource.MustParse("100"),
				},
			},
		})
		ExpectApplied(ctx, env.Client, nodePool)

		for i := 0; i < numNodes; i++ {
			ExpectApplied(ctx, env.Client, nodeClaims[i], nodes[i])
		}

		// inform cluster state about nodes and nodeclaims
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

		pod := test.Pod(test.PodOptions{
			ResourceRequirements: v1.ResourceRequirements{
				Requests: v1.ResourceList{
					// 2 cpu so each node can only fit one pod.
					v1.ResourceCPU:    resource.MustParse("2"),
					v1.ResourceMemory: resource.MustParse("100Mi"),
				},
			},
		})
		nodePool.Spec.Disruption.ConsolidateAfter = &v1beta1.NillableDuration{Duration: nil}
		ExpectApplied(ctx, env.Client, pod)
		ExpectManualBinding(ctx, env.Client, pod, nodes[0])

		// nodePool.Spec.Disruption.Budgets = []v1beta1.Budget{{Nodes: "100%"}}
		// ExpectApplied(ctx, env.Client, nodePool)

		nodePoolMap, nodePoolToInstanceTypesMap, err := disruption.BuildNodePoolMap(ctx, env.Client, cloudProvider)
		Expect(err).To(Succeed())

		// Mark all nodeclaims as marked for deletion
		for i, nc := range nodeClaims {
			ExpectReconcileSucceeded(ctx, nodeClaimStateController, client.ObjectKeyFromObject(nc))
			cluster.MarkForDeletion(nodeClaims[i].Status.ProviderID)
		}
		cluster.UnmarkForDeletion(nodeClaims[0].Status.ProviderID)
		// Mark all nodes as marked for deletion
		for _, n := range nodes {
			ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(n))
		}

		pdbs, err := pdb.NewLimits(ctx, fakeClock, env.Client)
		Expect(err).To(Succeed())

		// Generate a candidate
		stateNode := ExpectStateNodeExists(cluster, nodes[0])
		candidate, err := disruption.NewCandidate(ctx, env.Client, recorder, fakeClock, stateNode, pdbs, nodePoolMap, nodePoolToInstanceTypesMap, queue)
		Expect(err).To(Succeed())

		results, err := disruption.SimulateScheduling(ctx, env.Client, cluster, prov, candidate)
		Expect(err).To(Succeed())
		Expect(results.PodErrors[pod]).To(BeNil())
	})
	It("should allow multiple replace operations to happen successively", func() {
		numNodes := 10
		nodeClaims, nodes := test.NodeClaimsAndNodes(numNodes, v1beta1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Finalizers: []string{"karpenter.sh/test-finalizer"},
				Labels: map[string]string{
					v1beta1.NodePoolLabelKey:     nodePool.Name,
					v1.LabelInstanceTypeStable:   mostExpensiveInstance.Name,
					v1beta1.CapacityTypeLabelKey: mostExpensiveOffering.Requirements.Get(v1beta1.CapacityTypeLabelKey).Any(),
					v1.LabelTopologyZone:         mostExpensiveOffering.Requirements.Get(v1.LabelTopologyZone).Any(),
				},
			},
			Status: v1beta1.NodeClaimStatus{
				Allocatable: map[v1.ResourceName]resource.Quantity{
					v1.ResourceCPU:  resource.MustParse("3"),
					v1.ResourcePods: resource.MustParse("100"),
				},
			},
		})
		ExpectApplied(ctx, env.Client, nodePool)

		for i := 0; i < numNodes; i++ {
			ExpectApplied(ctx, env.Client, nodeClaims[i], nodes[i])
		}

		// inform cluster state about nodes and nodeclaims
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

		// Create a pod for each node
		pods := test.Pods(10, test.PodOptions{
			ResourceRequirements: v1.ResourceRequirements{
				Requests: v1.ResourceList{
					v1.ResourceCPU:    resource.MustParse("100m"),
					v1.ResourceMemory: resource.MustParse("100Mi"),
				},
			},
		})
		// Set a partition so that each node pool fits one node
		nodePool.Spec.Template.Spec.Requirements = append(nodePool.Spec.Template.Spec.Requirements, v1beta1.NodeSelectorRequirementWithMinValues{
			NodeSelectorRequirement: v1.NodeSelectorRequirement{
				Key:      "test-partition",
				Operator: v1.NodeSelectorOpExists,
			},
		})
		nodePool.Spec.Disruption.ExpireAfter = v1beta1.NillableDuration{Duration: lo.ToPtr(5 * time.Minute)}
		nodePool.Spec.Disruption.ConsolidateAfter = &v1beta1.NillableDuration{Duration: nil}
		nodePool.Spec.Disruption.Budgets = []v1beta1.Budget{{Nodes: "3"}}
		ExpectApplied(ctx, env.Client, nodePool)

		// Mark all nodeclaims as drifted
		for _, nc := range nodeClaims {
			nc.StatusConditions().SetTrue(v1beta1.ConditionTypeDrifted)
			ExpectApplied(ctx, env.Client, nc)
			ExpectReconcileSucceeded(ctx, nodeClaimStateController, client.ObjectKeyFromObject(nc))
		}
		// Add a partition label into each node so we have 10 distinct scheduling requiments for each pod/node pair
		for i, n := range nodes {
			n.Labels = lo.Assign(n.Labels, map[string]string{"test-partition": fmt.Sprintf("%d", i)})
			ExpectApplied(ctx, env.Client, n)
			ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(n))
		}

		for i := range pods {
			pods[i].Spec.NodeSelector = lo.Assign(pods[i].Spec.NodeSelector, map[string]string{"test-partition": fmt.Sprintf("%d", i)})
			ExpectApplied(ctx, env.Client, pods[i])
			ExpectManualBinding(ctx, env.Client, pods[i], nodes[i])
		}

		// Get a set of the node claim names so that it's easy to check if a new one is made
		nodeClaimNames := lo.SliceToMap(nodeClaims, func(nc *v1beta1.NodeClaim) (string, struct{}) {
			return nc.Name, struct{}{}
		})

		wg := sync.WaitGroup{}
		ExpectTriggerVerifyAction(&wg)
		ExpectSingletonReconciled(ctx, disruptionController)
		wg.Wait()

		// Expect a replace action
		ExpectTaintedNodeCount(ctx, env.Client, 1)
		ncs := ExpectNodeClaims(ctx, env.Client)
		// which would create one more node claim
		Expect(len(ncs)).To(Equal(11))
		nc, new := lo.Find(ncs, func(nc *v1beta1.NodeClaim) bool {
			_, ok := nodeClaimNames[nc.Name]
			return !ok
		})
		Expect(new).To(BeTrue())
		// which needs to be deployed
		ExpectNodeClaimDeployedAndStateUpdated(ctx, env.Client, cluster, cloudProvider, nc)
		nodeClaimNames[nc.Name] = struct{}{}

		ExpectTriggerVerifyAction(&wg)
		ExpectSingletonReconciled(ctx, disruptionController)
		wg.Wait()

		// Another replacement disruption action
		ncs = ExpectNodeClaims(ctx, env.Client)
		Expect(len(ncs)).To(Equal(12))
		nc, new = lo.Find(ncs, func(nc *v1beta1.NodeClaim) bool {
			_, ok := nodeClaimNames[nc.Name]
			return !ok
		})
		Expect(new).To(BeTrue())
		ExpectNodeClaimDeployedAndStateUpdated(ctx, env.Client, cluster, cloudProvider, nc)
		nodeClaimNames[nc.Name] = struct{}{}

		ExpectTriggerVerifyAction(&wg)
		ExpectSingletonReconciled(ctx, disruptionController)
		wg.Wait()

		// One more replacement disruption action
		ncs = ExpectNodeClaims(ctx, env.Client)
		Expect(len(ncs)).To(Equal(13))
		nc, new = lo.Find(ncs, func(nc *v1beta1.NodeClaim) bool {
			_, ok := nodeClaimNames[nc.Name]
			return !ok
		})
		Expect(new).To(BeTrue())
		ExpectNodeClaimDeployedAndStateUpdated(ctx, env.Client, cluster, cloudProvider, nc)
		nodeClaimNames[nc.Name] = struct{}{}

		// Try one more time, but fail since the budgets only allow 3 disruptions.
		ExpectTriggerVerifyAction(&wg)
		ExpectSingletonReconciled(ctx, disruptionController)
		wg.Wait()

		ncs = ExpectNodeClaims(ctx, env.Client)
		Expect(len(ncs)).To(Equal(13))
	})
	It("can replace node with a local PV (ignoring hostname affinity)", func() {
		nodeClaim, node := test.NodeClaimAndNode(v1beta1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1beta1.NodePoolLabelKey:     nodePool.Name,
					v1.LabelInstanceTypeStable:   mostExpensiveInstance.Name,
					v1beta1.CapacityTypeLabelKey: mostExpensiveOffering.Requirements.Get(v1beta1.CapacityTypeLabelKey).Any(),
					v1.LabelTopologyZone:         mostExpensiveOffering.Requirements.Get(v1.LabelTopologyZone).Any(),
				},
			},
			Status: v1beta1.NodeClaimStatus{
				Allocatable: map[v1.ResourceName]resource.Quantity{
					v1.ResourceCPU:  resource.MustParse("32"),
					v1.ResourcePods: resource.MustParse("100"),
				},
			},
		})
		labels := map[string]string{
			"app": "test",
		}
		// create our RS so we can link a pod to it
		ss := test.StatefulSet()
		ExpectApplied(ctx, env.Client, ss)

		// StorageClass that references "no-provisioner" and is used for local volume storage
		storageClass := test.StorageClass(test.StorageClassOptions{
			ObjectMeta: metav1.ObjectMeta{
				Name: "local-path",
			},
			Provisioner: lo.ToPtr("kubernetes.io/no-provisioner"),
		})
		persistentVolume := test.PersistentVolume(test.PersistentVolumeOptions{UseLocal: true})
		persistentVolume.Spec.NodeAffinity = &v1.VolumeNodeAffinity{
			Required: &v1.NodeSelector{
				NodeSelectorTerms: []v1.NodeSelectorTerm{
					{
						// This PV is only valid for use against this node
						MatchExpressions: []v1.NodeSelectorRequirement{
							{
								Key:      v1.LabelHostname,
								Operator: v1.NodeSelectorOpIn,
								Values:   []string{node.Name},
							},
						},
					},
				},
			},
		}
		persistentVolumeClaim := test.PersistentVolumeClaim(test.PersistentVolumeClaimOptions{VolumeName: persistentVolume.Name, StorageClassName: &storageClass.Name})
		pod := test.Pod(test.PodOptions{
			ObjectMeta: metav1.ObjectMeta{Labels: labels,
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion:         "apps/v1",
						Kind:               "StatefulSet",
						Name:               ss.Name,
						UID:                ss.UID,
						Controller:         lo.ToPtr(true),
						BlockOwnerDeletion: lo.ToPtr(true),
					},
				},
			},
			PersistentVolumeClaims: []string{persistentVolumeClaim.Name},
		})
		ExpectApplied(ctx, env.Client, ss, pod, nodeClaim, node, nodePool, storageClass, persistentVolume, persistentVolumeClaim)

		// bind pods to node
		ExpectManualBinding(ctx, env.Client, pod, node)

		// inform cluster state about nodes and nodeclaims
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node}, []*v1beta1.NodeClaim{nodeClaim})

		// disruption won't delete the old node until the new node is ready
		var wg sync.WaitGroup
		ExpectTriggerVerifyAction(&wg)
		ExpectMakeNewNodeClaimsReady(ctx, env.Client, &wg, cluster, cloudProvider, 1)
		ExpectSingletonReconciled(ctx, disruptionController)
		wg.Wait()

		// Process the item so that the nodes can be deleted.
		ExpectSingletonReconciled(ctx, queue)
		// Cascade any deletion of the nodeClaim to the node
		ExpectNodeClaimsCascadeDeletion(ctx, env.Client, nodeClaim)

		// Expect that the new nodeClaim was created, and it's different than the original
		// We should succeed in getting a replacement, since we assume that the node affinity requirement will be invalid
		// once we spin-down the old node
		ExpectNotFound(ctx, env.Client, nodeClaim, node)
		nodeclaims := ExpectNodeClaims(ctx, env.Client)
		nodes := ExpectNodes(ctx, env.Client)
		Expect(nodeclaims).To(HaveLen(1))
		Expect(nodes).To(HaveLen(1))
		Expect(nodeclaims[0].Name).ToNot(Equal(nodeClaim.Name))
		Expect(nodes[0].Name).ToNot(Equal(node.Name))
	})
})

var _ = Describe("Disruption Taints", func() {
	var nodePool *v1beta1.NodePool
	var nodeClaim *v1beta1.NodeClaim
	var node *v1.Node
	BeforeEach(func() {
		currentInstance := fake.NewInstanceType(fake.InstanceTypeOptions{
			Name: "current-on-demand",
			Offerings: []cloudprovider.Offering{
				{
					Requirements: scheduling.NewLabelRequirements(map[string]string{v1beta1.CapacityTypeLabelKey: v1beta1.CapacityTypeOnDemand, v1.LabelTopologyZone: "test-zone-1a"}),
					Price:        1.5,
					Available:    false,
				},
			},
		})
		replacementInstance := fake.NewInstanceType(fake.InstanceTypeOptions{
			Name: "spot-replacement",
			Offerings: []cloudprovider.Offering{
				{
					Requirements: scheduling.NewLabelRequirements(map[string]string{v1beta1.CapacityTypeLabelKey: v1beta1.CapacityTypeSpot, v1.LabelTopologyZone: "test-zone-1a"}),
					Price:        1.0,
					Available:    true,
				},
				{
					Requirements: scheduling.NewLabelRequirements(map[string]string{v1beta1.CapacityTypeLabelKey: v1beta1.CapacityTypeSpot, v1.LabelTopologyZone: "test-zone-1b"}),
					Price:        0.2,
					Available:    true,
				},
				{
					Requirements: scheduling.NewLabelRequirements(map[string]string{v1beta1.CapacityTypeLabelKey: v1beta1.CapacityTypeSpot, v1.LabelTopologyZone: "test-zone-1c"}),
					Price:        0.4,
					Available:    true,
				},
			},
		})
		nodePool = test.NodePool()
		nodeClaim, node = test.NodeClaimAndNode(v1beta1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1.LabelInstanceTypeStable:   currentInstance.Name,
					v1beta1.CapacityTypeLabelKey: currentInstance.Offerings[0].Requirements.Get(v1beta1.CapacityTypeLabelKey).Any(),
					v1.LabelTopologyZone:         currentInstance.Offerings[0].Requirements.Get(v1.LabelTopologyZone).Any(),
					v1beta1.NodePoolLabelKey:     nodePool.Name,
				},
			},
			Status: v1beta1.NodeClaimStatus{
				ProviderID: test.RandomProviderID(),
				Allocatable: map[v1.ResourceName]resource.Quantity{
					v1.ResourceCPU:  resource.MustParse("32"),
					v1.ResourcePods: resource.MustParse("100"),
				},
			},
		})
		cloudProvider.InstanceTypes = []*cloudprovider.InstanceType{
			currentInstance,
			replacementInstance,
		}
	})
	It("should remove taints from NodeClaims that were left tainted from a previous disruption action", func() {
		pod := test.Pod(test.PodOptions{
			ResourceRequirements: v1.ResourceRequirements{
				Requests: v1.ResourceList{
					v1.ResourceCPU:    resource.MustParse("100m"),
					v1.ResourceMemory: resource.MustParse("100Mi"),
				},
			},
		})
		nodePool.Spec.Disruption.ConsolidateAfter = &v1beta1.NillableDuration{Duration: nil}
		node.Spec.Taints = append(node.Spec.Taints, v1beta1.DisruptionNoScheduleTaint)
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node, pod)
		ExpectManualBinding(ctx, env.Client, pod, node)

		// inform cluster state about nodes and nodeClaims
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node}, []*v1beta1.NodeClaim{nodeClaim})

		// Trigger the reconcile loop to start but don't trigger the verify action
		wg := sync.WaitGroup{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			ExpectTriggerVerifyAction(&wg)
			ExpectSingletonReconciled(ctx, disruptionController)
		}()
		wg.Wait()
		node = ExpectNodeExists(ctx, env.Client, node.Name)
		Expect(node.Spec.Taints).ToNot(ContainElement(v1beta1.DisruptionNoScheduleTaint))
	})
	It("should add and remove taints from NodeClaims that fail to disrupt", func() {
		nodePool.Spec.Disruption.ConsolidationPolicy = v1beta1.ConsolidationPolicyWhenUnderutilized
		pod := test.Pod(test.PodOptions{
			ResourceRequirements: v1.ResourceRequirements{
				Requests: v1.ResourceList{
					v1.ResourceCPU:    resource.MustParse("100m"),
					v1.ResourceMemory: resource.MustParse("100Mi"),
				},
			},
		})
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node, pod)
		ExpectManualBinding(ctx, env.Client, pod, node)

		// inform cluster state about nodes and nodeClaims
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node}, []*v1beta1.NodeClaim{nodeClaim})

		// Trigger the reconcile loop to start but don't trigger the verify action
		wg := sync.WaitGroup{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			ExpectTriggerVerifyAction(&wg)
			ExpectSingletonReconciled(ctx, disruptionController)
		}()

		// Iterate in a loop until we get to the validation action
		// Then, apply the pods to the cluster and bind them to the nodes
		for {
			time.Sleep(100 * time.Millisecond)
			if len(ExpectNodeClaims(ctx, env.Client)) == 2 {
				break
			}
		}
		node = ExpectNodeExists(ctx, env.Client, node.Name)
		Expect(node.Spec.Taints).To(ContainElement(v1beta1.DisruptionNoScheduleTaint))

		createdNodeClaim := lo.Reject(ExpectNodeClaims(ctx, env.Client), func(nc *v1beta1.NodeClaim, _ int) bool {
			return nc.Name == nodeClaim.Name
		})
		ExpectDeleted(ctx, env.Client, createdNodeClaim[0])
		ExpectNodeClaimsCascadeDeletion(ctx, env.Client, createdNodeClaim[0])
		ExpectNotFound(ctx, env.Client, createdNodeClaim[0])
		wg.Wait()

		// Increment the clock so that the nodeclaim deletion isn't caught by the
		// eventual consistency delay.
		fakeClock.Step(6 * time.Second)
		ExpectSingletonReconciled(ctx, queue)

		node = ExpectNodeExists(ctx, env.Client, node.Name)
		Expect(node.Spec.Taints).ToNot(ContainElement(v1beta1.DisruptionNoScheduleTaint))
	})
})

var _ = Describe("BuildDisruptionBudgetMapping", func() {
	var nodePool *v1beta1.NodePool
	var nodeClaims []*v1beta1.NodeClaim
	var nodes []*v1.Node
	var numNodes int
	BeforeEach(func() {
		numNodes = 10
		nodePool = test.NodePool()
		nodeClaims, nodes = test.NodeClaimsAndNodes(numNodes, v1beta1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Finalizers: []string{"karpenter.sh/test-finalizer"},
				Labels: map[string]string{
					v1beta1.NodePoolLabelKey:     nodePool.Name,
					v1.LabelInstanceTypeStable:   mostExpensiveInstance.Name,
					v1beta1.CapacityTypeLabelKey: mostExpensiveOffering.Requirements.Get(v1beta1.CapacityTypeLabelKey).Any(),
					v1.LabelTopologyZone:         mostExpensiveOffering.Requirements.Get(v1.LabelTopologyZone).Any(),
				},
			},
			Status: v1beta1.NodeClaimStatus{
				Allocatable: map[v1.ResourceName]resource.Quantity{
					v1.ResourceCPU:  resource.MustParse("32"),
					v1.ResourcePods: resource.MustParse("100"),
				},
			},
		})
		ExpectApplied(ctx, env.Client, nodePool)

		for i := 0; i < numNodes; i++ {
			ExpectApplied(ctx, env.Client, nodeClaims[i], nodes[i])
		}

		// inform cluster state about nodes and nodeclaims
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, nodes, nodeClaims)
	})
	It("should not consider nodes that are not managed as part of disruption count", func() {
		nodePool.Spec.Disruption.Budgets = []v1beta1.Budget{{Nodes: "100%"}}
		ExpectApplied(ctx, env.Client, nodePool)
		unmanaged := test.Node()
		ExpectApplied(ctx, env.Client, unmanaged)
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{unmanaged}, []*v1beta1.NodeClaim{})
		budgets, err := disruption.BuildDisruptionBudgets(ctx, cluster, fakeClock, env.Client, recorder)
		Expect(err).To(Succeed())
		// This should not bring in the unmanaged node.
		for _, reason := range v1beta1.WellKnownDisruptionReasons {
			Expect(budgets[nodePool.Name][reason]).To(Equal(10))
		}
	})
	It("should not consider nodes that are not initialized as part of disruption count", func() {
		nodePool.Spec.Disruption.Budgets = []v1beta1.Budget{{Nodes: "100%"}}
		ExpectApplied(ctx, env.Client, nodePool)
		nodeClaim, node := test.NodeClaimAndNode(v1beta1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Finalizers: []string{"karpenter.sh/test-finalizer"},
				Labels: map[string]string{
					v1beta1.NodePoolLabelKey:     nodePool.Name,
					v1.LabelInstanceTypeStable:   mostExpensiveInstance.Name,
					v1beta1.CapacityTypeLabelKey: mostExpensiveOffering.Requirements.Get(v1beta1.CapacityTypeLabelKey).Any(),
					v1.LabelTopologyZone:         mostExpensiveOffering.Requirements.Get(v1.LabelTopologyZone).Any(),
				},
			},
			Status: v1beta1.NodeClaimStatus{
				Allocatable: map[v1.ResourceName]resource.Quantity{
					v1.ResourceCPU:  resource.MustParse("32"),
					v1.ResourcePods: resource.MustParse("100"),
				},
			},
		})
		ExpectApplied(ctx, env.Client, nodeClaim, node)
		ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(node))
		ExpectReconcileSucceeded(ctx, nodeClaimStateController, client.ObjectKeyFromObject(nodeClaim))

		budgets, err := disruption.BuildDisruptionBudgets(ctx, cluster, fakeClock, env.Client, recorder)
		Expect(err).To(Succeed())
		// This should not bring in the uninitialized node.
		for _, reason := range v1beta1.WellKnownDisruptionReasons {
			Expect(budgets[nodePool.Name][reason]).To(Equal(10))
		}
	})
	It("should not return a negative disruption value", func() {
		nodePool.Spec.Disruption.Budgets = []v1beta1.Budget{{Nodes: "10%"}}
		ExpectApplied(ctx, env.Client, nodePool)

		// Mark all nodeclaims as marked for deletion
		for _, i := range nodeClaims {
			Expect(env.Client.Delete(ctx, i)).To(Succeed())
			ExpectReconcileSucceeded(ctx, nodeClaimStateController, client.ObjectKeyFromObject(i))
		}
		// Mark all nodes as marked for deletion
		for _, i := range nodes {
			Expect(env.Client.Delete(ctx, i)).To(Succeed())
			ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(i))
		}

		budgets, err := disruption.BuildDisruptionBudgets(ctx, cluster, fakeClock, env.Client, recorder)
		Expect(err).To(Succeed())
		for _, reason := range v1beta1.WellKnownDisruptionReasons {
			Expect(budgets[nodePool.Name][reason]).To(Equal(0))
		}
	})
	It("should consider nodes with a deletion timestamp set and MarkedForDeletion to the disruption count", func() {
		nodePool.Spec.Disruption.Budgets = []v1beta1.Budget{{Nodes: "100%"}}
		ExpectApplied(ctx, env.Client, nodePool)

		// Delete one node and nodeclaim
		Expect(env.Client.Delete(ctx, nodeClaims[0])).To(Succeed())
		Expect(env.Client.Delete(ctx, nodes[0])).To(Succeed())
		cluster.MarkForDeletion(nodeClaims[1].Status.ProviderID)

		// Mark all nodeclaims as marked for deletion
		for _, i := range nodeClaims {
			ExpectReconcileSucceeded(ctx, nodeClaimStateController, client.ObjectKeyFromObject(i))
		}
		// Mark all nodes as marked for deletion
		for _, i := range nodes {
			ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(i))
		}

		budgets, err := disruption.BuildDisruptionBudgets(ctx, cluster, fakeClock, env.Client, recorder)
		Expect(err).To(Succeed())

		for _, reason := range v1beta1.WellKnownDisruptionReasons {
			Expect(budgets[nodePool.Name][reason]).To(Equal(8))
		}
	})
	It("should consider not ready nodes to the disruption count", func() {
		nodePool.Spec.Disruption.Budgets = []v1beta1.Budget{{Nodes: "100%"}}
		ExpectApplied(ctx, env.Client, nodePool)

		ExpectMakeNodesNotReady(ctx, env.Client, nodes[0], nodes[1])

		// Mark all nodeclaims as marked for deletion
		for _, i := range nodeClaims {
			ExpectReconcileSucceeded(ctx, nodeClaimStateController, client.ObjectKeyFromObject(i))
		}
		// Mark all nodes as marked for deletion
		for _, i := range nodes {
			ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(i))
		}

		budgets, err := disruption.BuildDisruptionBudgets(ctx, cluster, fakeClock, env.Client, recorder)
		Expect(err).To(Succeed())
		for _, reason := range v1beta1.WellKnownDisruptionReasons {
			Expect(budgets[nodePool.Name][reason]).To(Equal(8))
		}
	})
})

var _ = Describe("Pod Eviction Cost", func() {
	const standardPodCost = 1.0
	It("should have a standard disruptionCost for a pod with no priority or disruptionCost specified", func() {
		cost := disruptionutils.EvictionCost(ctx, &v1.Pod{})
		Expect(cost).To(BeNumerically("==", standardPodCost))
	})
	It("should have a higher disruptionCost for a pod with a positive deletion disruptionCost", func() {
		cost := disruptionutils.EvictionCost(ctx, &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
				v1.PodDeletionCost: "100",
			}},
		})
		Expect(cost).To(BeNumerically(">", standardPodCost))
	})
	It("should have a lower disruptionCost for a pod with a positive deletion disruptionCost", func() {
		cost := disruptionutils.EvictionCost(ctx, &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
				v1.PodDeletionCost: "-100",
			}},
		})
		Expect(cost).To(BeNumerically("<", standardPodCost))
	})
	It("should have higher costs for higher deletion costs", func() {
		cost1 := disruptionutils.EvictionCost(ctx, &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
				v1.PodDeletionCost: "101",
			}},
		})
		cost2 := disruptionutils.EvictionCost(ctx, &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
				v1.PodDeletionCost: "100",
			}},
		})
		cost3 := disruptionutils.EvictionCost(ctx, &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
				v1.PodDeletionCost: "99",
			}},
		})
		Expect(cost1).To(BeNumerically(">", cost2))
		Expect(cost2).To(BeNumerically(">", cost3))
	})
	It("should have a higher disruptionCost for a pod with a higher priority", func() {
		cost := disruptionutils.EvictionCost(ctx, &v1.Pod{
			Spec: v1.PodSpec{Priority: lo.ToPtr(int32(1))},
		})
		Expect(cost).To(BeNumerically(">", standardPodCost))
	})
	It("should have a lower disruptionCost for a pod with a lower priority", func() {
		cost := disruptionutils.EvictionCost(ctx, &v1.Pod{
			Spec: v1.PodSpec{Priority: lo.ToPtr(int32(-1))},
		})
		Expect(cost).To(BeNumerically("<", standardPodCost))
	})
})

var _ = Describe("Candidate Filtering", func() {
	var nodePool *v1beta1.NodePool
	var nodePoolMap map[string]*v1beta1.NodePool
	var nodePoolInstanceTypeMap map[string]map[string]*cloudprovider.InstanceType
	var pdbLimits pdb.Limits
	BeforeEach(func() {
		nodePool = test.NodePool()
		nodePoolMap = map[string]*v1beta1.NodePool{
			nodePool.Name: nodePool,
		}
		nodePoolInstanceTypeMap = map[string]map[string]*cloudprovider.InstanceType{
			nodePool.Name: lo.SliceToMap(cloudProvider.InstanceTypes, func(i *cloudprovider.InstanceType) (string, *cloudprovider.InstanceType) {
				return i.Name, i
			}),
		}
		var err error
		pdbLimits, err = pdb.NewLimits(ctx, fakeClock, env.Client)
		Expect(err).ToNot(HaveOccurred())
	})
	It("should not consider candidates that have do-not-disrupt pods scheduled", func() {
		nodeClaim, node := test.NodeClaimAndNode(v1beta1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1beta1.NodePoolLabelKey:     nodePool.Name,
					v1.LabelInstanceTypeStable:   mostExpensiveInstance.Name,
					v1beta1.CapacityTypeLabelKey: mostExpensiveOffering.Requirements.Get(v1beta1.CapacityTypeLabelKey).Any(),
					v1.LabelTopologyZone:         mostExpensiveOffering.Requirements.Get(v1.LabelTopologyZone).Any(),
				},
			},
		})
		pod := test.Pod(test.PodOptions{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					v1beta1.DoNotDisruptAnnotationKey: "true",
				},
			},
		})
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node, pod)
		ExpectManualBinding(ctx, env.Client, pod, node)
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node}, []*v1beta1.NodeClaim{nodeClaim})

		Expect(cluster.Nodes()).To(HaveLen(1))
		_, err := disruption.NewCandidate(ctx, env.Client, recorder, fakeClock, cluster.Nodes()[0], pdbLimits, nodePoolMap, nodePoolInstanceTypeMap, queue)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(Equal(fmt.Sprintf(`pod %q has "karpenter.sh/do-not-disrupt" annotation`, client.ObjectKeyFromObject(pod))))
		Expect(recorder.DetectedEvent(fmt.Sprintf(`Cannot disrupt Node: pod %q has "karpenter.sh/do-not-disrupt" annotation`, client.ObjectKeyFromObject(pod)))).To(BeTrue())
	})
	It("should not consider candidates that have do-not-disrupt mirror pods scheduled", func() {
		nodeClaim, node := test.NodeClaimAndNode(v1beta1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1beta1.NodePoolLabelKey:     nodePool.Name,
					v1.LabelInstanceTypeStable:   mostExpensiveInstance.Name,
					v1beta1.CapacityTypeLabelKey: mostExpensiveOffering.Requirements.Get(v1beta1.CapacityTypeLabelKey).Any(),
					v1.LabelTopologyZone:         mostExpensiveOffering.Requirements.Get(v1.LabelTopologyZone).Any(),
				},
			},
		})
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
		pod := test.Pod(test.PodOptions{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					v1beta1.DoNotDisruptAnnotationKey: "true",
				},
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion: "v1",
						Kind:       "Node",
						Name:       node.Name,
						UID:        node.UID,
						Controller: lo.ToPtr(true),
					},
				},
			},
		})
		ExpectApplied(ctx, env.Client, pod)
		ExpectManualBinding(ctx, env.Client, pod, node)
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node}, []*v1beta1.NodeClaim{nodeClaim})

		Expect(cluster.Nodes()).To(HaveLen(1))
		_, err := disruption.NewCandidate(ctx, env.Client, recorder, fakeClock, cluster.Nodes()[0], pdbLimits, nodePoolMap, nodePoolInstanceTypeMap, queue)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(Equal(fmt.Sprintf(`pod %q has "karpenter.sh/do-not-disrupt" annotation`, client.ObjectKeyFromObject(pod))))
		Expect(recorder.DetectedEvent(fmt.Sprintf(`Cannot disrupt Node: pod %q has "karpenter.sh/do-not-disrupt" annotation`, client.ObjectKeyFromObject(pod)))).To(BeTrue())
	})
	It("should not consider candidates that have do-not-disrupt daemonset pods scheduled", func() {
		daemonSet := test.DaemonSet()
		nodeClaim, node := test.NodeClaimAndNode(v1beta1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1beta1.NodePoolLabelKey:     nodePool.Name,
					v1.LabelInstanceTypeStable:   mostExpensiveInstance.Name,
					v1beta1.CapacityTypeLabelKey: mostExpensiveOffering.Requirements.Get(v1beta1.CapacityTypeLabelKey).Any(),
					v1.LabelTopologyZone:         mostExpensiveOffering.Requirements.Get(v1.LabelTopologyZone).Any(),
				},
			},
		})
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node, daemonSet)
		pod := test.Pod(test.PodOptions{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					v1beta1.DoNotDisruptAnnotationKey: "true",
				},
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion: "apps/v1",
						Kind:       "DaemonSet",
						Name:       daemonSet.Name,
						UID:        daemonSet.UID,
						Controller: lo.ToPtr(true),
					},
				},
			},
		})
		ExpectApplied(ctx, env.Client, pod)
		ExpectManualBinding(ctx, env.Client, pod, node)
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node}, []*v1beta1.NodeClaim{nodeClaim})

		Expect(cluster.Nodes()).To(HaveLen(1))
		_, err := disruption.NewCandidate(ctx, env.Client, recorder, fakeClock, cluster.Nodes()[0], pdbLimits, nodePoolMap, nodePoolInstanceTypeMap, queue)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(Equal(fmt.Sprintf(`pod %q has "karpenter.sh/do-not-disrupt" annotation`, client.ObjectKeyFromObject(pod))))
		Expect(recorder.DetectedEvent(fmt.Sprintf(`Cannot disrupt Node: pod %q has "karpenter.sh/do-not-disrupt" annotation`, client.ObjectKeyFromObject(pod)))).To(BeTrue())
	})
	It("should consider candidates that have do-not-disrupt terminating pods", func() {
		nodeClaim, node := test.NodeClaimAndNode(v1beta1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1beta1.NodePoolLabelKey:     nodePool.Name,
					v1.LabelInstanceTypeStable:   mostExpensiveInstance.Name,
					v1beta1.CapacityTypeLabelKey: mostExpensiveOffering.Requirements.Get(v1beta1.CapacityTypeLabelKey).Any(),
					v1.LabelTopologyZone:         mostExpensiveOffering.Requirements.Get(v1.LabelTopologyZone).Any(),
				},
			},
		})
		pod := test.Pod(test.PodOptions{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					v1beta1.DoNotDisruptAnnotationKey: "true",
				},
			},
		})
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node, pod)
		ExpectManualBinding(ctx, env.Client, pod, node)
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node}, []*v1beta1.NodeClaim{nodeClaim})

		ExpectDeletionTimestampSet(ctx, env.Client, pod)

		Expect(cluster.Nodes()).To(HaveLen(1))
		c, err := disruption.NewCandidate(ctx, env.Client, recorder, fakeClock, cluster.Nodes()[0], pdbLimits, nodePoolMap, nodePoolInstanceTypeMap, queue)
		Expect(err).ToNot(HaveOccurred())
		Expect(c.NodeClaim).ToNot(BeNil())
		Expect(c.Node).ToNot(BeNil())
	})
	It("should consider candidates that have do-not-disrupt terminal pods", func() {
		nodeClaim, node := test.NodeClaimAndNode(v1beta1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1beta1.NodePoolLabelKey:     nodePool.Name,
					v1.LabelInstanceTypeStable:   mostExpensiveInstance.Name,
					v1beta1.CapacityTypeLabelKey: mostExpensiveOffering.Requirements.Get(v1beta1.CapacityTypeLabelKey).Any(),
					v1.LabelTopologyZone:         mostExpensiveOffering.Requirements.Get(v1.LabelTopologyZone).Any(),
				},
			},
		})
		podSucceeded := test.Pod(test.PodOptions{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					v1beta1.DoNotDisruptAnnotationKey: "true",
				},
			},
			Phase: v1.PodSucceeded,
		})
		podFailed := test.Pod(test.PodOptions{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					v1beta1.DoNotDisruptAnnotationKey: "true",
				},
			},
			Phase: v1.PodFailed,
		})
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node, podSucceeded, podFailed)
		ExpectManualBinding(ctx, env.Client, podSucceeded, node)
		ExpectManualBinding(ctx, env.Client, podFailed, node)
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node}, []*v1beta1.NodeClaim{nodeClaim})

		Expect(cluster.Nodes()).To(HaveLen(1))
		c, err := disruption.NewCandidate(ctx, env.Client, recorder, fakeClock, cluster.Nodes()[0], pdbLimits, nodePoolMap, nodePoolInstanceTypeMap, queue)
		Expect(err).ToNot(HaveOccurred())
		Expect(c.NodeClaim).ToNot(BeNil())
		Expect(c.Node).ToNot(BeNil())
	})
	It("should not consider candidates that have do-not-disrupt on nodes", func() {
		nodeClaim, node := test.NodeClaimAndNode(v1beta1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					v1beta1.DoNotDisruptAnnotationKey: "true",
				},
				Labels: map[string]string{
					v1beta1.NodePoolLabelKey:     nodePool.Name,
					v1.LabelInstanceTypeStable:   mostExpensiveInstance.Name,
					v1beta1.CapacityTypeLabelKey: mostExpensiveOffering.Requirements.Get(v1beta1.CapacityTypeLabelKey).Any(),
					v1.LabelTopologyZone:         mostExpensiveOffering.Requirements.Get(v1.LabelTopologyZone).Any(),
				},
			},
		})
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node}, []*v1beta1.NodeClaim{nodeClaim})

		Expect(cluster.Nodes()).To(HaveLen(1))
		_, err := disruption.NewCandidate(ctx, env.Client, recorder, fakeClock, cluster.Nodes()[0], pdbLimits, nodePoolMap, nodePoolInstanceTypeMap, queue)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(Equal(`disruption is blocked through the "karpenter.sh/do-not-disrupt" annotation`))
		Expect(recorder.DetectedEvent(`Cannot disrupt Node: disruption is blocked through the "karpenter.sh/do-not-disrupt" annotation`)).To(BeTrue())
	})
	It("should not consider candidates that have fully blocking PDBs", func() {
		nodeClaim, node := test.NodeClaimAndNode(v1beta1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1beta1.NodePoolLabelKey:     nodePool.Name,
					v1.LabelInstanceTypeStable:   mostExpensiveInstance.Name,
					v1beta1.CapacityTypeLabelKey: mostExpensiveOffering.Requirements.Get(v1beta1.CapacityTypeLabelKey).Any(),
					v1.LabelTopologyZone:         mostExpensiveOffering.Requirements.Get(v1.LabelTopologyZone).Any(),
				},
			},
		})
		podLabels := map[string]string{"test": "value"}
		pod := test.Pod(test.PodOptions{
			ObjectMeta: metav1.ObjectMeta{
				Labels: podLabels,
			},
		})
		budget := test.PodDisruptionBudget(test.PDBOptions{
			Labels:         podLabels,
			MaxUnavailable: fromInt(0),
		})
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node, pod, budget)
		ExpectManualBinding(ctx, env.Client, pod, node)

		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node}, []*v1beta1.NodeClaim{nodeClaim})

		var err error
		pdbLimits, err = pdb.NewLimits(ctx, fakeClock, env.Client)
		Expect(err).ToNot(HaveOccurred())

		Expect(cluster.Nodes()).To(HaveLen(1))
		_, err = disruption.NewCandidate(ctx, env.Client, recorder, fakeClock, cluster.Nodes()[0], pdbLimits, nodePoolMap, nodePoolInstanceTypeMap, queue)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(Equal(fmt.Sprintf(`pdb %q prevents pod evictions`, client.ObjectKeyFromObject(budget))))
		Expect(recorder.DetectedEvent(fmt.Sprintf(`Cannot disrupt Node: pdb %q prevents pod evictions`, client.ObjectKeyFromObject(budget)))).To(BeTrue())
	})
	It("should not consider candidates that have fully blocking PDBs on daemonset pods", func() {
		daemonSet := test.DaemonSet()
		nodeClaim, node := test.NodeClaimAndNode(v1beta1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1beta1.NodePoolLabelKey:     nodePool.Name,
					v1.LabelInstanceTypeStable:   mostExpensiveInstance.Name,
					v1beta1.CapacityTypeLabelKey: mostExpensiveOffering.Requirements.Get(v1beta1.CapacityTypeLabelKey).Any(),
					v1.LabelTopologyZone:         mostExpensiveOffering.Requirements.Get(v1.LabelTopologyZone).Any(),
				},
			},
		})
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node, daemonSet)
		podLabels := map[string]string{"test": "value"}
		pod := test.Pod(test.PodOptions{
			ObjectMeta: metav1.ObjectMeta{
				Labels: podLabels,
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion: "apps/v1",
						Kind:       "DaemonSet",
						Name:       daemonSet.Name,
						UID:        daemonSet.UID,
						Controller: lo.ToPtr(true),
					},
				},
			},
		})
		budget := test.PodDisruptionBudget(test.PDBOptions{
			Labels:         podLabels,
			MaxUnavailable: fromInt(0),
		})
		ExpectApplied(ctx, env.Client, pod, budget)
		ExpectManualBinding(ctx, env.Client, pod, node)

		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node}, []*v1beta1.NodeClaim{nodeClaim})

		var err error
		pdbLimits, err = pdb.NewLimits(ctx, fakeClock, env.Client)
		Expect(err).ToNot(HaveOccurred())

		Expect(cluster.Nodes()).To(HaveLen(1))
		_, err = disruption.NewCandidate(ctx, env.Client, recorder, fakeClock, cluster.Nodes()[0], pdbLimits, nodePoolMap, nodePoolInstanceTypeMap, queue)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(Equal(fmt.Sprintf(`pdb %q prevents pod evictions`, client.ObjectKeyFromObject(budget))))
		Expect(recorder.DetectedEvent(fmt.Sprintf(`Cannot disrupt Node: pdb %q prevents pod evictions`, client.ObjectKeyFromObject(budget)))).To(BeTrue())
	})
	It("should consider candidates that have fully blocking PDBs on mirror pods", func() {
		nodeClaim, node := test.NodeClaimAndNode(v1beta1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1beta1.NodePoolLabelKey:     nodePool.Name,
					v1.LabelInstanceTypeStable:   mostExpensiveInstance.Name,
					v1beta1.CapacityTypeLabelKey: mostExpensiveOffering.Requirements.Get(v1beta1.CapacityTypeLabelKey).Any(),
					v1.LabelTopologyZone:         mostExpensiveOffering.Requirements.Get(v1.LabelTopologyZone).Any(),
				},
			},
		})
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
		podLabels := map[string]string{"test": "value"}
		pod := test.Pod(test.PodOptions{
			ObjectMeta: metav1.ObjectMeta{
				Labels: podLabels,
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion: "v1",
						Kind:       "Node",
						Name:       node.Name,
						UID:        node.UID,
						Controller: lo.ToPtr(true),
					},
				},
			},
		})
		budget := test.PodDisruptionBudget(test.PDBOptions{
			Labels:         podLabels,
			MaxUnavailable: fromInt(0),
		})
		ExpectApplied(ctx, env.Client, pod, budget)
		ExpectManualBinding(ctx, env.Client, pod, node)

		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node}, []*v1beta1.NodeClaim{nodeClaim})

		var err error
		pdbLimits, err = pdb.NewLimits(ctx, fakeClock, env.Client)
		Expect(err).ToNot(HaveOccurred())

		Expect(cluster.Nodes()).To(HaveLen(1))
		c, err := disruption.NewCandidate(ctx, env.Client, recorder, fakeClock, cluster.Nodes()[0], pdbLimits, nodePoolMap, nodePoolInstanceTypeMap, queue)
		Expect(err).ToNot(HaveOccurred())
		Expect(c.NodeClaim).ToNot(BeNil())
		Expect(c.Node).ToNot(BeNil())
	})
	It("should consider candidates that have fully blocking PDBs on terminal pods", func() {
		nodeClaim, node := test.NodeClaimAndNode(v1beta1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1beta1.NodePoolLabelKey:     nodePool.Name,
					v1.LabelInstanceTypeStable:   mostExpensiveInstance.Name,
					v1beta1.CapacityTypeLabelKey: mostExpensiveOffering.Requirements.Get(v1beta1.CapacityTypeLabelKey).Any(),
					v1.LabelTopologyZone:         mostExpensiveOffering.Requirements.Get(v1.LabelTopologyZone).Any(),
				},
			},
		})
		podLabels := map[string]string{"test": "value"}
		succeededPod := test.Pod(test.PodOptions{
			ObjectMeta: metav1.ObjectMeta{
				Labels: podLabels,
			},
			Phase: v1.PodSucceeded,
		})
		failedPod := test.Pod(test.PodOptions{
			ObjectMeta: metav1.ObjectMeta{
				Labels: podLabels,
			},
			Phase: v1.PodFailed,
		})
		budget := test.PodDisruptionBudget(test.PDBOptions{
			Labels:         podLabels,
			MaxUnavailable: fromInt(0),
		})
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node, succeededPod, failedPod, budget)
		ExpectManualBinding(ctx, env.Client, succeededPod, node)
		ExpectManualBinding(ctx, env.Client, failedPod, node)

		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node}, []*v1beta1.NodeClaim{nodeClaim})

		var err error
		pdbLimits, err = pdb.NewLimits(ctx, fakeClock, env.Client)
		Expect(err).ToNot(HaveOccurred())

		Expect(cluster.Nodes()).To(HaveLen(1))
		c, err := disruption.NewCandidate(ctx, env.Client, recorder, fakeClock, cluster.Nodes()[0], pdbLimits, nodePoolMap, nodePoolInstanceTypeMap, queue)
		Expect(err).ToNot(HaveOccurred())
		Expect(c.NodeClaim).ToNot(BeNil())
		Expect(c.Node).ToNot(BeNil())
	})
	It("should consider candidates that have fully blocking PDBs on terminating pods", func() {
		nodeClaim, node := test.NodeClaimAndNode(v1beta1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1beta1.NodePoolLabelKey:     nodePool.Name,
					v1.LabelInstanceTypeStable:   mostExpensiveInstance.Name,
					v1beta1.CapacityTypeLabelKey: mostExpensiveOffering.Requirements.Get(v1beta1.CapacityTypeLabelKey).Any(),
					v1.LabelTopologyZone:         mostExpensiveOffering.Requirements.Get(v1.LabelTopologyZone).Any(),
				},
			},
		})
		podLabels := map[string]string{"test": "value"}
		pod := test.Pod(test.PodOptions{
			ObjectMeta: metav1.ObjectMeta{
				Labels: podLabels,
			},
		})
		budget := test.PodDisruptionBudget(test.PDBOptions{
			Labels:         podLabels,
			MaxUnavailable: fromInt(0),
		})
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node, pod, budget)
		ExpectManualBinding(ctx, env.Client, pod, node)

		ExpectDeletionTimestampSet(ctx, env.Client, pod)

		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node}, []*v1beta1.NodeClaim{nodeClaim})

		var err error
		pdbLimits, err = pdb.NewLimits(ctx, fakeClock, env.Client)
		Expect(err).ToNot(HaveOccurred())

		Expect(cluster.Nodes()).To(HaveLen(1))
		c, err := disruption.NewCandidate(ctx, env.Client, recorder, fakeClock, cluster.Nodes()[0], pdbLimits, nodePoolMap, nodePoolInstanceTypeMap, queue)
		Expect(err).ToNot(HaveOccurred())
		Expect(c.NodeClaim).ToNot(BeNil())
		Expect(c.Node).ToNot(BeNil())
	})
	It("should not consider candidates that has just a Node representation", func() {
		_, node := test.NodeClaimAndNode(v1beta1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1beta1.NodePoolLabelKey:     nodePool.Name,
					v1.LabelInstanceTypeStable:   mostExpensiveInstance.Name,
					v1beta1.CapacityTypeLabelKey: mostExpensiveOffering.Requirements.Get(v1beta1.CapacityTypeLabelKey).Any(),
					v1.LabelTopologyZone:         mostExpensiveOffering.Requirements.Get(v1.LabelTopologyZone).Any(),
				},
			},
		})
		ExpectApplied(ctx, env.Client, nodePool, node)
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node}, nil)

		Expect(cluster.Nodes()).To(HaveLen(1))
		_, err := disruption.NewCandidate(ctx, env.Client, recorder, fakeClock, cluster.Nodes()[0], pdbLimits, nodePoolMap, nodePoolInstanceTypeMap, queue)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(Equal("state node doesn't contain both a node and a nodeclaim"))
	})
	It("should not consider candidate that has just a NodeClaim representation", func() {
		nodeClaim, _ := test.NodeClaimAndNode(v1beta1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1beta1.NodePoolLabelKey:     nodePool.Name,
					v1.LabelInstanceTypeStable:   mostExpensiveInstance.Name,
					v1beta1.CapacityTypeLabelKey: mostExpensiveOffering.Requirements.Get(v1beta1.CapacityTypeLabelKey).Any(),
					v1.LabelTopologyZone:         mostExpensiveOffering.Requirements.Get(v1.LabelTopologyZone).Any(),
				},
			},
		})
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim)
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, nil, []*v1beta1.NodeClaim{nodeClaim})

		Expect(cluster.Nodes()).To(HaveLen(1))
		_, err := disruption.NewCandidate(ctx, env.Client, recorder, fakeClock, cluster.Nodes()[0], pdbLimits, nodePoolMap, nodePoolInstanceTypeMap, queue)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(Equal("state node doesn't contain both a node and a nodeclaim"))
	})
	It("should not consider candidates that are nominated", func() {
		nodeClaim, node := test.NodeClaimAndNode(v1beta1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1beta1.NodePoolLabelKey:     nodePool.Name,
					v1.LabelInstanceTypeStable:   mostExpensiveInstance.Name,
					v1beta1.CapacityTypeLabelKey: mostExpensiveOffering.Requirements.Get(v1beta1.CapacityTypeLabelKey).Any(),
					v1.LabelTopologyZone:         mostExpensiveOffering.Requirements.Get(v1.LabelTopologyZone).Any(),
				},
			},
		})
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node}, []*v1beta1.NodeClaim{nodeClaim})
		cluster.NominateNodeForPod(ctx, node.Spec.ProviderID)

		Expect(cluster.Nodes()).To(HaveLen(1))
		_, err := disruption.NewCandidate(ctx, env.Client, recorder, fakeClock, cluster.Nodes()[0], pdbLimits, nodePoolMap, nodePoolInstanceTypeMap, queue)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(Equal("state node is nominated for a pending pod"))
		Expect(recorder.DetectedEvent("Cannot disrupt Node: state node is nominated for a pending pod")).To(BeTrue())
	})
	It("should not consider candidates that are deleting", func() {
		nodeClaim, node := test.NodeClaimAndNode(v1beta1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1beta1.NodePoolLabelKey:     nodePool.Name,
					v1.LabelInstanceTypeStable:   mostExpensiveInstance.Name,
					v1beta1.CapacityTypeLabelKey: mostExpensiveOffering.Requirements.Get(v1beta1.CapacityTypeLabelKey).Any(),
					v1.LabelTopologyZone:         mostExpensiveOffering.Requirements.Get(v1.LabelTopologyZone).Any(),
				},
			},
		})
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node}, []*v1beta1.NodeClaim{nodeClaim})

		ExpectDeletionTimestampSet(ctx, env.Client, nodeClaim)
		ExpectReconcileSucceeded(ctx, nodeClaimStateController, client.ObjectKeyFromObject(nodeClaim))

		Expect(cluster.Nodes()).To(HaveLen(1))
		_, err := disruption.NewCandidate(ctx, env.Client, recorder, fakeClock, cluster.Nodes()[0], pdbLimits, nodePoolMap, nodePoolInstanceTypeMap, queue)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(Equal("state node is marked for deletion"))
	})
	It("should not consider candidates that are MarkedForDeletion", func() {
		nodeClaim, node := test.NodeClaimAndNode(v1beta1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1beta1.NodePoolLabelKey:     nodePool.Name,
					v1.LabelInstanceTypeStable:   mostExpensiveInstance.Name,
					v1beta1.CapacityTypeLabelKey: mostExpensiveOffering.Requirements.Get(v1beta1.CapacityTypeLabelKey).Any(),
					v1.LabelTopologyZone:         mostExpensiveOffering.Requirements.Get(v1.LabelTopologyZone).Any(),
				},
			},
		})
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node}, []*v1beta1.NodeClaim{nodeClaim})

		cluster.MarkForDeletion(node.Spec.ProviderID)

		Expect(cluster.Nodes()).To(HaveLen(1))
		_, err := disruption.NewCandidate(ctx, env.Client, recorder, fakeClock, cluster.Nodes()[0], pdbLimits, nodePoolMap, nodePoolInstanceTypeMap, queue)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(Equal("state node is marked for deletion"))
	})
	It("should not consider candidates that aren't yet initialized", func() {
		nodeClaim, node := test.NodeClaimAndNode(v1beta1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1beta1.NodePoolLabelKey:     nodePool.Name,
					v1.LabelInstanceTypeStable:   mostExpensiveInstance.Name,
					v1beta1.CapacityTypeLabelKey: mostExpensiveOffering.Requirements.Get(v1beta1.CapacityTypeLabelKey).Any(),
					v1.LabelTopologyZone:         mostExpensiveOffering.Requirements.Get(v1.LabelTopologyZone).Any(),
				},
			},
		})
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
		ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(node))
		ExpectReconcileSucceeded(ctx, nodeClaimStateController, client.ObjectKeyFromObject(nodeClaim))

		Expect(cluster.Nodes()).To(HaveLen(1))
		_, err := disruption.NewCandidate(ctx, env.Client, recorder, fakeClock, cluster.Nodes()[0], pdbLimits, nodePoolMap, nodePoolInstanceTypeMap, queue)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(Equal("state node isn't initialized"))
	})
	It("should not consider candidates that are not owned by a NodePool (no karpenter.sh/nodepool label)", func() {
		nodeClaim, node := test.NodeClaimAndNode(v1beta1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1.LabelInstanceTypeStable:   mostExpensiveInstance.Name,
					v1beta1.CapacityTypeLabelKey: mostExpensiveOffering.Requirements.Get(v1beta1.CapacityTypeLabelKey).Any(),
					v1.LabelTopologyZone:         mostExpensiveOffering.Requirements.Get(v1.LabelTopologyZone).Any(),
				},
			},
		})
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node}, []*v1beta1.NodeClaim{nodeClaim})

		Expect(cluster.Nodes()).To(HaveLen(1))
		_, err := disruption.NewCandidate(ctx, env.Client, recorder, fakeClock, cluster.Nodes()[0], pdbLimits, nodePoolMap, nodePoolInstanceTypeMap, queue)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(Equal(`state node doesn't have required label "karpenter.sh/nodepool"`))
		Expect(recorder.DetectedEvent(`Cannot disrupt Node: state node doesn't have required label "karpenter.sh/nodepool"`)).To(BeTrue())
	})
	It("should not consider candidates that are have a non-existent NodePool", func() {
		nodeClaim, node := test.NodeClaimAndNode(v1beta1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1beta1.NodePoolLabelKey:     nodePool.Name,
					v1.LabelInstanceTypeStable:   mostExpensiveInstance.Name,
					v1beta1.CapacityTypeLabelKey: mostExpensiveOffering.Requirements.Get(v1beta1.CapacityTypeLabelKey).Any(),
					v1.LabelTopologyZone:         mostExpensiveOffering.Requirements.Get(v1.LabelTopologyZone).Any(),
				},
			},
		})
		// Don't apply the NodePool
		ExpectApplied(ctx, env.Client, nodeClaim, node)
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node}, []*v1beta1.NodeClaim{nodeClaim})

		// Mock the NodePool not existing by removing it from the nodePool and nodePoolInstanceTypes maps
		delete(nodePoolMap, nodePool.Name)
		delete(nodePoolInstanceTypeMap, nodePool.Name)

		Expect(cluster.Nodes()).To(HaveLen(1))
		_, err := disruption.NewCandidate(ctx, env.Client, recorder, fakeClock, cluster.Nodes()[0], pdbLimits, nodePoolMap, nodePoolInstanceTypeMap, queue)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(Equal(fmt.Sprintf("nodepool %q can't be resolved for state node", nodePool.Name)))
		Expect(recorder.DetectedEvent(fmt.Sprintf("Cannot disrupt Node: NodePool %q not found", nodePool.Name))).To(BeTrue())
	})
	It("should not consider candidates that do not have the karpenter.sh/capacity-type label", func() {
		nodeClaim, node := test.NodeClaimAndNode(v1beta1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1beta1.NodePoolLabelKey:   nodePool.Name,
					v1.LabelInstanceTypeStable: mostExpensiveInstance.Name,
					v1.LabelTopologyZone:       mostExpensiveOffering.Requirements.Get(v1.LabelTopologyZone).Any(),
				},
			},
		})
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node}, []*v1beta1.NodeClaim{nodeClaim})

		Expect(cluster.Nodes()).To(HaveLen(1))
		_, err := disruption.NewCandidate(ctx, env.Client, recorder, fakeClock, cluster.Nodes()[0], pdbLimits, nodePoolMap, nodePoolInstanceTypeMap, queue)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(Equal(`state node doesn't have required label "karpenter.sh/capacity-type"`))
		Expect(recorder.DetectedEvent(`Cannot disrupt Node: state node doesn't have required label "karpenter.sh/capacity-type"`)).To(BeTrue())
	})
	It("should not consider candidates that do not have the topology.kubernetes.io/zone label", func() {
		nodeClaim, node := test.NodeClaimAndNode(v1beta1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1beta1.NodePoolLabelKey:     nodePool.Name,
					v1.LabelInstanceTypeStable:   mostExpensiveInstance.Name,
					v1beta1.CapacityTypeLabelKey: mostExpensiveOffering.Requirements.Get(v1beta1.CapacityTypeLabelKey).Any(),
				},
			},
		})
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node}, []*v1beta1.NodeClaim{nodeClaim})

		Expect(cluster.Nodes()).To(HaveLen(1))
		_, err := disruption.NewCandidate(ctx, env.Client, recorder, fakeClock, cluster.Nodes()[0], pdbLimits, nodePoolMap, nodePoolInstanceTypeMap, queue)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(Equal(`state node doesn't have required label "topology.kubernetes.io/zone"`))
		Expect(recorder.DetectedEvent(`Cannot disrupt Node: state node doesn't have required label "topology.kubernetes.io/zone"`)).To(BeTrue())
	})
	It("should not consider candidates that do not have the node.kubernetes.io/instance-type label", func() {
		nodeClaim, node := test.NodeClaimAndNode(v1beta1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1beta1.NodePoolLabelKey:     nodePool.Name,
					v1beta1.CapacityTypeLabelKey: mostExpensiveOffering.Requirements.Get(v1beta1.CapacityTypeLabelKey).Any(),
					v1.LabelTopologyZone:         mostExpensiveOffering.Requirements.Get(v1.LabelTopologyZone).Any(),
				},
			},
		})
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node}, []*v1beta1.NodeClaim{nodeClaim})

		Expect(cluster.Nodes()).To(HaveLen(1))
		_, err := disruption.NewCandidate(ctx, env.Client, recorder, fakeClock, cluster.Nodes()[0], pdbLimits, nodePoolMap, nodePoolInstanceTypeMap, queue)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(Equal(`state node doesn't have required label "node.kubernetes.io/instance-type"`))
		Expect(recorder.DetectedEvent(`Cannot disrupt Node: state node doesn't have required label "node.kubernetes.io/instance-type"`)).To(BeTrue())
	})
	It("should not consider candidates that have an instance type that cannot be resolved", func() {
		nodeClaim, node := test.NodeClaimAndNode(v1beta1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1beta1.NodePoolLabelKey:     nodePool.Name,
					v1.LabelInstanceTypeStable:   mostExpensiveInstance.Name,
					v1beta1.CapacityTypeLabelKey: mostExpensiveOffering.Requirements.Get(v1beta1.CapacityTypeLabelKey).Any(),
					v1.LabelTopologyZone:         mostExpensiveOffering.Requirements.Get(v1.LabelTopologyZone).Any(),
				},
			},
		})
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node}, []*v1beta1.NodeClaim{nodeClaim})

		// Mock the InstanceType not existing by removing it from the nodePoolInstanceTypes map
		delete(nodePoolInstanceTypeMap[nodePool.Name], mostExpensiveInstance.Name)

		Expect(cluster.Nodes()).To(HaveLen(1))
		_, err := disruption.NewCandidate(ctx, env.Client, recorder, fakeClock, cluster.Nodes()[0], pdbLimits, nodePoolMap, nodePoolInstanceTypeMap, queue)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(Equal(fmt.Sprintf("instance type %q can't be resolved", mostExpensiveInstance.Name)))
		Expect(recorder.DetectedEvent(fmt.Sprintf("Cannot disrupt Node: Instance Type %q not found", mostExpensiveInstance.Name))).To(BeTrue())
	})
	It("should not consider candidates that are actively being processed in the queue", func() {
		nodeClaim, node := test.NodeClaimAndNode(v1beta1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1beta1.NodePoolLabelKey:     nodePool.Name,
					v1.LabelInstanceTypeStable:   mostExpensiveInstance.Name,
					v1beta1.CapacityTypeLabelKey: mostExpensiveOffering.Requirements.Get(v1beta1.CapacityTypeLabelKey).Any(),
					v1.LabelTopologyZone:         mostExpensiveOffering.Requirements.Get(v1.LabelTopologyZone).Any(),
				},
			},
		})
		ExpectApplied(ctx, env.Client, nodePool, nodeClaim, node)
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node}, []*v1beta1.NodeClaim{nodeClaim})

		Expect(cluster.Nodes()).To(HaveLen(1))
		Expect(queue.Add(orchestration.NewCommand([]string{}, []*state.StateNode{cluster.Nodes()[0]}, "", "test-method", "fake-type"))).To(Succeed())

		_, err := disruption.NewCandidate(ctx, env.Client, recorder, fakeClock, cluster.Nodes()[0], pdbLimits, nodePoolMap, nodePoolInstanceTypeMap, queue)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(Equal("candidate is already being disrupted"))
	})
})

var _ = Describe("Metrics", func() {
	var nodePool *v1beta1.NodePool
	var labels = map[string]string{
		"app": "test",
	}
	BeforeEach(func() {
		nodePool = test.NodePool(v1beta1.NodePool{
			Spec: v1beta1.NodePoolSpec{
				Disruption: v1beta1.Disruption{
					ConsolidationPolicy: v1beta1.ConsolidationPolicyWhenUnderutilized,
					// Disrupt away!
					Budgets: []v1beta1.Budget{{
						Nodes: "100%",
					}},
				},
			},
		})
	})
	It("should fire metrics for single node empty disruption", func() {
		nodeClaim, node := test.NodeClaimAndNode(v1beta1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1beta1.NodePoolLabelKey:     nodePool.Name,
					v1.LabelInstanceTypeStable:   mostExpensiveInstance.Name,
					v1beta1.CapacityTypeLabelKey: mostExpensiveOffering.Requirements.Get(v1beta1.CapacityTypeLabelKey).Any(),
					v1.LabelTopologyZone:         mostExpensiveOffering.Requirements.Get(v1.LabelTopologyZone).Any(),
				},
			},
			Status: v1beta1.NodeClaimStatus{
				ProviderID: test.RandomProviderID(),
				Allocatable: map[v1.ResourceName]resource.Quantity{
					v1.ResourceCPU:  resource.MustParse("32"),
					v1.ResourcePods: resource.MustParse("100"),
				},
			},
		})
		nodeClaim.StatusConditions().SetTrue(v1beta1.ConditionTypeDrifted)
		ExpectApplied(ctx, env.Client, nodeClaim, node, nodePool)

		// inform cluster state about nodes and nodeclaims
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node}, []*v1beta1.NodeClaim{nodeClaim})

		fakeClock.Step(10 * time.Minute)

		var wg sync.WaitGroup
		ExpectTriggerVerifyAction(&wg)
		ExpectSingletonReconciled(ctx, disruptionController)
		wg.Wait()

		ExpectMetricCounterValue(disruption.ActionsPerformedCounter, 1, map[string]string{
			"action": "delete",
			"method": "drift",
		})
		ExpectMetricCounterValue(disruption.NodesDisruptedCounter, 1, map[string]string{
			"nodepool": nodePool.Name,
			"action":   "delete",
			"method":   "drift",
		})
		ExpectMetricCounterValue(disruption.PodsDisruptedCounter, 0, map[string]string{
			"nodepool": nodePool.Name,
			"action":   "delete",
			"method":   "drift",
		})
	})
	It("should fire metrics for single node delete disruption", func() {
		nodeClaims, nodes := test.NodeClaimsAndNodes(2, v1beta1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1beta1.NodePoolLabelKey:     nodePool.Name,
					v1.LabelInstanceTypeStable:   leastExpensiveInstance.Name,
					v1beta1.CapacityTypeLabelKey: leastExpensiveOffering.Requirements.Get(v1beta1.CapacityTypeLabelKey).Any(),
					v1.LabelTopologyZone:         leastExpensiveOffering.Requirements.Get(v1.LabelTopologyZone).Any(),
				},
			},
			Status: v1beta1.NodeClaimStatus{
				Allocatable: map[v1.ResourceName]resource.Quantity{
					v1.ResourceCPU:  resource.MustParse("32"),
					v1.ResourcePods: resource.MustParse("100"),
				},
			},
		})
		pods := test.Pods(4, test.PodOptions{})

		nodeClaims[0].StatusConditions().SetTrue(v1beta1.ConditionTypeDrifted)

		ExpectApplied(ctx, env.Client, pods[0], pods[1], pods[2], pods[3], nodeClaims[0], nodes[0], nodeClaims[1], nodes[1], nodePool)

		// bind pods to nodes
		ExpectManualBinding(ctx, env.Client, pods[0], nodes[0])
		ExpectManualBinding(ctx, env.Client, pods[1], nodes[0])
		ExpectManualBinding(ctx, env.Client, pods[2], nodes[1])
		ExpectManualBinding(ctx, env.Client, pods[3], nodes[1])

		// inform cluster state about nodes and nodeclaims
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{nodes[0], nodes[1]}, []*v1beta1.NodeClaim{nodeClaims[0], nodeClaims[1]})

		fakeClock.Step(10 * time.Minute)

		var wg sync.WaitGroup
		ExpectTriggerVerifyAction(&wg)
		ExpectSingletonReconciled(ctx, disruptionController)
		wg.Wait()

		ExpectMetricCounterValue(disruption.ActionsPerformedCounter, 1, map[string]string{
			"action": "delete",
			"method": "drift",
		})
		ExpectMetricCounterValue(disruption.NodesDisruptedCounter, 1, map[string]string{
			"nodepool": nodePool.Name,
			"action":   "delete",
			"method":   "drift",
		})
		ExpectMetricCounterValue(disruption.PodsDisruptedCounter, 2, map[string]string{
			"nodepool": nodePool.Name,
			"action":   "delete",
			"method":   "drift",
		})
	})
	It("should fire metrics for single node replace disruption", func() {
		nodeClaim, node := test.NodeClaimAndNode(v1beta1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1beta1.NodePoolLabelKey:     nodePool.Name,
					v1.LabelInstanceTypeStable:   mostExpensiveInstance.Name,
					v1beta1.CapacityTypeLabelKey: mostExpensiveOffering.Requirements.Get(v1beta1.CapacityTypeLabelKey).Any(),
					v1.LabelTopologyZone:         mostExpensiveOffering.Requirements.Get(v1.LabelTopologyZone).Any(),
				},
			},
			Status: v1beta1.NodeClaimStatus{
				ProviderID: test.RandomProviderID(),
				Allocatable: map[v1.ResourceName]resource.Quantity{
					v1.ResourceCPU:  resource.MustParse("32"),
					v1.ResourcePods: resource.MustParse("100"),
				},
			},
		})
		pods := test.Pods(4, test.PodOptions{})
		nodeClaim.StatusConditions().SetTrue(v1beta1.ConditionTypeDrifted)

		ExpectApplied(ctx, env.Client, pods[0], pods[1], pods[2], pods[3], nodeClaim, node, nodePool)

		// bind pods to nodes
		ExpectManualBinding(ctx, env.Client, pods[0], node)
		ExpectManualBinding(ctx, env.Client, pods[1], node)
		ExpectManualBinding(ctx, env.Client, pods[2], node)
		ExpectManualBinding(ctx, env.Client, pods[3], node)

		// inform cluster state about nodes and nodeclaims
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{node}, []*v1beta1.NodeClaim{nodeClaim})

		fakeClock.Step(10 * time.Minute)

		var wg sync.WaitGroup
		ExpectTriggerVerifyAction(&wg)
		ExpectSingletonReconciled(ctx, disruptionController)
		wg.Wait()

		ExpectMetricCounterValue(disruption.ActionsPerformedCounter, 1, map[string]string{
			"action": "replace",
			"method": "drift",
		})
		ExpectMetricCounterValue(disruption.NodesDisruptedCounter, 1, map[string]string{
			"nodepool": nodePool.Name,
			"action":   "replace",
			"method":   "drift",
		})
		ExpectMetricCounterValue(disruption.PodsDisruptedCounter, 4, map[string]string{
			"nodepool": nodePool.Name,
			"action":   "replace",
			"method":   "drift",
		})
	})
	It("should fire metrics for multi-node empty disruption", func() {
		nodeClaims, nodes := test.NodeClaimsAndNodes(3, v1beta1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1beta1.NodePoolLabelKey:     nodePool.Name,
					v1.LabelInstanceTypeStable:   leastExpensiveInstance.Name,
					v1beta1.CapacityTypeLabelKey: leastExpensiveOffering.Requirements.Get(v1beta1.CapacityTypeLabelKey).Any(),
					v1.LabelTopologyZone:         leastExpensiveOffering.Requirements.Get(v1.LabelTopologyZone).Any(),
				},
			},
			Status: v1beta1.NodeClaimStatus{
				Allocatable: map[v1.ResourceName]resource.Quantity{
					v1.ResourceCPU:  resource.MustParse("32"),
					v1.ResourcePods: resource.MustParse("100"),
				},
			},
		})
		ExpectApplied(ctx, env.Client, nodeClaims[0], nodes[0], nodeClaims[1], nodes[1], nodeClaims[2], nodes[2], nodePool)

		// inform cluster state about nodes and nodeclaims
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{nodes[0], nodes[1], nodes[2]}, []*v1beta1.NodeClaim{nodeClaims[0], nodeClaims[1], nodeClaims[2]})

		fakeClock.Step(10 * time.Minute)

		var wg sync.WaitGroup
		ExpectTriggerVerifyAction(&wg)
		ExpectSingletonReconciled(ctx, disruptionController)
		wg.Wait()

		ExpectMetricCounterValue(disruption.ActionsPerformedCounter, 1, map[string]string{
			"action":             "delete",
			"method":             "consolidation",
			"consolidation_type": "empty",
		})
		ExpectMetricCounterValue(disruption.NodesDisruptedCounter, 3, map[string]string{
			"nodepool":           nodePool.Name,
			"action":             "delete",
			"method":             "consolidation",
			"consolidation_type": "empty",
		})
		ExpectMetricCounterValue(disruption.PodsDisruptedCounter, 0, map[string]string{
			"nodepool":           nodePool.Name,
			"action":             "delete",
			"method":             "consolidation",
			"consolidation_type": "empty",
		})
	})
	It("should fire metrics for multi-node delete disruption", func() {
		nodeClaims, nodes := test.NodeClaimsAndNodes(3, v1beta1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1beta1.NodePoolLabelKey:     nodePool.Name,
					v1.LabelInstanceTypeStable:   leastExpensiveInstance.Name,
					v1beta1.CapacityTypeLabelKey: leastExpensiveOffering.Requirements.Get(v1beta1.CapacityTypeLabelKey).Any(),
					v1.LabelTopologyZone:         leastExpensiveOffering.Requirements.Get(v1.LabelTopologyZone).Any(),
				},
			},
			Status: v1beta1.NodeClaimStatus{
				Allocatable: map[v1.ResourceName]resource.Quantity{
					v1.ResourceCPU:  resource.MustParse("32"),
					v1.ResourcePods: resource.MustParse("100"),
				},
			},
		})

		// create our RS so we can link a pod to it
		rs := test.ReplicaSet()
		ExpectApplied(ctx, env.Client, rs)
		pods := test.Pods(4, test.PodOptions{
			ObjectMeta: metav1.ObjectMeta{Labels: labels,
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion:         "apps/v1",
						Kind:               "ReplicaSet",
						Name:               rs.Name,
						UID:                rs.UID,
						Controller:         lo.ToPtr(true),
						BlockOwnerDeletion: lo.ToPtr(true),
					},
				},
			},
		})

		ExpectApplied(ctx, env.Client, rs, pods[0], pods[1], pods[2], pods[3], nodeClaims[0], nodes[0], nodeClaims[1], nodes[1], nodeClaims[2], nodes[2], nodePool)

		// bind pods to nodes
		ExpectManualBinding(ctx, env.Client, pods[0], nodes[0])
		ExpectManualBinding(ctx, env.Client, pods[1], nodes[1])
		ExpectManualBinding(ctx, env.Client, pods[2], nodes[2])

		// inform cluster state about nodes and nodeclaims
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{nodes[0], nodes[1], nodes[2]}, []*v1beta1.NodeClaim{nodeClaims[0], nodeClaims[1], nodeClaims[2]})

		fakeClock.Step(10 * time.Minute)

		var wg sync.WaitGroup
		ExpectTriggerVerifyAction(&wg)
		ExpectSingletonReconciled(ctx, disruptionController)
		wg.Wait()

		ExpectMetricCounterValue(disruption.ActionsPerformedCounter, 1, map[string]string{
			"action":             "delete",
			"method":             "consolidation",
			"consolidation_type": "multi",
		})
		ExpectMetricCounterValue(disruption.NodesDisruptedCounter, 2, map[string]string{
			"nodepool":           nodePool.Name,
			"action":             "delete",
			"method":             "consolidation",
			"consolidation_type": "multi",
		})
		ExpectMetricCounterValue(disruption.PodsDisruptedCounter, 2, map[string]string{
			"nodepool":           nodePool.Name,
			"action":             "delete",
			"method":             "consolidation",
			"consolidation_type": "multi",
		})
	})
	It("should fire metrics for multi-node replace disruption", func() {
		nodeClaims, nodes := test.NodeClaimsAndNodes(3, v1beta1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1beta1.NodePoolLabelKey:     nodePool.Name,
					v1.LabelInstanceTypeStable:   mostExpensiveInstance.Name,
					v1beta1.CapacityTypeLabelKey: mostExpensiveOffering.Requirements.Get(v1beta1.CapacityTypeLabelKey).Any(),
					v1.LabelTopologyZone:         mostExpensiveOffering.Requirements.Get(v1.LabelTopologyZone).Any(),
				},
			},
			Status: v1beta1.NodeClaimStatus{
				Allocatable: map[v1.ResourceName]resource.Quantity{
					v1.ResourceCPU:  resource.MustParse("32"),
					v1.ResourcePods: resource.MustParse("100"),
				},
			},
		})

		// create our RS so we can link a pod to it
		rs := test.ReplicaSet()
		ExpectApplied(ctx, env.Client, rs)
		pods := test.Pods(4, test.PodOptions{
			ObjectMeta: metav1.ObjectMeta{Labels: labels,
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion:         "apps/v1",
						Kind:               "ReplicaSet",
						Name:               rs.Name,
						UID:                rs.UID,
						Controller:         lo.ToPtr(true),
						BlockOwnerDeletion: lo.ToPtr(true),
					},
				},
			},
		})

		ExpectApplied(ctx, env.Client, rs, pods[0], pods[1], pods[2], pods[3], nodeClaims[0], nodes[0], nodeClaims[1], nodes[1], nodeClaims[2], nodes[2], nodePool)

		// bind pods to nodes
		ExpectManualBinding(ctx, env.Client, pods[0], nodes[0])
		ExpectManualBinding(ctx, env.Client, pods[1], nodes[1])
		ExpectManualBinding(ctx, env.Client, pods[2], nodes[2])
		ExpectManualBinding(ctx, env.Client, pods[3], nodes[2])

		// inform cluster state about nodes and nodeclaims
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, []*v1.Node{nodes[0], nodes[1], nodes[2]}, []*v1beta1.NodeClaim{nodeClaims[0], nodeClaims[1], nodeClaims[2]})

		fakeClock.Step(10 * time.Minute)

		var wg sync.WaitGroup
		ExpectTriggerVerifyAction(&wg)
		ExpectSingletonReconciled(ctx, disruptionController)
		wg.Wait()

		ExpectMetricCounterValue(disruption.ActionsPerformedCounter, 1, map[string]string{
			"action":             "replace",
			"method":             "consolidation",
			"consolidation_type": "multi",
		})
		ExpectMetricCounterValue(disruption.NodesDisruptedCounter, 3, map[string]string{
			"nodepool":           nodePool.Name,
			"action":             "replace",
			"method":             "consolidation",
			"consolidation_type": "multi",
		})
		ExpectMetricCounterValue(disruption.PodsDisruptedCounter, 4, map[string]string{
			"nodepool":           nodePool.Name,
			"action":             "replace",
			"method":             "consolidation",
			"consolidation_type": "multi",
		})
	})
})

func leastExpensiveInstanceWithZone(zone string) *cloudprovider.InstanceType {
	for _, elem := range onDemandInstances {
		if len(elem.Offerings.Compatible(scheduling.NewRequirements(scheduling.NewRequirement(v1.LabelTopologyZone, v1.NodeSelectorOpIn, zone)))) > 0 {
			return elem
		}
	}
	return onDemandInstances[len(onDemandInstances)-1]
}

func mostExpensiveInstanceWithZone(zone string) *cloudprovider.InstanceType {
	for i := len(onDemandInstances) - 1; i >= 0; i-- {
		elem := onDemandInstances[i]
		if len(elem.Offerings.Compatible(scheduling.NewRequirements(scheduling.NewRequirement(v1.LabelTopologyZone, v1.NodeSelectorOpIn, zone)))) > 0 {
			return elem
		}
	}
	return onDemandInstances[0]
}

//nolint:unparam
func fromInt(i int) *intstr.IntOrString {
	v := intstr.FromInt(i)
	return &v
}

// This continually polls the wait group to see if there
// is a timer waiting, incrementing the clock if not.
// If you're seeing goroutine timeouts on suite tests, it's possible
// another timer was added, or the computation required for a loop is taking more than
// 20 * 400 milliseconds = 8s to complete, potentially requiring an increase in the
// duration of the polling period.
func ExpectTriggerVerifyAction(wg *sync.WaitGroup) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			time.Sleep(400 * time.Millisecond)
			if fakeClock.HasWaiters() {
				break
			}
		}
		fakeClock.Step(45 * time.Second)
	}()
}

// ExpectTaintedNodeCount will assert the number of nodes and tainted nodes in the cluster and return the tainted nodes.
func ExpectTaintedNodeCount(ctx context.Context, c client.Client, numTainted int) []*v1.Node {
	GinkgoHelper()
	tainted := lo.Filter(ExpectNodes(ctx, c), func(n *v1.Node, _ int) bool {
		return lo.Contains(n.Spec.Taints, v1beta1.DisruptionNoScheduleTaint)
	})
	Expect(len(tainted)).To(Equal(numTainted))
	return tainted
}

// ExpectNewNodeClaimsDeleted simulates the nodeClaims being created and then removed, similar to what would happen
// during an ICE error on the created nodeClaim
func ExpectNewNodeClaimsDeleted(ctx context.Context, c client.Client, wg *sync.WaitGroup, numNewNodeClaims int) {
	GinkgoHelper()
	existingNodeClaims := ExpectNodeClaims(ctx, c)
	existingNodeClaimNames := sets.NewString(lo.Map(existingNodeClaims, func(nc *v1beta1.NodeClaim, _ int) string {
		return nc.Name
	})...)

	wg.Add(1)
	go func() {
		GinkgoHelper()
		nodeClaimsDeleted := 0
		ctx, cancel := context.WithTimeout(ctx, time.Second*30) // give up after 30s
		defer GinkgoRecover()
		defer wg.Done()
		defer cancel()
		for {
			select {
			case <-time.After(50 * time.Millisecond):
				nodeClaimList := &v1beta1.NodeClaimList{}
				if err := c.List(ctx, nodeClaimList); err != nil {
					continue
				}
				for i := range nodeClaimList.Items {
					m := &nodeClaimList.Items[i]
					if existingNodeClaimNames.Has(m.Name) {
						continue
					}
					Expect(client.IgnoreNotFound(c.Delete(ctx, m))).To(Succeed())
					nodeClaimsDeleted++
					if nodeClaimsDeleted == numNewNodeClaims {
						return
					}
				}
			case <-ctx.Done():
				Fail(fmt.Sprintf("waiting for nodeclaims to be deleted, %s", ctx.Err()))
			}
		}
	}()
}

func ExpectMakeNewNodeClaimsReady(ctx context.Context, c client.Client, wg *sync.WaitGroup, cluster *state.Cluster,
	cloudProvider cloudprovider.CloudProvider, numNewNodeClaims int) {
	GinkgoHelper()

	existingNodeClaims := ExpectNodeClaims(ctx, c)
	existingNodeClaimNames := sets.NewString(lo.Map(existingNodeClaims, func(nc *v1beta1.NodeClaim, _ int) string {
		return nc.Name
	})...)

	wg.Add(1)
	go func() {
		nodeClaimsMadeReady := 0
		ctx, cancel := context.WithTimeout(ctx, time.Second*10) // give up after 10s
		defer GinkgoRecover()
		defer wg.Done()
		defer cancel()
		for {
			select {
			case <-time.After(50 * time.Millisecond):
				nodeClaimList := &v1beta1.NodeClaimList{}
				if err := c.List(ctx, nodeClaimList); err != nil {
					continue
				}
				for i := range nodeClaimList.Items {
					nc := &nodeClaimList.Items[i]
					if existingNodeClaimNames.Has(nc.Name) {
						continue
					}
					nc, n := ExpectNodeClaimDeployedAndStateUpdated(ctx, c, cluster, cloudProvider, nc)
					ExpectMakeNodeClaimsInitialized(ctx, c, nc)
					ExpectMakeNodesInitialized(ctx, c, n)

					nodeClaimsMadeReady++
					existingNodeClaimNames.Insert(nc.Name)
					// did we make all the nodes ready that we expected?
					if nodeClaimsMadeReady == numNewNodeClaims {
						return
					}
				}
			case <-ctx.Done():
				Fail(fmt.Sprintf("waiting for nodeclaims to be ready, %s", ctx.Err()))
			}
		}
	}()
}
