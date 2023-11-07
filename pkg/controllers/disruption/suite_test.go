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
	. "knative.dev/pkg/logging/testing"
	"knative.dev/pkg/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	coreapis "github.com/aws/karpenter-core/pkg/apis"
	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/apis/v1beta1"
	"github.com/aws/karpenter-core/pkg/cloudprovider"
	"github.com/aws/karpenter-core/pkg/cloudprovider/fake"
	"github.com/aws/karpenter-core/pkg/controllers/disruption"
	"github.com/aws/karpenter-core/pkg/controllers/disruption/orchestration"
	"github.com/aws/karpenter-core/pkg/controllers/provisioning"
	"github.com/aws/karpenter-core/pkg/controllers/state"
	"github.com/aws/karpenter-core/pkg/controllers/state/informer"
	"github.com/aws/karpenter-core/pkg/operator/controller"
	"github.com/aws/karpenter-core/pkg/operator/options"
	"github.com/aws/karpenter-core/pkg/operator/scheme"
	"github.com/aws/karpenter-core/pkg/scheduling"
	"github.com/aws/karpenter-core/pkg/test"
	. "github.com/aws/karpenter-core/pkg/test/expectations"
)

var ctx context.Context
var env *test.Environment
var cluster *state.Cluster
var disruptionController *disruption.Controller
var prov *provisioning.Provisioner
var cloudProvider *fake.CloudProvider
var nodeStateController controller.Controller
var nodeClaimStateController controller.Controller
var fakeClock *clock.FakeClock
var recorder *test.EventRecorder
var queue *orchestration.Queue

var onDemandInstances []*cloudprovider.InstanceType
var leastExpensiveInstance, mostExpensiveInstance *cloudprovider.InstanceType
var leastExpensiveOffering, mostExpensiveOffering cloudprovider.Offering

func TestAPIs(t *testing.T) {
	ctx = TestContextWithLogger(t)
	RegisterFailHandler(Fail)
	RunSpecs(t, "Disruption")
}

var _ = BeforeSuite(func() {
	env = test.NewEnvironment(scheme.Scheme, test.WithCRDs(coreapis.CRDs...))
	ctx = options.ToContext(ctx, test.Options(test.OptionsFields{FeatureGates: test.FeatureGates{Drift: lo.ToPtr(true)}}))
	cloudProvider = fake.NewCloudProvider()
	fakeClock = clock.NewFakeClock(time.Now())
	cluster = state.NewCluster(fakeClock, env.Client, cloudProvider)
	nodeStateController = informer.NewNodeController(env.Client, cluster)
	nodeClaimStateController = informer.NewNodeClaimController(env.Client, cluster)
	recorder = test.NewEventRecorder()
	prov = provisioning.NewProvisioner(env.Client, env.KubernetesInterface.CoreV1(), recorder, cloudProvider, cluster)
	queue = orchestration.NewQueue(env.Client, recorder, cluster, fakeClock, prov)
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
	cluster.MarkUnconsolidated()

	// Reset Feature Flags to test defaults
	ctx = options.ToContext(ctx, test.Options(test.OptionsFields{FeatureGates: test.FeatureGates{Drift: lo.ToPtr(true)}}))

	onDemandInstances = lo.Filter(cloudProvider.InstanceTypes, func(i *cloudprovider.InstanceType, _ int) bool {
		for _, o := range i.Offerings.Available() {
			if o.CapacityType == v1alpha5.CapacityTypeOnDemand {
				return true
			}
		}
		return false
	})
	// Sort the instances by pricing from low to high
	sort.Slice(onDemandInstances, func(i, j int) bool {
		return onDemandInstances[i].Offerings.Cheapest().Price < onDemandInstances[j].Offerings.Cheapest().Price
	})
	leastExpensiveInstance, mostExpensiveInstance = onDemandInstances[0], onDemandInstances[len(onDemandInstances)-1]
	leastExpensiveOffering, mostExpensiveOffering = leastExpensiveInstance.Offerings[0], mostExpensiveInstance.Offerings[0]
})

