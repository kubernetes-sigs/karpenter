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

package deprovisioning_test

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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/sets"
	clock "k8s.io/utils/clock/testing"
	. "knative.dev/pkg/logging/testing"
	"knative.dev/pkg/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/aws/karpenter-core/pkg/apis"
	"github.com/aws/karpenter-core/pkg/apis/settings"
	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/apis/v1beta1"
	"github.com/aws/karpenter-core/pkg/cloudprovider"
	"github.com/aws/karpenter-core/pkg/cloudprovider/fake"
	"github.com/aws/karpenter-core/pkg/controllers/deprovisioning"
	"github.com/aws/karpenter-core/pkg/controllers/provisioning"
	"github.com/aws/karpenter-core/pkg/controllers/state"
	"github.com/aws/karpenter-core/pkg/controllers/state/informer"
	"github.com/aws/karpenter-core/pkg/operator/controller"
	"github.com/aws/karpenter-core/pkg/operator/scheme"
	"github.com/aws/karpenter-core/pkg/scheduling"
	"github.com/aws/karpenter-core/pkg/test"
	. "github.com/aws/karpenter-core/pkg/test/expectations"
)

var ctx context.Context
var env *test.Environment
var cluster *state.Cluster
var deprovisioningController *deprovisioning.Controller
var prov *provisioning.Provisioner
var cloudProvider *fake.CloudProvider
var nodeStateController controller.Controller
var machineStateController controller.Controller
var nodeClaimStateController controller.Controller
var fakeClock *clock.FakeClock
var recorder *test.EventRecorder

var onDemandInstances []*cloudprovider.InstanceType
var leastExpensiveInstance, mostExpensiveInstance *cloudprovider.InstanceType
var leastExpensiveOffering, mostExpensiveOffering cloudprovider.Offering

func TestAPIs(t *testing.T) {
	ctx = TestContextWithLogger(t)
	RegisterFailHandler(Fail)
	RunSpecs(t, "Disruption")
}