var _ = AfterEach(func() {
	ExpectCleanedUp(ctx, env.Client)
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
					CapacityType: v1alpha5.CapacityTypeOnDemand,
					Zone:         "test-zone-1a",
					Price:        1.5,
					Available:    false,
				},
			},
		})
		replacementInstance := fake.NewInstanceType(fake.InstanceTypeOptions{
			Name: "spot-replacement",
			Offerings: []cloudprovider.Offering{
				{
					CapacityType: v1alpha5.CapacityTypeSpot,
					Zone:         "test-zone-1a",
					Price:        1.0,
					Available:    true,
				},
				{
					CapacityType: v1alpha5.CapacityTypeSpot,
					Zone:         "test-zone-1b",
					Price:        0.2,
					Available:    true,
				},
				{
					CapacityType: v1alpha5.CapacityTypeSpot,
					Zone:         "test-zone-1c",
					Price:        0.4,
					Available:    true,
				},
			},
		})
		nodePool = test.NodePool()
		nodeClaim, node = test.NodeClaimAndNode(v1beta1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1.LabelInstanceTypeStable: currentInstance.Name,
					v1alpha5.LabelCapacityType: currentInstance.Offerings[0].CapacityType,
					v1.LabelTopologyZone:       currentInstance.Offerings[0].Zone,
					v1beta1.NodePoolLabelKey:   nodePool.Name,
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
			ExpectReconcileSucceeded(ctx, disruptionController, client.ObjectKey{})
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
			ExpectReconcileFailed(ctx, disruptionController, client.ObjectKey{})
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
		node = ExpectNodeExists(ctx, env.Client, node.Name)
		Expect(node.Spec.Taints).ToNot(ContainElement(v1beta1.DisruptionNoScheduleTaint))
	})
})

var _ = Describe("Pod Eviction Cost", func() {
	const standardPodCost = 1.0
	It("should have a standard disruptionCost for a pod with no priority or disruptionCost specified", func() {
		cost := disruption.GetPodEvictionCost(ctx, &v1.Pod{})
		Expect(cost).To(BeNumerically("==", standardPodCost))
	})
	It("should have a higher disruptionCost for a pod with a positive deletion disruptionCost", func() {
		cost := disruption.GetPodEvictionCost(ctx, &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
				v1.PodDeletionCost: "100",
			}},
		})
		Expect(cost).To(BeNumerically(">", standardPodCost))
	})
	It("should have a lower disruptionCost for a pod with a positive deletion disruptionCost", func() {
		cost := disruption.GetPodEvictionCost(ctx, &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
				v1.PodDeletionCost: "-100",
			}},
		})
		Expect(cost).To(BeNumerically("<", standardPodCost))
	})
	It("should have higher costs for higher deletion costs", func() {
		cost1 := disruption.GetPodEvictionCost(ctx, &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
				v1.PodDeletionCost: "101",
			}},
		})
		cost2 := disruption.GetPodEvictionCost(ctx, &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
				v1.PodDeletionCost: "100",
			}},
		})
		cost3 := disruption.GetPodEvictionCost(ctx, &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
				v1.PodDeletionCost: "99",
			}},
		})
		Expect(cost1).To(BeNumerically(">", cost2))
		Expect(cost2).To(BeNumerically(">", cost3))
	})
	It("should have a higher disruptionCost for a pod with a higher priority", func() {
		cost := disruption.GetPodEvictionCost(ctx, &v1.Pod{
			Spec: v1.PodSpec{Priority: ptr.Int32(1)},
		})
		Expect(cost).To(BeNumerically(">", standardPodCost))
	})
	It("should have a lower disruptionCost for a pod with a lower priority", func() {
		cost := disruption.GetPodEvictionCost(ctx, &v1.Pod{
			Spec: v1.PodSpec{Priority: ptr.Int32(-1)},
		})
		Expect(cost).To(BeNumerically("<", standardPodCost))
	})
})

func leastExpensiveInstanceWithZone(zone string) *cloudprovider.InstanceType {
	for _, elem := range onDemandInstances {
		if len(elem.Offerings.Requirements(scheduling.NewRequirements(scheduling.NewRequirement(v1.LabelTopologyZone, v1.NodeSelectorOpIn, zone)))) > 0 {
			return elem
		}
	}
	return onDemandInstances[len(onDemandInstances)-1]
}

func mostExpensiveInstanceWithZone(zone string) *cloudprovider.InstanceType {
	for i := len(onDemandInstances) - 1; i >= 0; i-- {
		elem := onDemandInstances[i]
		if len(elem.Offerings.Requirements(scheduling.NewRequirements(scheduling.NewRequirement(v1.LabelTopologyZone, v1.NodeSelectorOpIn, zone)))) > 0 {
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

func ExpectTriggerVerifyAction(wg *sync.WaitGroup) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 10; i++ {
			time.Sleep(250 * time.Millisecond)
			if fakeClock.HasWaiters() {
				break
			}
		}
		fakeClock.Step(45 * time.Second)
	}()
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
					nc, n := ExpectNodeClaimDeployed(ctx, c, cluster, cloudProvider, nc)
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