var _ = BeforeSuite(func() {
	env = test.NewEnvironment(scheme.Scheme, test.WithCRDs(apis.CRDs...))
	ctx = settings.ToContext(ctx, test.Settings(settings.Settings{DriftEnabled: true}))
	cloudProvider = fake.NewCloudProvider()
	fakeClock = clock.NewFakeClock(time.Now())
	cluster = state.NewCluster(fakeClock, env.Client, cloudProvider)
	nodeStateController = informer.NewNodeController(env.Client, cluster)
	machineStateController = informer.NewMachineController(env.Client, cluster)
	nodeClaimStateController = informer.NewNodeClaimController(env.Client, cluster)
	recorder = test.NewEventRecorder()
	prov = provisioning.NewProvisioner(env.Client, env.KubernetesInterface.CoreV1(), recorder, cloudProvider, cluster)
	deprovisioningController = deprovisioning.NewController(fakeClock, env.Client, prov, cloudProvider, recorder, cluster)
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
	ctx = settings.ToContext(ctx, test.Settings(settings.Settings{DriftEnabled: true}))

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

var _ = Describe("Pod Eviction Cost", func() {
	const standardPodCost = 1.0
	It("should have a standard disruptionCost for a pod with no priority or disruptionCost specified", func() {
		cost := deprovisioning.GetPodEvictionCost(ctx, &v1.Pod{})
		Expect(cost).To(BeNumerically("==", standardPodCost))
	})
	It("should have a higher disruptionCost for a pod with a positive deletion disruptionCost", func() {
		cost := deprovisioning.GetPodEvictionCost(ctx, &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
				v1.PodDeletionCost: "100",
			}},
		})
		Expect(cost).To(BeNumerically(">", standardPodCost))
	})
	It("should have a lower disruptionCost for a pod with a positive deletion disruptionCost", func() {
		cost := deprovisioning.GetPodEvictionCost(ctx, &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
				v1.PodDeletionCost: "-100",
			}},
		})
		Expect(cost).To(BeNumerically("<", standardPodCost))
	})
	It("should have higher costs for higher deletion costs", func() {
		cost1 := deprovisioning.GetPodEvictionCost(ctx, &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
				v1.PodDeletionCost: "101",
			}},
		})
		cost2 := deprovisioning.GetPodEvictionCost(ctx, &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
				v1.PodDeletionCost: "100",
			}},
		})
		cost3 := deprovisioning.GetPodEvictionCost(ctx, &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
				v1.PodDeletionCost: "99",
			}},
		})
		Expect(cost1).To(BeNumerically(">", cost2))
		Expect(cost2).To(BeNumerically(">", cost3))
	})
	It("should have a higher disruptionCost for a pod with a higher priority", func() {
		cost := deprovisioning.GetPodEvictionCost(ctx, &v1.Pod{
			Spec: v1.PodSpec{Priority: ptr.Int32(1)},
		})
		Expect(cost).To(BeNumerically(">", standardPodCost))
	})
	It("should have a lower disruptionCost for a pod with a lower priority", func() {
		cost := deprovisioning.GetPodEvictionCost(ctx, &v1.Pod{
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

// ExpectNewMachinesDeleted simulates the machines being created and then removed, similar to what would happen
// during an ICE error on the created machine
func ExpectNewMachinesDeleted(ctx context.Context, c client.Client, wg *sync.WaitGroup, numNewMachines int) {
	existingMachines := ExpectMachines(ctx, c)
	existingMachineNames := sets.NewString(lo.Map(existingMachines, func(m *v1alpha5.Machine, _ int) string {
		return m.Name
	})...)

	wg.Add(1)
	go func() {
		machinesDeleted := 0
		ctx, cancel := context.WithTimeout(ctx, time.Second*30) // give up after 30s
		defer GinkgoRecover()
		defer wg.Done()
		defer cancel()
		for {
			select {
			case <-time.After(50 * time.Millisecond):
				machineList := &v1alpha5.MachineList{}
				if err := c.List(ctx, machineList); err != nil {
					continue
				}
				for i := range machineList.Items {
					m := &machineList.Items[i]
					if existingMachineNames.Has(m.Name) {
						continue
					}
					ExpectWithOffset(1, client.IgnoreNotFound(c.Delete(ctx, m))).To(Succeed())
					machinesDeleted++
					if machinesDeleted == numNewMachines {
						return
					}
				}
			case <-ctx.Done():
				Fail(fmt.Sprintf("waiting for machines to be deleted, %s", ctx.Err()))
			}
		}
	}()
}

func ExpectMakeNewMachinesReady(ctx context.Context, c client.Client, wg *sync.WaitGroup, cluster *state.Cluster,
	cloudProvider cloudprovider.CloudProvider, numNewMachines int) {
	GinkgoHelper()

	existingMachines := ExpectMachines(ctx, c)
	existingMachineNames := sets.NewString(lo.Map(existingMachines, func(m *v1alpha5.Machine, _ int) string {
		return m.Name
	})...)

	wg.Add(1)
	go func() {
		machinesMadeReady := 0
		ctx, cancel := context.WithTimeout(ctx, time.Second*10) // give up after 10s
		defer GinkgoRecover()
		defer wg.Done()
		defer cancel()
		for {
			select {
			case <-time.After(50 * time.Millisecond):
				machineList := &v1alpha5.MachineList{}
				if err := c.List(ctx, machineList); err != nil {
					continue
				}
				for i := range machineList.Items {
					m := &machineList.Items[i]
					if existingMachineNames.Has(m.Name) {
						continue
					}
					m, n := ExpectMachineDeployed(ctx, c, cluster, cloudProvider, m)
					ExpectMakeMachinesInitialized(ctx, c, m)
					ExpectMakeNodesInitialized(ctx, c, n)

					machinesMadeReady++
					existingMachineNames.Insert(m.Name)
					// did we make all the nodes ready that we expected?
					if machinesMadeReady == numNewMachines {
						return
					}
				}
			case <-ctx.Done():
				Fail(fmt.Sprintf("waiting for machines to be ready, %s", ctx.Err()))
			}
		}
	}()
}

// ExpectNewMachinesDeleted simulates the machines being created and then removed, similar to what would happen
// during an ICE error on the created machine
func ExpectNewNodeClaimsDeleted(ctx context.Context, c client.Client, wg *sync.WaitGroup, numNewNodeClaims int) {
	existingNodeClaims := ExpectNodeClaims(ctx, c)
	existingNodeClaimNames := sets.NewString(lo.Map(existingNodeClaims, func(nc *v1beta1.NodeClaim, _ int) string {
		return nc.Name
	})...)

	wg.Add(1)
	go func() {
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
					ExpectWithOffset(1, client.IgnoreNotFound(c.Delete(ctx, m))).To(Succeed())
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
