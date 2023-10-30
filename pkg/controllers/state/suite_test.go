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

package state_test

import (
	"context"
	"fmt"
	"math/rand"
	"net"
	"testing"
	"time"

	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	cloudproviderapi "k8s.io/cloud-provider/api"
	clock "k8s.io/utils/clock/testing"
	"knative.dev/pkg/ptr"

	"github.com/aws/karpenter-core/pkg/apis"
	"github.com/aws/karpenter-core/pkg/apis/settings"
	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/apis/v1beta1"
	corecloudprovider "github.com/aws/karpenter-core/pkg/cloudprovider"
	"github.com/aws/karpenter-core/pkg/cloudprovider/fake"
	"github.com/aws/karpenter-core/pkg/controllers/state/informer"
	"github.com/aws/karpenter-core/pkg/operator/controller"
	"github.com/aws/karpenter-core/pkg/operator/scheme"
	"github.com/aws/karpenter-core/pkg/scheduling"
	nodeclaimutil "github.com/aws/karpenter-core/pkg/utils/nodeclaim"
	nodepoolutil "github.com/aws/karpenter-core/pkg/utils/nodepool"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/aws/karpenter-core/pkg/controllers/state"
	"github.com/aws/karpenter-core/pkg/test"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	. "knative.dev/pkg/logging/testing"

	. "github.com/aws/karpenter-core/pkg/test/expectations"
)

var ctx context.Context
var env *test.Environment
var fakeClock *clock.FakeClock
var cluster *state.Cluster
var nodeClaimController controller.Controller
var machineController controller.Controller
var nodeController controller.Controller
var podController controller.Controller
var provisionerController controller.Controller
var nodePoolController controller.Controller
var daemonsetController controller.Controller
var cloudProvider *fake.CloudProvider
var provisioner *v1alpha5.Provisioner
var nodePool *v1beta1.NodePool

const csiProvider = "fake.csi.provider"

func TestAPIs(t *testing.T) {
	ctx = TestContextWithLogger(t)
	RegisterFailHandler(Fail)
	RunSpecs(t, "Controllers/State")
}

var _ = BeforeSuite(func() {
	env = test.NewEnvironment(scheme.Scheme, test.WithCRDs(apis.CRDs...))
	ctx = settings.ToContext(ctx, test.Settings())
	cloudProvider = fake.NewCloudProvider()
	fakeClock = clock.NewFakeClock(time.Now())
	cluster = state.NewCluster(fakeClock, env.Client, cloudProvider)
	nodeClaimController = informer.NewNodeClaimController(env.Client, cluster)
	machineController = informer.NewMachineController(env.Client, cluster)
	nodeController = informer.NewNodeController(env.Client, cluster)
	podController = informer.NewPodController(env.Client, cluster)
	provisionerController = informer.NewProvisionerController(env.Client, cluster)
	nodePoolController = informer.NewNodePoolController(env.Client, cluster)
	daemonsetController = informer.NewDaemonSetController(env.Client, cluster)
})

var _ = AfterSuite(func() {
	Expect(env.Stop()).To(Succeed(), "Failed to stop environment")
})

var _ = BeforeEach(func() {
	fakeClock.SetTime(time.Now())
	cloudProvider.InstanceTypes = fake.InstanceTypesAssorted()
	provisioner = test.Provisioner(test.ProvisionerOptions{ObjectMeta: metav1.ObjectMeta{Name: "default"}})
	nodePool = test.NodePool(v1beta1.NodePool{ObjectMeta: metav1.ObjectMeta{Name: "default"}})
	nodepoolutil.EnableNodePools = true
	nodeclaimutil.EnableNodeClaims = true
	ExpectApplied(ctx, env.Client, provisioner, nodePool)
})
var _ = AfterEach(func() {
	ExpectCleanedUp(ctx, env.Client)
	cluster.Reset()
	cloudProvider.Reset()
})

var _ = Describe("Volume Usage/Limits", func() {
	var nodeClaim *v1beta1.NodeClaim
	var machine *v1alpha5.Machine
	var node1, node2 *v1.Node
	var csiNode1, csiNode2 *storagev1.CSINode
	var sc *storagev1.StorageClass
	BeforeEach(func() {
		instanceType := cloudProvider.InstanceTypes[0]
		machine, node1 = test.MachineAndNode(v1alpha5.Machine{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
				v1alpha5.ProvisionerNameLabelKey: provisioner.Name,
				v1.LabelInstanceTypeStable:       instanceType.Name,
			}},
			Status: v1alpha5.MachineStatus{
				ProviderID: test.RandomProviderID(),
			},
		})
		nodeClaim, node2 = test.NodeClaimAndNode(v1beta1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
				v1beta1.NodePoolLabelKey:   nodePool.Name,
				v1.LabelInstanceTypeStable: instanceType.Name,
			}},
			Status: v1beta1.NodeClaimStatus{
				ProviderID: test.RandomProviderID(),
			},
		})
		sc = test.StorageClass(test.StorageClassOptions{
			ObjectMeta:  metav1.ObjectMeta{Name: "my-storage-class"},
			Provisioner: ptr.String(csiProvider),
			Zones:       []string{"test-zone-1"},
		})
		csiNode1 = &storagev1.CSINode{
			ObjectMeta: metav1.ObjectMeta{
				Name: node1.Name,
			},
			Spec: storagev1.CSINodeSpec{
				Drivers: []storagev1.CSINodeDriver{
					{
						Name:   csiProvider,
						NodeID: "fake-node-id",
						Allocatable: &storagev1.VolumeNodeResources{
							Count: ptr.Int32(10),
						},
					},
				},
			},
		}
		csiNode2 = &storagev1.CSINode{
			ObjectMeta: metav1.ObjectMeta{
				Name: node2.Name,
			},
			Spec: storagev1.CSINodeSpec{
				Drivers: []storagev1.CSINodeDriver{
					{
						Name:   csiProvider,
						NodeID: "fake-node-id",
						Allocatable: &storagev1.VolumeNodeResources{
							Count: ptr.Int32(10),
						},
					},
				},
			},
		}
	})
	It("should hydrate the volume usage on a Node update", func() {
		ExpectApplied(ctx, env.Client, sc, node1, csiNode1)
		for i := 0; i < 10; i++ {
			pvc := test.PersistentVolumeClaim(test.PersistentVolumeClaimOptions{
				StorageClassName: ptr.String(sc.Name),
			})
			pod := test.Pod(test.PodOptions{
				PersistentVolumeClaims: []string{pvc.Name},
			})
			ExpectApplied(ctx, env.Client, pvc, pod)
			ExpectManualBinding(ctx, env.Client, pod, node1)
		}
		ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node1))
		ExpectStateNodeCount("==", 1)
		stateNode := ExpectStateNodeExists(node1)

		// Adding more volumes should cause an error since we are at the volume limits
		Expect(stateNode.VolumeUsage().ExceedsLimits(scheduling.Volumes{
			csiProvider: sets.New("test"),
		})).ToNot(BeNil())
	})
	Context("Machine", func() {
		It("should maintain the volume usage state when receiving Machine updates", func() {
			ExpectApplied(ctx, env.Client, sc, machine, node1, csiNode1)
			for i := 0; i < 10; i++ {
				pvc := test.PersistentVolumeClaim(test.PersistentVolumeClaimOptions{
					StorageClassName: ptr.String(sc.Name),
				})
				pod := test.Pod(test.PodOptions{
					PersistentVolumeClaims: []string{pvc.Name},
				})
				ExpectApplied(ctx, env.Client, pvc, pod)
				ExpectManualBinding(ctx, env.Client, pod, node1)
			}
			ExpectReconcileSucceeded(ctx, machineController, client.ObjectKeyFromObject(machine))
			ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node1))
			ExpectStateNodeCount("==", 1)
			stateNode := ExpectStateNodeExists(node1)

			// Adding more volumes should cause an error since we are at the volume limits
			Expect(stateNode.VolumeUsage().ExceedsLimits(scheduling.Volumes{
				csiProvider: sets.New("test"),
			})).ToNot(BeNil())

			// Reconcile the machine one more time to ensure that we maintain our volume usage state
			ExpectReconcileSucceeded(ctx, machineController, client.ObjectKeyFromObject(machine))

			// Ensure that we still consider adding another volume to the node breaching our volume limits
			Expect(stateNode.VolumeUsage().ExceedsLimits(scheduling.Volumes{
				csiProvider: sets.New("test"),
			})).ToNot(BeNil())
		})
		It("should ignore the volume usage limits breach if the pod update is for an already tracked pod", func() {
			ExpectApplied(ctx, env.Client, sc, machine, node1, csiNode1)
			var pvcs []*v1.PersistentVolumeClaim
			for i := 0; i < 10; i++ {
				pvc := test.PersistentVolumeClaim(test.PersistentVolumeClaimOptions{
					StorageClassName: ptr.String(sc.Name),
				})
				pod := test.Pod(test.PodOptions{
					PersistentVolumeClaims: []string{pvc.Name},
				})
				pvcs = append(pvcs, pvc)
				ExpectApplied(ctx, env.Client, pvc, pod)
				ExpectManualBinding(ctx, env.Client, pod, node1)
			}
			ExpectReconcileSucceeded(ctx, machineController, client.ObjectKeyFromObject(machine))
			ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node1))
			ExpectStateNodeCount("==", 1)
			stateNode := ExpectStateNodeExists(node1)

			// Adding more volumes should not cause an error since this PVC volume is already tracked
			Expect(stateNode.VolumeUsage().ExceedsLimits(scheduling.Volumes{
				csiProvider: sets.New(client.ObjectKeyFromObject(pvcs[5]).String()),
			})).To(BeNil())
		})

	})
	Context("NodeClaim", func() {
		It("should maintain the volume usage state when receiving NodeClaim updates", func() {
			ExpectApplied(ctx, env.Client, sc, nodeClaim, node2, csiNode2)
			for i := 0; i < 10; i++ {
				pvc := test.PersistentVolumeClaim(test.PersistentVolumeClaimOptions{
					StorageClassName: ptr.String(sc.Name),
				})
				pod := test.Pod(test.PodOptions{
					PersistentVolumeClaims: []string{pvc.Name},
				})
				ExpectApplied(ctx, env.Client, pvc, pod)
				ExpectManualBinding(ctx, env.Client, pod, node2)
			}
			ExpectReconcileSucceeded(ctx, nodeClaimController, client.ObjectKeyFromObject(nodeClaim))
			ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node2))
			ExpectStateNodeCount("==", 1)
			stateNode := ExpectStateNodeExists(node2)

			// Adding more volumes should cause an error since we are at the volume limits
			Expect(stateNode.VolumeUsage().ExceedsLimits(scheduling.Volumes{
				csiProvider: sets.New("test"),
			})).ToNot(BeNil())

			// Reconcile the machine one more time to ensure that we maintain our volume usage state
			ExpectReconcileSucceeded(ctx, nodeClaimController, client.ObjectKeyFromObject(nodeClaim))

			// Ensure that we still consider adding another volume to the node breaching our volume limits
			Expect(stateNode.VolumeUsage().ExceedsLimits(scheduling.Volumes{
				csiProvider: sets.New("test"),
			})).ToNot(BeNil())
		})
		It("should ignore the volume usage limits breach if the pod update is for an already tracked pod", func() {
			ExpectApplied(ctx, env.Client, sc, nodeClaim, node2, csiNode2)
			var pvcs []*v1.PersistentVolumeClaim
			for i := 0; i < 10; i++ {
				pvc := test.PersistentVolumeClaim(test.PersistentVolumeClaimOptions{
					StorageClassName: ptr.String(sc.Name),
				})
				pod := test.Pod(test.PodOptions{
					PersistentVolumeClaims: []string{pvc.Name},
				})
				pvcs = append(pvcs, pvc)
				ExpectApplied(ctx, env.Client, pvc, pod)
				ExpectManualBinding(ctx, env.Client, pod, node2)
			}
			ExpectReconcileSucceeded(ctx, nodeClaimController, client.ObjectKeyFromObject(nodeClaim))
			ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node2))
			ExpectStateNodeCount("==", 1)
			stateNode := ExpectStateNodeExists(node2)

			// Adding more volumes should not cause an error since this PVC volume is already tracked
			Expect(stateNode.VolumeUsage().ExceedsLimits(scheduling.Volumes{
				csiProvider: sets.New(client.ObjectKeyFromObject(pvcs[5]).String()),
			})).To(BeNil())
		})
	})
})

var _ = Describe("HostPort Usage", func() {
	var nodeClaim *v1beta1.NodeClaim
	var machine *v1alpha5.Machine
	var node1, node2 *v1.Node
	BeforeEach(func() {
		instanceType := cloudProvider.InstanceTypes[0]
		machine, node1 = test.MachineAndNode(v1alpha5.Machine{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
				v1alpha5.ProvisionerNameLabelKey: provisioner.Name,
				v1.LabelInstanceTypeStable:       instanceType.Name,
			}},
			Status: v1alpha5.MachineStatus{
				ProviderID: test.RandomProviderID(),
			},
		})
		nodeClaim, node2 = test.NodeClaimAndNode(v1beta1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
				v1beta1.NodePoolLabelKey:   nodePool.Name,
				v1.LabelInstanceTypeStable: instanceType.Name,
			}},
			Status: v1beta1.NodeClaimStatus{
				ProviderID: test.RandomProviderID(),
			},
		})
	})
	It("should hydrate the HostPort usage on a Node update", func() {
		ExpectApplied(ctx, env.Client, node1)
		ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node1))
		for i := 0; i < 10; i++ {
			pod := test.Pod(test.PodOptions{
				HostPorts: []int32{int32(i)},
			})
			ExpectApplied(ctx, env.Client, pod)
			ExpectManualBinding(ctx, env.Client, pod, node1)
		}
		ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node1))
		ExpectStateNodeCount("==", 1)
		stateNode := ExpectStateNodeExists(node1)

		// Adding a conflicting host port should cause an error
		Expect(stateNode.HostPortUsage().Conflicts(test.Pod(), []scheduling.HostPort{
			{
				IP:       net.IP("0.0.0.0"),
				Port:     int32(5),
				Protocol: v1.ProtocolTCP,
			},
		})).ToNot(BeNil())
	})
	Context("Machine", func() {
		It("should maintain the host port usage state when receiving Machine updates", func() {
			ExpectApplied(ctx, env.Client, node1)
			ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node1))
			for i := 0; i < 10; i++ {
				pod := test.Pod(test.PodOptions{
					HostPorts: []int32{int32(i)},
				})
				ExpectApplied(ctx, env.Client, pod)
				ExpectManualBinding(ctx, env.Client, pod, node1)
			}
			ExpectReconcileSucceeded(ctx, machineController, client.ObjectKeyFromObject(machine))
			ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node1))
			ExpectStateNodeCount("==", 1)
			stateNode := ExpectStateNodeExists(node1)

			// Adding a conflicting host port should cause an error
			Expect(stateNode.HostPortUsage().Conflicts(test.Pod(), []scheduling.HostPort{
				{
					IP:       net.IP("0.0.0.0"),
					Port:     int32(5),
					Protocol: v1.ProtocolTCP,
				},
			})).ToNot(BeNil())

			// Reconcile the machine one more time to ensure that we maintain our volume usage state
			ExpectReconcileSucceeded(ctx, machineController, client.ObjectKeyFromObject(machine))

			// Ensure that we still consider the host port usage addition an error
			Expect(stateNode.HostPortUsage().Conflicts(test.Pod(), []scheduling.HostPort{
				{
					IP:       net.IP("0.0.0.0"),
					Port:     int32(5),
					Protocol: v1.ProtocolTCP,
				},
			})).ToNot(BeNil())
		})
		It("should ignore the host port usage conflict if the pod update is for an already tracked pod", func() {
			ExpectApplied(ctx, env.Client, node1)
			ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node1))
			var pods []*v1.Pod
			for i := 0; i < 10; i++ {
				pod := test.Pod(test.PodOptions{
					HostPorts: []int32{int32(i)},
				})
				pods = append(pods, pod)
				ExpectApplied(ctx, env.Client, pod)
				ExpectManualBinding(ctx, env.Client, pod, node1)
			}
			ExpectReconcileSucceeded(ctx, machineController, client.ObjectKeyFromObject(machine))
			ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node1))
			ExpectStateNodeCount("==", 1)
			stateNode := ExpectStateNodeExists(node1)

			// Adding a conflicting host port should not cause an error since this port is already tracked for the pod
			Expect(stateNode.HostPortUsage().Conflicts(pods[5], []scheduling.HostPort{
				{
					IP:       net.IP("0.0.0.0"),
					Port:     int32(5),
					Protocol: v1.ProtocolTCP,
				},
			})).To(BeNil())
		})
	})
	Context("NodeClaim", func() {
		It("should maintain the host port usage state when receiving NodeClaim updates", func() {
			ExpectApplied(ctx, env.Client, node2)
			ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node2))
			for i := 0; i < 10; i++ {
				pod := test.Pod(test.PodOptions{
					HostPorts: []int32{int32(i)},
				})
				ExpectApplied(ctx, env.Client, pod)
				ExpectManualBinding(ctx, env.Client, pod, node2)
			}
			ExpectReconcileSucceeded(ctx, nodeClaimController, client.ObjectKeyFromObject(nodeClaim))
			ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node2))
			ExpectStateNodeCount("==", 1)
			stateNode := ExpectStateNodeExists(node2)

			// Adding a conflicting host port should cause an error
			Expect(stateNode.HostPortUsage().Conflicts(test.Pod(), []scheduling.HostPort{
				{
					IP:       net.IP("0.0.0.0"),
					Port:     int32(5),
					Protocol: v1.ProtocolTCP,
				},
			})).ToNot(BeNil())

			// Reconcile the nodeclaim one more time to ensure that we maintain our volume usage state
			ExpectReconcileSucceeded(ctx, nodeClaimController, client.ObjectKeyFromObject(nodeClaim))

			// Ensure that we still consider the host port usage addition an error
			Expect(stateNode.HostPortUsage().Conflicts(test.Pod(), []scheduling.HostPort{
				{
					IP:       net.IP("0.0.0.0"),
					Port:     int32(5),
					Protocol: v1.ProtocolTCP,
				},
			})).ToNot(BeNil())
		})
		It("should ignore the host port usage conflict if the pod update is for an already tracked pod", func() {
			ExpectApplied(ctx, env.Client, node2)
			ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node2))
			var pods []*v1.Pod
			for i := 0; i < 10; i++ {
				pod := test.Pod(test.PodOptions{
					HostPorts: []int32{int32(i)},
				})
				pods = append(pods, pod)
				ExpectApplied(ctx, env.Client, pod)
				ExpectManualBinding(ctx, env.Client, pod, node2)
			}
			ExpectReconcileSucceeded(ctx, nodeClaimController, client.ObjectKeyFromObject(nodeClaim))
			ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node2))
			ExpectStateNodeCount("==", 1)
			stateNode := ExpectStateNodeExists(node2)

			// Adding a conflicting host port should not cause an error since this port is already tracked for the pod
			Expect(stateNode.HostPortUsage().Conflicts(pods[5], []scheduling.HostPort{
				{
					IP:       net.IP("0.0.0.0"),
					Port:     int32(5),
					Protocol: v1.ProtocolTCP,
				},
			})).To(BeNil())
		})
	})
})

var _ = Describe("Inflight Nodes", func() {
	It("should consider the node capacity/allocatable based on the instance type", func() {
		instanceType := cloudProvider.InstanceTypes[0]
		node := test.Node(test.NodeOptions{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
				v1alpha5.ProvisionerNameLabelKey: provisioner.Name,
				v1.LabelInstanceTypeStable:       instanceType.Name,
			}},
			ProviderID: test.RandomProviderID(),
		})
		ExpectApplied(ctx, env.Client, node)
		ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))

		ExpectStateNodeCount("==", 1)
		ExpectResources(instanceType.Allocatable(), ExpectStateNodeExists(node).Allocatable())
		ExpectResources(instanceType.Capacity, ExpectStateNodeExists(node).Capacity())
	})
	It("should populate inflight details once", func() {
		instanceType := cloudProvider.InstanceTypes[0]
		node := test.Node(test.NodeOptions{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
				v1alpha5.ProvisionerNameLabelKey: provisioner.Name,
				v1.LabelInstanceTypeStable:       instanceType.Name,
			}},
			ProviderID: test.RandomProviderID(),
		})
		ExpectApplied(ctx, env.Client, node)
		ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))

		ExpectStateNodeCount("==", 1)
		ExpectResources(instanceType.Allocatable(), ExpectStateNodeExists(node).Allocatable())
		ExpectResources(instanceType.Capacity, ExpectStateNodeExists(node).Capacity())

		cloudProvider.InstanceTypes = cloudProvider.InstanceTypes[1:]
		// Still expect to succeed w/o instance type since inflight details are set once
		ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))
	})
	It("should populate node from the cluster if we have no instance types", func() {
		cloudProvider.InstanceTypes = []*corecloudprovider.InstanceType{}
		node := test.Node(test.NodeOptions{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
				v1alpha5.ProvisionerNameLabelKey: provisioner.Name,
				v1beta1.NodeInitializedLabelKey:  "true",
			}},
			ProviderID: test.RandomProviderID(),
			Capacity: v1.ResourceList{
				v1.ResourceCPU:    resource.MustParse(fmt.Sprintf("%d", 64)),
				v1.ResourceMemory: resource.MustParse(fmt.Sprintf("%dGi", 128)),
			},
			Allocatable: v1.ResourceList{
				v1.ResourceCPU:    resource.MustParse(fmt.Sprintf("%d", 32)),
				v1.ResourceMemory: resource.MustParse(fmt.Sprintf("%dGi", 64)),
			},
		})
		ExpectApplied(ctx, env.Client, node)
		ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))

		ExpectStateNodeCount("==", 1)
		ExpectResources(node.Status.Allocatable, ExpectStateNodeExists(node).Allocatable())
		ExpectResources(node.Status.Capacity, ExpectStateNodeExists(node).Capacity())
	})
	It("should consider the node capacity/allocatable as a combination of instance type and current node", func() {
		instanceType := cloudProvider.InstanceTypes[0]
		node := test.Node(test.NodeOptions{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
				v1alpha5.ProvisionerNameLabelKey: provisioner.Name,
				v1.LabelInstanceTypeStable:       instanceType.Name,
			}},
			Allocatable: v1.ResourceList{
				v1.ResourceMemory: resource.MustParse("100Mi"),
			},
			Capacity: v1.ResourceList{
				v1.ResourceEphemeralStorage: resource.MustParse("100Gi"),
			},
			ProviderID: test.RandomProviderID(),
		})
		ExpectApplied(ctx, env.Client, node)
		ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))

		ExpectStateNodeCount("==", 1)
		ExpectResources(v1.ResourceList{
			v1.ResourceMemory:           resource.MustParse("100Mi"), // pulled from the node's real allocatable
			v1.ResourceCPU:              *instanceType.Capacity.Cpu(),
			v1.ResourceEphemeralStorage: *instanceType.Capacity.StorageEphemeral(),
		}, ExpectStateNodeExists(node).Allocatable())
		ExpectResources(v1.ResourceList{
			v1.ResourceMemory:           *instanceType.Capacity.Memory(),
			v1.ResourceCPU:              *instanceType.Capacity.Cpu(),
			v1.ResourceEphemeralStorage: resource.MustParse("100Gi"), // pulled from the node's real capacity
		}, ExpectStateNodeExists(node).Capacity())
	})
	It("should consider the node capacity/allocatable to be equal to the node when the node is initialized", func() {
		instanceType := cloudProvider.InstanceTypes[0]
		node := test.Node(test.NodeOptions{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
				v1alpha5.ProvisionerNameLabelKey: provisioner.Name,
				v1.LabelInstanceTypeStable:       instanceType.Name,
				v1beta1.NodeInitializedLabelKey:  "true",
			}},
			ProviderID: test.RandomProviderID(),
			Capacity: v1.ResourceList{
				v1.ResourceCPU:    resource.MustParse(fmt.Sprintf("%d", 64)),
				v1.ResourceMemory: resource.MustParse(fmt.Sprintf("%dGi", 128)),
			},
			Allocatable: v1.ResourceList{
				v1.ResourceCPU:    resource.MustParse(fmt.Sprintf("%d", 32)),
				v1.ResourceMemory: resource.MustParse(fmt.Sprintf("%dGi", 64)),
			},
		})
		ExpectApplied(ctx, env.Client, node)
		ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))

		ExpectStateNodeCount("==", 1)
		ExpectResources(node.Status.Allocatable, ExpectStateNodeExists(node).Allocatable())
		ExpectResources(node.Status.Capacity, ExpectStateNodeExists(node).Capacity())
	})
	It("should only set startup taints once", func() {
		taints := []v1.Taint{
			{
				Key:    "custom-taint1",
				Value:  "custom-value1",
				Effect: v1.TaintEffectNoSchedule,
			},
		}

		customProvisioner := test.Provisioner(test.ProvisionerOptions{
			ObjectMeta:    metav1.ObjectMeta{Name: "custom-provisioner"},
			StartupTaints: taints,
		})
		ExpectApplied(ctx, env.Client, customProvisioner)

		node := test.Node(test.NodeOptions{
			ObjectMeta: metav1.ObjectMeta{
				Name: "abcd",
				Labels: map[string]string{
					v1alpha5.ProvisionerNameLabelKey: customProvisioner.Name,
					v1.LabelInstanceTypeStable:       cloudProvider.InstanceTypes[0].Name,
				},
			},
			Taints:     taints,
			ProviderID: test.RandomProviderID(),
		})
		ExpectApplied(ctx, env.Client, node)
		ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))

		// Update the provisioner to the default suite provisioner w/ no StartupTaints. StartupTaints should still be
		// populated by custom provisioner.
		node.Labels[v1alpha5.ProvisionerNameLabelKey] = provisioner.Name
		ExpectApplied(ctx, env.Client, node)
		ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))

		ExpectStateNodeCount("==", 1)
		stateNode := ExpectStateNodeExists(node)
		Expect(stateNode.Taints()).To(HaveLen(0))
	})

	Context("Machine", func() {
		It("should ignore machines that don't yet have provider id", func() {
			machine := test.Machine(v1alpha5.Machine{
				Spec: v1alpha5.MachineSpec{
					Taints: []v1.Taint{
						{
							Key:    "custom-taint",
							Value:  "custom-value",
							Effect: v1.TaintEffectNoSchedule,
						},
					},
					Requirements: []v1.NodeSelectorRequirement{
						{
							Key:      v1.LabelInstanceTypeStable,
							Operator: v1.NodeSelectorOpIn,
							Values:   []string{cloudProvider.InstanceTypes[0].Name},
						},
						{
							Key:      v1.LabelTopologyZone,
							Operator: v1.NodeSelectorOpIn,
							Values:   []string{"test-zone-1"},
						},
					},
					Resources: v1alpha5.ResourceRequirements{
						Requests: v1.ResourceList{
							v1.ResourceCPU:              resource.MustParse("2"),
							v1.ResourceMemory:           resource.MustParse("2Gi"),
							v1.ResourceEphemeralStorage: resource.MustParse("1Gi"),
						},
					},
					MachineTemplateRef: &v1alpha5.MachineTemplateRef{
						Name: "default",
					},
				},
			})
			machine.Status.ProviderID = ""
			ExpectApplied(ctx, env.Client, machine)
			ExpectReconcileSucceeded(ctx, machineController, client.ObjectKeyFromObject(machine))
			ExpectStateNodeNotFoundForMachine(machine)
		})
		It("should ignore node updates if nodes don't have provider id and are owned", func() {
			node := test.Node(test.NodeOptions{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1alpha5.ProvisionerNameLabelKey: provisioner.Name,
					},
				},
				Capacity: v1.ResourceList{
					v1.ResourceCPU:              resource.MustParse("1800m"),
					v1.ResourceMemory:           resource.MustParse("0"), // Should use the inflight capacity for this value
					v1.ResourceEphemeralStorage: resource.MustParse("19000Mi"),
				},
				Allocatable: v1.ResourceList{
					v1.ResourceCPU:              resource.MustParse("0"), // Should use the inflight allocatable for this value
					v1.ResourceMemory:           resource.MustParse("29250Mi"),
					v1.ResourceEphemeralStorage: resource.MustParse("0"), // Should use the inflight allocatable for this value
				},
			})
			ExpectApplied(ctx, env.Client, node)
			ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))
			ExpectStateNodeCount("==", 0)
		})
		It("should model the inflight data as machine with no node", func() {
			machine := test.Machine(v1alpha5.Machine{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						v1alpha5.DoNotConsolidateNodeAnnotationKey: "true",
					},
					Labels: map[string]string{
						v1alpha5.ProvisionerNameLabelKey: "default",
						v1.LabelInstanceTypeStable:       cloudProvider.InstanceTypes[0].Name,
						v1.LabelTopologyZone:             "test-zone-1",
						v1.LabelTopologyRegion:           "test-region",
						v1.LabelHostname:                 "custom-host-name",
					},
				},
				Spec: v1alpha5.MachineSpec{
					Taints: []v1.Taint{
						{
							Key:    "custom-taint",
							Value:  "custom-value",
							Effect: v1.TaintEffectNoSchedule,
						},
					},
					Requirements: []v1.NodeSelectorRequirement{
						{
							Key:      v1.LabelInstanceTypeStable,
							Operator: v1.NodeSelectorOpIn,
							Values:   []string{cloudProvider.InstanceTypes[0].Name},
						},
						{
							Key:      v1.LabelTopologyZone,
							Operator: v1.NodeSelectorOpIn,
							Values:   []string{"test-zone-1"},
						},
					},
					Resources: v1alpha5.ResourceRequirements{
						Requests: v1.ResourceList{
							v1.ResourceCPU:              resource.MustParse("2"),
							v1.ResourceMemory:           resource.MustParse("2Gi"),
							v1.ResourceEphemeralStorage: resource.MustParse("1Gi"),
						},
					},
					MachineTemplateRef: &v1alpha5.MachineTemplateRef{
						Name: "default",
					},
				},
				Status: v1alpha5.MachineStatus{
					ProviderID: test.RandomProviderID(),
				},
			})
			ExpectApplied(ctx, env.Client, machine)
			ExpectReconcileSucceeded(ctx, machineController, client.ObjectKeyFromObject(machine))

			ExpectStateNodeCount("==", 1)
			stateNode := ExpectStateNodeExistsForMachine(machine)
			Expect(stateNode.Labels()).To(HaveKeyWithValue(v1alpha5.ProvisionerNameLabelKey, "default"))
			Expect(stateNode.Labels()).To(HaveKeyWithValue(v1.LabelInstanceTypeStable, cloudProvider.InstanceTypes[0].Name))
			Expect(stateNode.Labels()).To(HaveKeyWithValue(v1.LabelTopologyZone, "test-zone-1"))
			Expect(stateNode.Labels()).To(HaveKeyWithValue(v1.LabelTopologyRegion, "test-region"))
			Expect(stateNode.Labels()).To(HaveKeyWithValue(v1.LabelHostname, "custom-host-name"))
			Expect(stateNode.HostName()).To(Equal("custom-host-name"))
			Expect(stateNode.Annotations()).To(HaveKeyWithValue(v1alpha5.DoNotConsolidateNodeAnnotationKey, "true"))
			Expect(stateNode.Initialized()).To(BeFalse())
			Expect(stateNode.Managed()).To(BeTrue())
		})
		It("should model the inflight capacity/allocatable as the machine capacity/allocatable", func() {
			machine := test.Machine(v1alpha5.Machine{
				Spec: v1alpha5.MachineSpec{
					Requirements: []v1.NodeSelectorRequirement{
						{
							Key:      v1.LabelInstanceTypeStable,
							Operator: v1.NodeSelectorOpIn,
							Values:   []string{cloudProvider.InstanceTypes[0].Name},
						},
						{
							Key:      v1.LabelTopologyZone,
							Operator: v1.NodeSelectorOpIn,
							Values:   []string{"test-zone-1"},
						},
					},
					MachineTemplateRef: &v1alpha5.MachineTemplateRef{
						Name: "default",
					},
				},
				Status: v1alpha5.MachineStatus{
					ProviderID: test.RandomProviderID(),
					Capacity: v1.ResourceList{
						v1.ResourceCPU:              resource.MustParse("2"),
						v1.ResourceMemory:           resource.MustParse("32Gi"),
						v1.ResourceEphemeralStorage: resource.MustParse("20Gi"),
					},
					Allocatable: v1.ResourceList{
						v1.ResourceCPU:              resource.MustParse("1"),
						v1.ResourceMemory:           resource.MustParse("30Gi"),
						v1.ResourceEphemeralStorage: resource.MustParse("18Gi"),
					},
				},
			})
			ExpectApplied(ctx, env.Client, machine)
			ExpectReconcileSucceeded(ctx, machineController, client.ObjectKeyFromObject(machine))

			ExpectStateNodeCount("==", 1)
			ExpectResources(v1.ResourceList{
				v1.ResourceCPU:              resource.MustParse("2"),
				v1.ResourceMemory:           resource.MustParse("32Gi"),
				v1.ResourceEphemeralStorage: resource.MustParse("20Gi"),
			}, ExpectStateNodeExistsForMachine(machine).Capacity())
			ExpectResources(v1.ResourceList{
				v1.ResourceCPU:              resource.MustParse("1"),
				v1.ResourceMemory:           resource.MustParse("30Gi"),
				v1.ResourceEphemeralStorage: resource.MustParse("18Gi"),
			}, ExpectStateNodeExistsForMachine(machine).Allocatable())
		})
		It("should model the inflight capacity of the machine until the node registers and is initialized", func() {
			machine := test.Machine(v1alpha5.Machine{
				Spec: v1alpha5.MachineSpec{
					Requirements: []v1.NodeSelectorRequirement{
						{
							Key:      v1.LabelInstanceTypeStable,
							Operator: v1.NodeSelectorOpIn,
							Values:   []string{cloudProvider.InstanceTypes[0].Name},
						},
						{
							Key:      v1.LabelTopologyZone,
							Operator: v1.NodeSelectorOpIn,
							Values:   []string{"test-zone-1"},
						},
					},
					MachineTemplateRef: &v1alpha5.MachineTemplateRef{
						Name: "default",
					},
				},
				Status: v1alpha5.MachineStatus{
					ProviderID: test.RandomProviderID(),
					Capacity: v1.ResourceList{
						v1.ResourceCPU:              resource.MustParse("2"),
						v1.ResourceMemory:           resource.MustParse("32Gi"),
						v1.ResourceEphemeralStorage: resource.MustParse("20Gi"),
					},
					Allocatable: v1.ResourceList{
						v1.ResourceCPU:              resource.MustParse("1"),
						v1.ResourceMemory:           resource.MustParse("30Gi"),
						v1.ResourceEphemeralStorage: resource.MustParse("18Gi"),
					},
				},
			})
			ExpectApplied(ctx, env.Client, machine)
			ExpectReconcileSucceeded(ctx, machineController, client.ObjectKeyFromObject(machine))

			ExpectStateNodeCount("==", 1)
			ExpectResources(v1.ResourceList{
				v1.ResourceCPU:              resource.MustParse("2"),
				v1.ResourceMemory:           resource.MustParse("32Gi"),
				v1.ResourceEphemeralStorage: resource.MustParse("20Gi"),
			}, ExpectStateNodeExistsForMachine(machine).Capacity())
			ExpectResources(v1.ResourceList{
				v1.ResourceCPU:              resource.MustParse("1"),
				v1.ResourceMemory:           resource.MustParse("30Gi"),
				v1.ResourceEphemeralStorage: resource.MustParse("18Gi"),
			}, ExpectStateNodeExistsForMachine(machine).Allocatable())

			node := test.Node(test.NodeOptions{
				ProviderID: machine.Status.ProviderID,
				Capacity: v1.ResourceList{
					v1.ResourceCPU:              resource.MustParse("1800m"),
					v1.ResourceMemory:           resource.MustParse("30500Mi"),
					v1.ResourceEphemeralStorage: resource.MustParse("19000Mi"),
				},
				Allocatable: v1.ResourceList{
					v1.ResourceCPU:              resource.MustParse("900m"),
					v1.ResourceMemory:           resource.MustParse("29250Mi"),
					v1.ResourceEphemeralStorage: resource.MustParse("17800Mi"),
				},
			})
			ExpectApplied(ctx, env.Client, node)

			ExpectStateNodeCount("==", 1)
			ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))

			// Update machine to be initialized
			machine.StatusConditions().MarkTrue(v1alpha5.MachineInitialized)
			ExpectApplied(ctx, env.Client, machine)
			ExpectReconcileSucceeded(ctx, machineController, client.ObjectKeyFromObject(machine))

			ExpectStateNodeCount("==", 1)
			ExpectStateNodeExistsForMachine(machine)
			ExpectResources(v1.ResourceList{
				v1.ResourceCPU:              resource.MustParse("1800m"),
				v1.ResourceMemory:           resource.MustParse("30500Mi"),
				v1.ResourceEphemeralStorage: resource.MustParse("19000Mi"),
			}, ExpectStateNodeExists(node).Capacity())
			ExpectResources(v1.ResourceList{
				v1.ResourceCPU:              resource.MustParse("900m"),
				v1.ResourceMemory:           resource.MustParse("29250Mi"),
				v1.ResourceEphemeralStorage: resource.MustParse("17800Mi"),
			}, ExpectStateNodeExists(node).Allocatable())
		})
		It("should not return startup taints when the node isn't initialized", func() {
			machine := test.Machine(v1alpha5.Machine{
				Spec: v1alpha5.MachineSpec{
					Requirements: []v1.NodeSelectorRequirement{
						{
							Key:      v1.LabelInstanceTypeStable,
							Operator: v1.NodeSelectorOpIn,
							Values:   []string{cloudProvider.InstanceTypes[0].Name},
						},
						{
							Key:      v1.LabelTopologyZone,
							Operator: v1.NodeSelectorOpIn,
							Values:   []string{"test-zone-1"},
						},
					},
					StartupTaints: []v1.Taint{
						{
							Key:    "custom-taint",
							Value:  "custom-value",
							Effect: v1.TaintEffectNoSchedule,
						},
						{
							Key:    "custom-taint2",
							Value:  "custom-value2",
							Effect: v1.TaintEffectNoExecute,
						},
					},
					MachineTemplateRef: &v1alpha5.MachineTemplateRef{
						Name: "default",
					},
				},
				Status: v1alpha5.MachineStatus{
					ProviderID: test.RandomProviderID(),
					Capacity: v1.ResourceList{
						v1.ResourceCPU:              resource.MustParse("2"),
						v1.ResourceMemory:           resource.MustParse("32Gi"),
						v1.ResourceEphemeralStorage: resource.MustParse("20Gi"),
					},
					Allocatable: v1.ResourceList{
						v1.ResourceCPU:              resource.MustParse("1"),
						v1.ResourceMemory:           resource.MustParse("30Gi"),
						v1.ResourceEphemeralStorage: resource.MustParse("18Gi"),
					},
				},
			})
			ExpectApplied(ctx, env.Client, machine)
			ExpectReconcileSucceeded(ctx, machineController, client.ObjectKeyFromObject(machine))

			ExpectStateNodeCount("==", 1)
			stateNode := ExpectStateNodeExistsForMachine(machine)
			Expect(stateNode.Taints()).To(HaveLen(0))

			node := test.Node(test.NodeOptions{
				ProviderID: machine.Status.ProviderID,
				Capacity: v1.ResourceList{
					v1.ResourceCPU:              resource.MustParse("1800m"),
					v1.ResourceMemory:           resource.MustParse("30500Mi"),
					v1.ResourceEphemeralStorage: resource.MustParse("19000Mi"),
				},
				Allocatable: v1.ResourceList{
					v1.ResourceCPU:              resource.MustParse("900m"),
					v1.ResourceMemory:           resource.MustParse("29250Mi"),
					v1.ResourceEphemeralStorage: resource.MustParse("17800Mi"),
				},
				Taints: []v1.Taint{
					{
						Key:    "custom-taint",
						Value:  "custom-value",
						Effect: v1.TaintEffectNoSchedule,
					},
					{
						Key:    "custom-taint2",
						Value:  "custom-value2",
						Effect: v1.TaintEffectNoExecute,
					},
				},
			})
			ExpectApplied(ctx, env.Client, node)
			ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))

			ExpectStateNodeCount("==", 1)
			stateNode = ExpectStateNodeExists(node)
			Expect(stateNode.Taints()).To(HaveLen(0))
		})
		It("should return startup taints when the node is initialized", func() {
			machine := test.Machine(v1alpha5.Machine{
				Spec: v1alpha5.MachineSpec{
					Requirements: []v1.NodeSelectorRequirement{
						{
							Key:      v1.LabelInstanceTypeStable,
							Operator: v1.NodeSelectorOpIn,
							Values:   []string{cloudProvider.InstanceTypes[0].Name},
						},
						{
							Key:      v1.LabelTopologyZone,
							Operator: v1.NodeSelectorOpIn,
							Values:   []string{"test-zone-1"},
						},
					},
					StartupTaints: []v1.Taint{
						{
							Key:    "custom-taint",
							Value:  "custom-value",
							Effect: v1.TaintEffectNoSchedule,
						},
						{
							Key:    "custom-taint2",
							Value:  "custom-value2",
							Effect: v1.TaintEffectNoExecute,
						},
					},
					MachineTemplateRef: &v1alpha5.MachineTemplateRef{
						Name: "default",
					},
				},
				Status: v1alpha5.MachineStatus{
					ProviderID: test.RandomProviderID(),
					Capacity: v1.ResourceList{
						v1.ResourceCPU:              resource.MustParse("2"),
						v1.ResourceMemory:           resource.MustParse("32Gi"),
						v1.ResourceEphemeralStorage: resource.MustParse("20Gi"),
					},
					Allocatable: v1.ResourceList{
						v1.ResourceCPU:              resource.MustParse("1"),
						v1.ResourceMemory:           resource.MustParse("30Gi"),
						v1.ResourceEphemeralStorage: resource.MustParse("18Gi"),
					},
				},
			})
			ExpectApplied(ctx, env.Client, machine)
			ExpectReconcileSucceeded(ctx, machineController, client.ObjectKeyFromObject(machine))

			node := test.Node(test.NodeOptions{
				ProviderID: machine.Status.ProviderID,
				Capacity: v1.ResourceList{
					v1.ResourceCPU:              resource.MustParse("1800m"),
					v1.ResourceMemory:           resource.MustParse("30500Mi"),
					v1.ResourceEphemeralStorage: resource.MustParse("19000Mi"),
				},
				Allocatable: v1.ResourceList{
					v1.ResourceCPU:              resource.MustParse("900m"),
					v1.ResourceMemory:           resource.MustParse("29250Mi"),
					v1.ResourceEphemeralStorage: resource.MustParse("17800Mi"),
				},
				Taints: []v1.Taint{
					{
						Key:    "custom-taint",
						Value:  "custom-value",
						Effect: v1.TaintEffectNoSchedule,
					},
					{
						Key:    "custom-taint2",
						Value:  "custom-value2",
						Effect: v1.TaintEffectNoExecute,
					},
				},
			})
			ExpectApplied(ctx, env.Client, node)
			ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))

			ExpectMakeMachinesInitialized(ctx, env.Client, machine)
			ExpectMakeNodesInitialized(ctx, env.Client, node)
			ExpectReconcileSucceeded(ctx, machineController, client.ObjectKeyFromObject(machine))
			ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))

			ExpectStateNodeCount("==", 1)
			stateNode := ExpectStateNodeExists(node)
			Expect(stateNode.Taints()).To(HaveLen(2))
			Expect(stateNode.Taints()).To(Equal([]v1.Taint{
				{
					Key:    "custom-taint",
					Value:  "custom-value",
					Effect: v1.TaintEffectNoSchedule,
				},
				{
					Key:    "custom-taint2",
					Value:  "custom-value2",
					Effect: v1.TaintEffectNoExecute,
				},
			}))
		})
		It("should not return known ephemeral taints", func() {
			machine := test.Machine(v1alpha5.Machine{
				Spec: v1alpha5.MachineSpec{
					Requirements: []v1.NodeSelectorRequirement{
						{
							Key:      v1.LabelInstanceTypeStable,
							Operator: v1.NodeSelectorOpIn,
							Values:   []string{cloudProvider.InstanceTypes[0].Name},
						},
						{
							Key:      v1.LabelTopologyZone,
							Operator: v1.NodeSelectorOpIn,
							Values:   []string{"test-zone-1"},
						},
					},
					MachineTemplateRef: &v1alpha5.MachineTemplateRef{
						Name: "default",
					},
				},
				Status: v1alpha5.MachineStatus{
					ProviderID: test.RandomProviderID(),
					Capacity: v1.ResourceList{
						v1.ResourceCPU:              resource.MustParse("2"),
						v1.ResourceMemory:           resource.MustParse("32Gi"),
						v1.ResourceEphemeralStorage: resource.MustParse("20Gi"),
					},
					Allocatable: v1.ResourceList{
						v1.ResourceCPU:              resource.MustParse("1"),
						v1.ResourceMemory:           resource.MustParse("30Gi"),
						v1.ResourceEphemeralStorage: resource.MustParse("18Gi"),
					},
				},
			})
			ExpectApplied(ctx, env.Client, machine)
			ExpectReconcileSucceeded(ctx, machineController, client.ObjectKeyFromObject(machine))

			node := test.Node(test.NodeOptions{
				ProviderID: machine.Status.ProviderID,
				Capacity: v1.ResourceList{
					v1.ResourceCPU:              resource.MustParse("1800m"),
					v1.ResourceMemory:           resource.MustParse("30500Mi"),
					v1.ResourceEphemeralStorage: resource.MustParse("19000Mi"),
				},
				Allocatable: v1.ResourceList{
					v1.ResourceCPU:              resource.MustParse("900m"),
					v1.ResourceMemory:           resource.MustParse("29250Mi"),
					v1.ResourceEphemeralStorage: resource.MustParse("17800Mi"),
				},
				Taints: []v1.Taint{
					{
						Key:    v1.TaintNodeNotReady,
						Effect: v1.TaintEffectNoSchedule,
					},
					{
						Key:    v1.TaintNodeUnreachable,
						Effect: v1.TaintEffectNoSchedule,
					},
					{
						Key:    cloudproviderapi.TaintExternalCloudProvider,
						Effect: v1.TaintEffectNoSchedule,
						Value:  "true",
					},
				},
			})
			ExpectApplied(ctx, env.Client, node)
			ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))

			ExpectStateNodeCount("==", 1)
			stateNode := ExpectStateNodeExists(node)
			Expect(stateNode.Taints()).To(HaveLen(0))
		})
		It("should combine the inflight capacity with node while node isn't initialized", func() {
			machine := test.Machine(v1alpha5.Machine{
				Spec: v1alpha5.MachineSpec{
					Requirements: []v1.NodeSelectorRequirement{
						{
							Key:      v1.LabelInstanceTypeStable,
							Operator: v1.NodeSelectorOpIn,
							Values:   []string{cloudProvider.InstanceTypes[0].Name},
						},
						{
							Key:      v1.LabelTopologyZone,
							Operator: v1.NodeSelectorOpIn,
							Values:   []string{"test-zone-1"},
						},
					},
					MachineTemplateRef: &v1alpha5.MachineTemplateRef{
						Name: "default",
					},
				},
				Status: v1alpha5.MachineStatus{
					ProviderID: test.RandomProviderID(),
					Capacity: v1.ResourceList{
						v1.ResourceCPU:              resource.MustParse("2"),
						v1.ResourceMemory:           resource.MustParse("32Gi"),
						v1.ResourceEphemeralStorage: resource.MustParse("20Gi"),
					},
					Allocatable: v1.ResourceList{
						v1.ResourceCPU:              resource.MustParse("1"),
						v1.ResourceMemory:           resource.MustParse("30Gi"),
						v1.ResourceEphemeralStorage: resource.MustParse("18Gi"),
					},
				},
			})
			ExpectApplied(ctx, env.Client, machine)
			ExpectReconcileSucceeded(ctx, machineController, client.ObjectKeyFromObject(machine))
			ExpectStateNodeCount("==", 1)

			node := test.Node(test.NodeOptions{
				ProviderID: machine.Status.ProviderID,
				Capacity: v1.ResourceList{
					v1.ResourceCPU:              resource.MustParse("1800m"),
					v1.ResourceMemory:           resource.MustParse("0"), // Should use the inflight capacity for this value
					v1.ResourceEphemeralStorage: resource.MustParse("19000Mi"),
				},
				Allocatable: v1.ResourceList{
					v1.ResourceCPU:              resource.MustParse("0"), // Should use the inflight allocatable for this value
					v1.ResourceMemory:           resource.MustParse("29250Mi"),
					v1.ResourceEphemeralStorage: resource.MustParse("0"), // Should use the inflight allocatable for this value
				},
			})
			ExpectApplied(ctx, env.Client, node)
			ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))
			ExpectStateNodeCount("==", 1)

			// Machine isn't initialized yet so the resources should remain as the in-flight resources
			ExpectResources(v1.ResourceList{
				v1.ResourceCPU:              resource.MustParse("1800m"),
				v1.ResourceMemory:           resource.MustParse("32Gi"),
				v1.ResourceEphemeralStorage: resource.MustParse("19000Mi"),
			}, ExpectStateNodeExistsForMachine(machine).Capacity())
			ExpectResources(v1.ResourceList{
				v1.ResourceCPU:              resource.MustParse("1"),
				v1.ResourceMemory:           resource.MustParse("29250Mi"),
				v1.ResourceEphemeralStorage: resource.MustParse("18Gi"),
			}, ExpectStateNodeExistsForMachine(machine).Allocatable())
		})
		It("should continue node nomination when an inflight node becomes a real node", func() {
			machine := test.Machine(v1alpha5.Machine{
				Status: v1alpha5.MachineStatus{
					ProviderID: test.RandomProviderID(),
				},
			})
			ExpectApplied(ctx, env.Client, machine)
			ExpectReconcileSucceeded(ctx, machineController, client.ObjectKeyFromObject(machine))
			ExpectStateNodeCount("==", 1)
			cluster.NominateNodeForPod(ctx, machine.Status.ProviderID)
			Expect(ExpectStateNodeExistsForMachine(machine).Nominated()).To(BeTrue())

			node := test.Node(test.NodeOptions{
				ProviderID: machine.Status.ProviderID,
			})
			node.Spec.ProviderID = machine.Status.ProviderID
			ExpectApplied(ctx, env.Client, node)
			ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))
			ExpectStateNodeCount("==", 1)
			Expect(ExpectStateNodeExists(node).Nominated()).To(BeTrue())
		})
		It("should continue MarkedForDeletion when an inflight node becomes a real node", func() {
			machine := test.Machine(v1alpha5.Machine{
				Status: v1alpha5.MachineStatus{
					ProviderID: test.RandomProviderID(),
				},
			})
			ExpectApplied(ctx, env.Client, machine)
			ExpectReconcileSucceeded(ctx, machineController, client.ObjectKeyFromObject(machine))
			ExpectStateNodeCount("==", 1)
			cluster.MarkForDeletion(machine.Status.ProviderID)
			Expect(ExpectStateNodeExistsForMachine(machine).MarkedForDeletion()).To(BeTrue())

			node := test.Node(test.NodeOptions{
				ProviderID: machine.Status.ProviderID,
			})
			node.Spec.ProviderID = machine.Status.ProviderID
			ExpectApplied(ctx, env.Client, node)
			ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))
			ExpectStateNodeCount("==", 1)
			Expect(ExpectStateNodeExists(node).MarkedForDeletion()).To(BeTrue())
		})
	})
	Context("NodeClaim", func() {
		It("should ignore nodeclaims that don't yet have provider id", func() {
			nodeClaim := test.NodeClaim(v1beta1.NodeClaim{
				Spec: v1beta1.NodeClaimSpec{
					Taints: []v1.Taint{
						{
							Key:    "custom-taint",
							Value:  "custom-value",
							Effect: v1.TaintEffectNoSchedule,
						},
					},
					Requirements: []v1.NodeSelectorRequirement{
						{
							Key:      v1.LabelInstanceTypeStable,
							Operator: v1.NodeSelectorOpIn,
							Values:   []string{cloudProvider.InstanceTypes[0].Name},
						},
						{
							Key:      v1.LabelTopologyZone,
							Operator: v1.NodeSelectorOpIn,
							Values:   []string{"test-zone-1"},
						},
					},
					Resources: v1beta1.ResourceRequirements{
						Requests: v1.ResourceList{
							v1.ResourceCPU:              resource.MustParse("2"),
							v1.ResourceMemory:           resource.MustParse("2Gi"),
							v1.ResourceEphemeralStorage: resource.MustParse("1Gi"),
						},
					},
					NodeClass: &v1beta1.NodeClassReference{
						Name: "default",
					},
				},
			})
			nodeClaim.Status.ProviderID = ""
			ExpectApplied(ctx, env.Client, nodeClaim)
			ExpectReconcileSucceeded(ctx, nodeClaimController, client.ObjectKeyFromObject(nodeClaim))
			ExpectStateNodeNotFoundForNodeClaim(nodeClaim)
		})
		It("should ignore node updates if nodes don't have provider id and are owned", func() {
			node := test.Node(test.NodeOptions{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1beta1.NodePoolLabelKey: nodePool.Name,
					},
				},
				Capacity: v1.ResourceList{
					v1.ResourceCPU:              resource.MustParse("1800m"),
					v1.ResourceMemory:           resource.MustParse("0"), // Should use the inflight capacity for this value
					v1.ResourceEphemeralStorage: resource.MustParse("19000Mi"),
				},
				Allocatable: v1.ResourceList{
					v1.ResourceCPU:              resource.MustParse("0"), // Should use the inflight allocatable for this value
					v1.ResourceMemory:           resource.MustParse("29250Mi"),
					v1.ResourceEphemeralStorage: resource.MustParse("0"), // Should use the inflight allocatable for this value
				},
			})
			ExpectApplied(ctx, env.Client, node)
			ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))
			ExpectStateNodeCount("==", 0)
		})
		It("shouldn't ignore node updates if nodes don't have provider id and EnableNodePools/EnableNodeClaims isn't enabled", func() {
			nodepoolutil.EnableNodePools = false
			nodeclaimutil.EnableNodeClaims = false

			node := test.Node(test.NodeOptions{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1beta1.NodePoolLabelKey: nodePool.Name,
					},
				},
				Capacity: v1.ResourceList{
					v1.ResourceCPU:              resource.MustParse("1800m"),
					v1.ResourceMemory:           resource.MustParse("0"), // Should use the inflight capacity for this value
					v1.ResourceEphemeralStorage: resource.MustParse("19000Mi"),
				},
				Allocatable: v1.ResourceList{
					v1.ResourceCPU:              resource.MustParse("0"), // Should use the inflight allocatable for this value
					v1.ResourceMemory:           resource.MustParse("29250Mi"),
					v1.ResourceEphemeralStorage: resource.MustParse("0"), // Should use the inflight allocatable for this value
				},
			})
			ExpectApplied(ctx, env.Client, node)
			ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))
			ExpectStateNodeCount("==", 1)
		})
		It("shouldn't populate inflight capacity if EnableNodePools/EnableNodeClaims isn't enabled", func() {
			nodepoolutil.EnableNodePools = false
			nodeclaimutil.EnableNodeClaims = false

			instanceType := cloudProvider.InstanceTypes[0]
			node := test.Node(test.NodeOptions{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
					v1beta1.NodePoolLabelKey:   nodePool.Name,
					v1.LabelInstanceTypeStable: instanceType.Name,
				}},
				ProviderID: test.RandomProviderID(),
			})
			ExpectApplied(ctx, env.Client, node)
			ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))

			ExpectStateNodeCount("==", 1)
			Expect(ExpectStateNodeExists(node).Allocatable()).To(HaveLen(0))
			Expect(ExpectStateNodeExists(node).Capacity()).To(HaveLen(0))
		})
		It("shouldn't populate startup taints if EnableNodePools/EnableNodeClaims isn't enabled", func() {
			nodepoolutil.EnableNodePools = false
			nodeclaimutil.EnableNodeClaims = false

			instanceType := cloudProvider.InstanceTypes[0]
			nodePool.Spec.Template.Spec.StartupTaints = []v1.Taint{
				{
					Key:    "test",
					Effect: v1.TaintEffectNoSchedule,
				},
			}
			node := test.Node(test.NodeOptions{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
					v1beta1.NodePoolLabelKey:   nodePool.Name,
					v1.LabelInstanceTypeStable: instanceType.Name,
				}},
				Taints: []v1.Taint{
					{
						Key:    "test",
						Effect: v1.TaintEffectNoSchedule,
					},
				},
				ProviderID: test.RandomProviderID(),
			})
			ExpectApplied(ctx, env.Client, node)
			ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))

			ExpectStateNodeCount("==", 1)

			// This taint wouldn't show if we discovered the startup taints
			Expect(ExpectStateNodeExists(node).Taints()).To(HaveLen(1))
		})
		It("should model the inflight data as nodeclaim with no node", func() {
			nodeClaim := test.NodeClaim(v1beta1.NodeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						v1beta1.DoNotDisruptAnnotationKey: "true",
					},
					Labels: map[string]string{
						v1beta1.NodePoolLabelKey:   "default",
						v1.LabelInstanceTypeStable: cloudProvider.InstanceTypes[0].Name,
						v1.LabelTopologyZone:       "test-zone-1",
						v1.LabelTopologyRegion:     "test-region",
						v1.LabelHostname:           "custom-host-name",
					},
				},
				Spec: v1beta1.NodeClaimSpec{
					Taints: []v1.Taint{
						{
							Key:    "custom-taint",
							Value:  "custom-value",
							Effect: v1.TaintEffectNoSchedule,
						},
					},
					Requirements: []v1.NodeSelectorRequirement{
						{
							Key:      v1.LabelInstanceTypeStable,
							Operator: v1.NodeSelectorOpIn,
							Values:   []string{cloudProvider.InstanceTypes[0].Name},
						},
						{
							Key:      v1.LabelTopologyZone,
							Operator: v1.NodeSelectorOpIn,
							Values:   []string{"test-zone-1"},
						},
					},
					Resources: v1beta1.ResourceRequirements{
						Requests: v1.ResourceList{
							v1.ResourceCPU:              resource.MustParse("2"),
							v1.ResourceMemory:           resource.MustParse("2Gi"),
							v1.ResourceEphemeralStorage: resource.MustParse("1Gi"),
						},
					},
					NodeClass: &v1beta1.NodeClassReference{
						Name: "default",
					},
				},
				Status: v1beta1.NodeClaimStatus{
					ProviderID: test.RandomProviderID(),
				},
			})
			ExpectApplied(ctx, env.Client, nodeClaim)
			ExpectReconcileSucceeded(ctx, nodeClaimController, client.ObjectKeyFromObject(nodeClaim))

			ExpectStateNodeCount("==", 1)
			stateNode := ExpectStateNodeExistsForNodeClaim(nodeClaim)
			Expect(stateNode.Labels()).To(HaveKeyWithValue(v1beta1.NodePoolLabelKey, "default"))
			Expect(stateNode.Labels()).To(HaveKeyWithValue(v1.LabelInstanceTypeStable, cloudProvider.InstanceTypes[0].Name))
			Expect(stateNode.Labels()).To(HaveKeyWithValue(v1.LabelTopologyZone, "test-zone-1"))
			Expect(stateNode.Labels()).To(HaveKeyWithValue(v1.LabelTopologyRegion, "test-region"))
			Expect(stateNode.Labels()).To(HaveKeyWithValue(v1.LabelHostname, "custom-host-name"))
			Expect(stateNode.HostName()).To(Equal("custom-host-name"))
			Expect(stateNode.Annotations()).To(HaveKeyWithValue(v1beta1.DoNotDisruptAnnotationKey, "true"))
			Expect(stateNode.Initialized()).To(BeFalse())
			Expect(stateNode.Managed()).To(BeTrue())
		})
		It("should model the inflight capacity/allocatable as the nodeclaim capacity/allocatable", func() {
			nodeClaim := test.NodeClaim(v1beta1.NodeClaim{
				Spec: v1beta1.NodeClaimSpec{
					Requirements: []v1.NodeSelectorRequirement{
						{
							Key:      v1.LabelInstanceTypeStable,
							Operator: v1.NodeSelectorOpIn,
							Values:   []string{cloudProvider.InstanceTypes[0].Name},
						},
						{
							Key:      v1.LabelTopologyZone,
							Operator: v1.NodeSelectorOpIn,
							Values:   []string{"test-zone-1"},
						},
					},
					NodeClass: &v1beta1.NodeClassReference{
						Name: "default",
					},
				},
				Status: v1beta1.NodeClaimStatus{
					ProviderID: test.RandomProviderID(),
					Capacity: v1.ResourceList{
						v1.ResourceCPU:              resource.MustParse("2"),
						v1.ResourceMemory:           resource.MustParse("32Gi"),
						v1.ResourceEphemeralStorage: resource.MustParse("20Gi"),
					},
					Allocatable: v1.ResourceList{
						v1.ResourceCPU:              resource.MustParse("1"),
						v1.ResourceMemory:           resource.MustParse("30Gi"),
						v1.ResourceEphemeralStorage: resource.MustParse("18Gi"),
					},
				},
			})
			ExpectApplied(ctx, env.Client, nodeClaim)
			ExpectReconcileSucceeded(ctx, nodeClaimController, client.ObjectKeyFromObject(nodeClaim))

			ExpectStateNodeCount("==", 1)
			ExpectResources(v1.ResourceList{
				v1.ResourceCPU:              resource.MustParse("2"),
				v1.ResourceMemory:           resource.MustParse("32Gi"),
				v1.ResourceEphemeralStorage: resource.MustParse("20Gi"),
			}, ExpectStateNodeExistsForNodeClaim(nodeClaim).Capacity())
			ExpectResources(v1.ResourceList{
				v1.ResourceCPU:              resource.MustParse("1"),
				v1.ResourceMemory:           resource.MustParse("30Gi"),
				v1.ResourceEphemeralStorage: resource.MustParse("18Gi"),
			}, ExpectStateNodeExistsForNodeClaim(nodeClaim).Allocatable())
		})
		It("should model the inflight capacity of the nodeclaim until the node registers and is initialized", func() {
			nodeClaim := test.NodeClaim(v1beta1.NodeClaim{
				Spec: v1beta1.NodeClaimSpec{
					Requirements: []v1.NodeSelectorRequirement{
						{
							Key:      v1.LabelInstanceTypeStable,
							Operator: v1.NodeSelectorOpIn,
							Values:   []string{cloudProvider.InstanceTypes[0].Name},
						},
						{
							Key:      v1.LabelTopologyZone,
							Operator: v1.NodeSelectorOpIn,
							Values:   []string{"test-zone-1"},
						},
					},
					NodeClass: &v1beta1.NodeClassReference{
						Name: "default",
					},
				},
				Status: v1beta1.NodeClaimStatus{
					ProviderID: test.RandomProviderID(),
					Capacity: v1.ResourceList{
						v1.ResourceCPU:              resource.MustParse("2"),
						v1.ResourceMemory:           resource.MustParse("32Gi"),
						v1.ResourceEphemeralStorage: resource.MustParse("20Gi"),
					},
					Allocatable: v1.ResourceList{
						v1.ResourceCPU:              resource.MustParse("1"),
						v1.ResourceMemory:           resource.MustParse("30Gi"),
						v1.ResourceEphemeralStorage: resource.MustParse("18Gi"),
					},
				},
			})
			ExpectApplied(ctx, env.Client, nodeClaim)
			ExpectReconcileSucceeded(ctx, nodeClaimController, client.ObjectKeyFromObject(nodeClaim))

			ExpectStateNodeCount("==", 1)
			ExpectResources(v1.ResourceList{
				v1.ResourceCPU:              resource.MustParse("2"),
				v1.ResourceMemory:           resource.MustParse("32Gi"),
				v1.ResourceEphemeralStorage: resource.MustParse("20Gi"),
			}, ExpectStateNodeExistsForNodeClaim(nodeClaim).Capacity())
			ExpectResources(v1.ResourceList{
				v1.ResourceCPU:              resource.MustParse("1"),
				v1.ResourceMemory:           resource.MustParse("30Gi"),
				v1.ResourceEphemeralStorage: resource.MustParse("18Gi"),
			}, ExpectStateNodeExistsForNodeClaim(nodeClaim).Allocatable())

			node := test.Node(test.NodeOptions{
				ProviderID: nodeClaim.Status.ProviderID,
				Capacity: v1.ResourceList{
					v1.ResourceCPU:              resource.MustParse("1800m"),
					v1.ResourceMemory:           resource.MustParse("30500Mi"),
					v1.ResourceEphemeralStorage: resource.MustParse("19000Mi"),
				},
				Allocatable: v1.ResourceList{
					v1.ResourceCPU:              resource.MustParse("900m"),
					v1.ResourceMemory:           resource.MustParse("29250Mi"),
					v1.ResourceEphemeralStorage: resource.MustParse("17800Mi"),
				},
			})
			ExpectApplied(ctx, env.Client, node)

			ExpectStateNodeCount("==", 1)
			ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))

			// Update nodeClaim to be initialized
			nodeClaim.StatusConditions().MarkTrue(v1beta1.Initialized)
			ExpectApplied(ctx, env.Client, nodeClaim)
			ExpectReconcileSucceeded(ctx, nodeClaimController, client.ObjectKeyFromObject(nodeClaim))

			ExpectStateNodeCount("==", 1)
			ExpectStateNodeExistsForNodeClaim(nodeClaim)
			ExpectResources(v1.ResourceList{
				v1.ResourceCPU:              resource.MustParse("1800m"),
				v1.ResourceMemory:           resource.MustParse("30500Mi"),
				v1.ResourceEphemeralStorage: resource.MustParse("19000Mi"),
			}, ExpectStateNodeExists(node).Capacity())
			ExpectResources(v1.ResourceList{
				v1.ResourceCPU:              resource.MustParse("900m"),
				v1.ResourceMemory:           resource.MustParse("29250Mi"),
				v1.ResourceEphemeralStorage: resource.MustParse("17800Mi"),
			}, ExpectStateNodeExists(node).Allocatable())
		})
		It("should not return startup taints when the node isn't initialized", func() {
			nodeClaim := test.NodeClaim(v1beta1.NodeClaim{
				Spec: v1beta1.NodeClaimSpec{
					Requirements: []v1.NodeSelectorRequirement{
						{
							Key:      v1.LabelInstanceTypeStable,
							Operator: v1.NodeSelectorOpIn,
							Values:   []string{cloudProvider.InstanceTypes[0].Name},
						},
						{
							Key:      v1.LabelTopologyZone,
							Operator: v1.NodeSelectorOpIn,
							Values:   []string{"test-zone-1"},
						},
					},
					StartupTaints: []v1.Taint{
						{
							Key:    "custom-taint",
							Value:  "custom-value",
							Effect: v1.TaintEffectNoSchedule,
						},
						{
							Key:    "custom-taint2",
							Value:  "custom-value2",
							Effect: v1.TaintEffectNoExecute,
						},
					},
					NodeClass: &v1beta1.NodeClassReference{
						Name: "default",
					},
				},
				Status: v1beta1.NodeClaimStatus{
					ProviderID: test.RandomProviderID(),
					Capacity: v1.ResourceList{
						v1.ResourceCPU:              resource.MustParse("2"),
						v1.ResourceMemory:           resource.MustParse("32Gi"),
						v1.ResourceEphemeralStorage: resource.MustParse("20Gi"),
					},
					Allocatable: v1.ResourceList{
						v1.ResourceCPU:              resource.MustParse("1"),
						v1.ResourceMemory:           resource.MustParse("30Gi"),
						v1.ResourceEphemeralStorage: resource.MustParse("18Gi"),
					},
				},
			})
			ExpectApplied(ctx, env.Client, nodeClaim)
			ExpectReconcileSucceeded(ctx, nodeClaimController, client.ObjectKeyFromObject(nodeClaim))

			ExpectStateNodeCount("==", 1)
			stateNode := ExpectStateNodeExistsForNodeClaim(nodeClaim)
			Expect(stateNode.Taints()).To(HaveLen(0))

			node := test.Node(test.NodeOptions{
				ProviderID: nodeClaim.Status.ProviderID,
				Capacity: v1.ResourceList{
					v1.ResourceCPU:              resource.MustParse("1800m"),
					v1.ResourceMemory:           resource.MustParse("30500Mi"),
					v1.ResourceEphemeralStorage: resource.MustParse("19000Mi"),
				},
				Allocatable: v1.ResourceList{
					v1.ResourceCPU:              resource.MustParse("900m"),
					v1.ResourceMemory:           resource.MustParse("29250Mi"),
					v1.ResourceEphemeralStorage: resource.MustParse("17800Mi"),
				},
				Taints: []v1.Taint{
					{
						Key:    "custom-taint",
						Value:  "custom-value",
						Effect: v1.TaintEffectNoSchedule,
					},
					{
						Key:    "custom-taint2",
						Value:  "custom-value2",
						Effect: v1.TaintEffectNoExecute,
					},
				},
			})
			ExpectApplied(ctx, env.Client, node)
			ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))

			ExpectStateNodeCount("==", 1)
			stateNode = ExpectStateNodeExists(node)
			Expect(stateNode.Taints()).To(HaveLen(0))
		})
		It("should return startup taints when the node is initialized", func() {
			nodeClaim := test.NodeClaim(v1beta1.NodeClaim{
				Spec: v1beta1.NodeClaimSpec{
					Requirements: []v1.NodeSelectorRequirement{
						{
							Key:      v1.LabelInstanceTypeStable,
							Operator: v1.NodeSelectorOpIn,
							Values:   []string{cloudProvider.InstanceTypes[0].Name},
						},
						{
							Key:      v1.LabelTopologyZone,
							Operator: v1.NodeSelectorOpIn,
							Values:   []string{"test-zone-1"},
						},
					},
					StartupTaints: []v1.Taint{
						{
							Key:    "custom-taint",
							Value:  "custom-value",
							Effect: v1.TaintEffectNoSchedule,
						},
						{
							Key:    "custom-taint2",
							Value:  "custom-value2",
							Effect: v1.TaintEffectNoExecute,
						},
					},
					NodeClass: &v1beta1.NodeClassReference{
						Name: "default",
					},
				},
				Status: v1beta1.NodeClaimStatus{
					ProviderID: test.RandomProviderID(),
					Capacity: v1.ResourceList{
						v1.ResourceCPU:              resource.MustParse("2"),
						v1.ResourceMemory:           resource.MustParse("32Gi"),
						v1.ResourceEphemeralStorage: resource.MustParse("20Gi"),
					},
					Allocatable: v1.ResourceList{
						v1.ResourceCPU:              resource.MustParse("1"),
						v1.ResourceMemory:           resource.MustParse("30Gi"),
						v1.ResourceEphemeralStorage: resource.MustParse("18Gi"),
					},
				},
			})
			ExpectApplied(ctx, env.Client, nodeClaim)
			ExpectReconcileSucceeded(ctx, nodeClaimController, client.ObjectKeyFromObject(nodeClaim))

			node := test.Node(test.NodeOptions{
				ProviderID: nodeClaim.Status.ProviderID,
				Capacity: v1.ResourceList{
					v1.ResourceCPU:              resource.MustParse("1800m"),
					v1.ResourceMemory:           resource.MustParse("30500Mi"),
					v1.ResourceEphemeralStorage: resource.MustParse("19000Mi"),
				},
				Allocatable: v1.ResourceList{
					v1.ResourceCPU:              resource.MustParse("900m"),
					v1.ResourceMemory:           resource.MustParse("29250Mi"),
					v1.ResourceEphemeralStorage: resource.MustParse("17800Mi"),
				},
				Taints: []v1.Taint{
					{
						Key:    "custom-taint",
						Value:  "custom-value",
						Effect: v1.TaintEffectNoSchedule,
					},
					{
						Key:    "custom-taint2",
						Value:  "custom-value2",
						Effect: v1.TaintEffectNoExecute,
					},
				},
			})
			ExpectApplied(ctx, env.Client, node)
			ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))

			ExpectMakeNodeClaimsInitialized(ctx, env.Client, nodeClaim)
			ExpectMakeNodesInitialized(ctx, env.Client, node)
			ExpectReconcileSucceeded(ctx, nodeClaimController, client.ObjectKeyFromObject(nodeClaim))
			ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))

			ExpectStateNodeCount("==", 1)
			stateNode := ExpectStateNodeExists(node)
			Expect(stateNode.Taints()).To(HaveLen(2))
			Expect(stateNode.Taints()).To(Equal([]v1.Taint{
				{
					Key:    "custom-taint",
					Value:  "custom-value",
					Effect: v1.TaintEffectNoSchedule,
				},
				{
					Key:    "custom-taint2",
					Value:  "custom-value2",
					Effect: v1.TaintEffectNoExecute,
				},
			}))
		})
		It("should not return known ephemeral taints", func() {
			nodeClaim := test.NodeClaim(v1beta1.NodeClaim{
				Spec: v1beta1.NodeClaimSpec{
					Requirements: []v1.NodeSelectorRequirement{
						{
							Key:      v1.LabelInstanceTypeStable,
							Operator: v1.NodeSelectorOpIn,
							Values:   []string{cloudProvider.InstanceTypes[0].Name},
						},
						{
							Key:      v1.LabelTopologyZone,
							Operator: v1.NodeSelectorOpIn,
							Values:   []string{"test-zone-1"},
						},
					},
					NodeClass: &v1beta1.NodeClassReference{
						Name: "default",
					},
				},
				Status: v1beta1.NodeClaimStatus{
					ProviderID: test.RandomProviderID(),
					Capacity: v1.ResourceList{
						v1.ResourceCPU:              resource.MustParse("2"),
						v1.ResourceMemory:           resource.MustParse("32Gi"),
						v1.ResourceEphemeralStorage: resource.MustParse("20Gi"),
					},
					Allocatable: v1.ResourceList{
						v1.ResourceCPU:              resource.MustParse("1"),
						v1.ResourceMemory:           resource.MustParse("30Gi"),
						v1.ResourceEphemeralStorage: resource.MustParse("18Gi"),
					},
				},
			})
			ExpectApplied(ctx, env.Client, nodeClaim)
			ExpectReconcileSucceeded(ctx, nodeClaimController, client.ObjectKeyFromObject(nodeClaim))

			node := test.Node(test.NodeOptions{
				ProviderID: nodeClaim.Status.ProviderID,
				Capacity: v1.ResourceList{
					v1.ResourceCPU:              resource.MustParse("1800m"),
					v1.ResourceMemory:           resource.MustParse("30500Mi"),
					v1.ResourceEphemeralStorage: resource.MustParse("19000Mi"),
				},
				Allocatable: v1.ResourceList{
					v1.ResourceCPU:              resource.MustParse("900m"),
					v1.ResourceMemory:           resource.MustParse("29250Mi"),
					v1.ResourceEphemeralStorage: resource.MustParse("17800Mi"),
				},
				Taints: []v1.Taint{
					{
						Key:    v1.TaintNodeNotReady,
						Effect: v1.TaintEffectNoSchedule,
					},
					{
						Key:    v1.TaintNodeUnreachable,
						Effect: v1.TaintEffectNoSchedule,
					},
					{
						Key:    cloudproviderapi.TaintExternalCloudProvider,
						Effect: v1.TaintEffectNoSchedule,
						Value:  "true",
					},
				},
			})
			ExpectApplied(ctx, env.Client, node)
			ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))

			ExpectStateNodeCount("==", 1)
			stateNode := ExpectStateNodeExists(node)
			Expect(stateNode.Taints()).To(HaveLen(0))
		})
		It("should combine the inflight capacity with node while node isn't initialized", func() {
			nodeClaim := test.NodeClaim(v1beta1.NodeClaim{
				Spec: v1beta1.NodeClaimSpec{
					Requirements: []v1.NodeSelectorRequirement{
						{
							Key:      v1.LabelInstanceTypeStable,
							Operator: v1.NodeSelectorOpIn,
							Values:   []string{cloudProvider.InstanceTypes[0].Name},
						},
						{
							Key:      v1.LabelTopologyZone,
							Operator: v1.NodeSelectorOpIn,
							Values:   []string{"test-zone-1"},
						},
					},
					NodeClass: &v1beta1.NodeClassReference{
						Name: "default",
					},
				},
				Status: v1beta1.NodeClaimStatus{
					ProviderID: test.RandomProviderID(),
					Capacity: v1.ResourceList{
						v1.ResourceCPU:              resource.MustParse("2"),
						v1.ResourceMemory:           resource.MustParse("32Gi"),
						v1.ResourceEphemeralStorage: resource.MustParse("20Gi"),
					},
					Allocatable: v1.ResourceList{
						v1.ResourceCPU:              resource.MustParse("1"),
						v1.ResourceMemory:           resource.MustParse("30Gi"),
						v1.ResourceEphemeralStorage: resource.MustParse("18Gi"),
					},
				},
			})
			ExpectApplied(ctx, env.Client, nodeClaim)
			ExpectReconcileSucceeded(ctx, nodeClaimController, client.ObjectKeyFromObject(nodeClaim))
			ExpectStateNodeCount("==", 1)

			node := test.Node(test.NodeOptions{
				ProviderID: nodeClaim.Status.ProviderID,
				Capacity: v1.ResourceList{
					v1.ResourceCPU:              resource.MustParse("1800m"),
					v1.ResourceMemory:           resource.MustParse("0"), // Should use the inflight capacity for this value
					v1.ResourceEphemeralStorage: resource.MustParse("19000Mi"),
				},
				Allocatable: v1.ResourceList{
					v1.ResourceCPU:              resource.MustParse("0"), // Should use the inflight allocatable for this value
					v1.ResourceMemory:           resource.MustParse("29250Mi"),
					v1.ResourceEphemeralStorage: resource.MustParse("0"), // Should use the inflight allocatable for this value
				},
			})
			ExpectApplied(ctx, env.Client, node)
			ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))
			ExpectStateNodeCount("==", 1)

			// Machine isn't initialized yet so the resources should remain as the in-flight resources
			ExpectResources(v1.ResourceList{
				v1.ResourceCPU:              resource.MustParse("1800m"),
				v1.ResourceMemory:           resource.MustParse("32Gi"),
				v1.ResourceEphemeralStorage: resource.MustParse("19000Mi"),
			}, ExpectStateNodeExistsForNodeClaim(nodeClaim).Capacity())
			ExpectResources(v1.ResourceList{
				v1.ResourceCPU:              resource.MustParse("1"),
				v1.ResourceMemory:           resource.MustParse("29250Mi"),
				v1.ResourceEphemeralStorage: resource.MustParse("18Gi"),
			}, ExpectStateNodeExistsForNodeClaim(nodeClaim).Allocatable())
		})
		It("should continue node nomination when an inflight node becomes a real node", func() {
			nodeClaim := test.NodeClaim(v1beta1.NodeClaim{
				Status: v1beta1.NodeClaimStatus{
					ProviderID: test.RandomProviderID(),
				},
			})
			ExpectApplied(ctx, env.Client, nodeClaim)
			ExpectReconcileSucceeded(ctx, nodeClaimController, client.ObjectKeyFromObject(nodeClaim))
			ExpectStateNodeCount("==", 1)
			cluster.NominateNodeForPod(ctx, nodeClaim.Status.ProviderID)
			Expect(ExpectStateNodeExistsForNodeClaim(nodeClaim).Nominated()).To(BeTrue())

			node := test.Node(test.NodeOptions{
				ProviderID: nodeClaim.Status.ProviderID,
			})
			node.Spec.ProviderID = nodeClaim.Status.ProviderID
			ExpectApplied(ctx, env.Client, node)
			ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))
			ExpectStateNodeCount("==", 1)
			Expect(ExpectStateNodeExists(node).Nominated()).To(BeTrue())
		})
		It("should continue MarkedForDeletion when an inflight node becomes a real node", func() {
			nodeClaim := test.NodeClaim(v1beta1.NodeClaim{
				Status: v1beta1.NodeClaimStatus{
					ProviderID: test.RandomProviderID(),
				},
			})
			ExpectApplied(ctx, env.Client, nodeClaim)
			ExpectReconcileSucceeded(ctx, nodeClaimController, client.ObjectKeyFromObject(nodeClaim))
			ExpectStateNodeCount("==", 1)
			cluster.MarkForDeletion(nodeClaim.Status.ProviderID)
			Expect(ExpectStateNodeExistsForNodeClaim(nodeClaim).MarkedForDeletion()).To(BeTrue())

			node := test.Node(test.NodeOptions{
				ProviderID: nodeClaim.Status.ProviderID,
			})
			node.Spec.ProviderID = nodeClaim.Status.ProviderID
			ExpectApplied(ctx, env.Client, node)
			ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))
			ExpectStateNodeCount("==", 1)
			Expect(ExpectStateNodeExists(node).MarkedForDeletion()).To(BeTrue())
		})
	})
})

var _ = Describe("Node Deletion", func() {
	It("should not leak a state node when the Machine and Node names match", func() {
		machine, node := test.MachineAndNode()
		node.Name = machine.Name

		ExpectApplied(ctx, env.Client, machine, node)
		ExpectReconcileSucceeded(ctx, machineController, client.ObjectKeyFromObject(machine))
		ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))

		ExpectStateNodeCount("==", 1)

		// Expect that the node isn't leaked due to names matching
		ExpectDeleted(ctx, env.Client, machine)
		ExpectReconcileSucceeded(ctx, machineController, client.ObjectKeyFromObject(machine))
		ExpectStateNodeCount("==", 1)
		ExpectDeleted(ctx, env.Client, node)
		ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))
		ExpectStateNodeCount("==", 0)
	})
	It("should not leak a state node when the NodeClaim and Node names match", func() {
		nodeClaim, node := test.NodeClaimAndNode()
		node.Name = nodeClaim.Name

		ExpectApplied(ctx, env.Client, nodeClaim, node)
		ExpectReconcileSucceeded(ctx, nodeClaimController, client.ObjectKeyFromObject(nodeClaim))
		ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))

		ExpectStateNodeCount("==", 1)

		// Expect that the node isn't leaked due to names matching
		ExpectDeleted(ctx, env.Client, nodeClaim)
		ExpectReconcileSucceeded(ctx, nodeClaimController, client.ObjectKeyFromObject(nodeClaim))
		ExpectStateNodeCount("==", 1)
		ExpectDeleted(ctx, env.Client, node)
		ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))
		ExpectStateNodeCount("==", 0)
	})
	It("should not leak a state node when the Machine and NodeClaim names match", func() {
		machine, node1 := test.MachineAndNode()
		nodeClaim, node2 := test.NodeClaimAndNode()
		nodeClaim.Name = machine.Name

		ExpectApplied(ctx, env.Client, machine, nodeClaim, node1, node2)
		ExpectReconcileSucceeded(ctx, machineController, client.ObjectKeyFromObject(machine))
		ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node1))
		ExpectReconcileSucceeded(ctx, machineController, client.ObjectKeyFromObject(nodeClaim))
		ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node2))

		ExpectStateNodeCount("==", 2)

		// Expect that the node isn't leaked due to names matching
		ExpectDeleted(ctx, env.Client, machine)
		ExpectReconcileSucceeded(ctx, machineController, client.ObjectKeyFromObject(machine))
		ExpectStateNodeCount("==", 2)
		ExpectDeleted(ctx, env.Client, node1)
		ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node1))
		ExpectStateNodeCount("==", 1)
	})
})

var _ = Describe("Node Resource Level", func() {
	It("should not count pods not bound to nodes", func() {
		pod1 := test.UnschedulablePod(test.PodOptions{
			ResourceRequirements: v1.ResourceRequirements{
				Requests: map[v1.ResourceName]resource.Quantity{
					v1.ResourceCPU: resource.MustParse("1.5"),
				}},
		})
		pod2 := test.UnschedulablePod(test.PodOptions{
			ResourceRequirements: v1.ResourceRequirements{
				Requests: map[v1.ResourceName]resource.Quantity{
					v1.ResourceCPU: resource.MustParse("2"),
				}},
		})
		node := test.Node(test.NodeOptions{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
				v1alpha5.ProvisionerNameLabelKey: provisioner.Name,
				v1.LabelInstanceTypeStable:       cloudProvider.InstanceTypes[0].Name,
			}},
			Allocatable: map[v1.ResourceName]resource.Quantity{
				v1.ResourceCPU: resource.MustParse("4"),
			},
			ProviderID: test.RandomProviderID(),
		})
		ExpectApplied(ctx, env.Client, pod1, pod2)
		ExpectApplied(ctx, env.Client, node)

		ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))
		ExpectReconcileSucceeded(ctx, podController, client.ObjectKeyFromObject(pod1))
		ExpectReconcileSucceeded(ctx, podController, client.ObjectKeyFromObject(pod2))

		// two pods, but neither is bound to the node so the node's CPU requests should be zero
		ExpectResources(v1.ResourceList{v1.ResourceCPU: resource.MustParse("0.0")}, ExpectStateNodeExists(node).PodRequests())
	})
	It("should count new pods bound to nodes", func() {
		pod1 := test.UnschedulablePod(test.PodOptions{
			ResourceRequirements: v1.ResourceRequirements{
				Requests: map[v1.ResourceName]resource.Quantity{
					v1.ResourceCPU: resource.MustParse("1.5"),
				}},
		})
		pod2 := test.UnschedulablePod(test.PodOptions{
			ResourceRequirements: v1.ResourceRequirements{
				Requests: map[v1.ResourceName]resource.Quantity{
					v1.ResourceCPU: resource.MustParse("2"),
				}},
		})
		node := test.Node(test.NodeOptions{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
				v1alpha5.ProvisionerNameLabelKey: provisioner.Name,
				v1.LabelInstanceTypeStable:       cloudProvider.InstanceTypes[0].Name,
			}},
			Allocatable: map[v1.ResourceName]resource.Quantity{
				v1.ResourceCPU: resource.MustParse("4"),
			},
			ProviderID: test.RandomProviderID(),
		})
		ExpectApplied(ctx, env.Client, pod1, pod2)
		ExpectApplied(ctx, env.Client, node)

		ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))
		ExpectReconcileSucceeded(ctx, podController, client.ObjectKeyFromObject(pod1))
		ExpectReconcileSucceeded(ctx, podController, client.ObjectKeyFromObject(pod2))

		ExpectManualBinding(ctx, env.Client, pod1, node)
		ExpectReconcileSucceeded(ctx, podController, client.ObjectKeyFromObject(pod1))
		ExpectReconcileSucceeded(ctx, podController, client.ObjectKeyFromObject(pod2))

		ExpectResources(v1.ResourceList{v1.ResourceCPU: resource.MustParse("1.5")}, ExpectStateNodeExists(node).PodRequests())

		ExpectManualBinding(ctx, env.Client, pod2, node)
		ExpectReconcileSucceeded(ctx, podController, client.ObjectKeyFromObject(pod2))
		ExpectResources(v1.ResourceList{v1.ResourceCPU: resource.MustParse("3.5")}, ExpectStateNodeExists(node).PodRequests())
	})
	It("should count existing pods bound to nodes", func() {
		pod1 := test.UnschedulablePod(test.PodOptions{
			ResourceRequirements: v1.ResourceRequirements{
				Requests: map[v1.ResourceName]resource.Quantity{
					v1.ResourceCPU: resource.MustParse("1.5"),
				}},
		})
		pod2 := test.UnschedulablePod(test.PodOptions{
			ResourceRequirements: v1.ResourceRequirements{
				Requests: map[v1.ResourceName]resource.Quantity{
					v1.ResourceCPU: resource.MustParse("2"),
				}},
		})
		node := test.Node(test.NodeOptions{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
				v1alpha5.ProvisionerNameLabelKey: provisioner.Name,
				v1.LabelInstanceTypeStable:       cloudProvider.InstanceTypes[0].Name,
			}},
			Allocatable: map[v1.ResourceName]resource.Quantity{
				v1.ResourceCPU: resource.MustParse("4"),
			},
			ProviderID: test.RandomProviderID(),
		})

		// simulate a node that already exists in our cluster
		ExpectApplied(ctx, env.Client, pod1, pod2)
		ExpectApplied(ctx, env.Client, node)
		ExpectManualBinding(ctx, env.Client, pod1, node)
		ExpectManualBinding(ctx, env.Client, pod2, node)

		// that we just noticed
		ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))
		ExpectResources(v1.ResourceList{v1.ResourceCPU: resource.MustParse("3.5")}, ExpectStateNodeExists(node).PodRequests())
	})
	It("should subtract requests if the pod is deleted", func() {
		pod1 := test.UnschedulablePod(test.PodOptions{
			ResourceRequirements: v1.ResourceRequirements{
				Requests: map[v1.ResourceName]resource.Quantity{
					v1.ResourceCPU: resource.MustParse("1.5"),
				}},
		})
		pod2 := test.UnschedulablePod(test.PodOptions{
			ResourceRequirements: v1.ResourceRequirements{
				Requests: map[v1.ResourceName]resource.Quantity{
					v1.ResourceCPU: resource.MustParse("2"),
				}},
		})
		node := test.Node(test.NodeOptions{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
				v1alpha5.ProvisionerNameLabelKey: provisioner.Name,
				v1.LabelInstanceTypeStable:       cloudProvider.InstanceTypes[0].Name,
			}},
			Allocatable: map[v1.ResourceName]resource.Quantity{
				v1.ResourceCPU: resource.MustParse("4"),
			},
			ProviderID: test.RandomProviderID(),
		})
		ExpectApplied(ctx, env.Client, pod1, pod2)
		ExpectApplied(ctx, env.Client, node)

		ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))
		ExpectReconcileSucceeded(ctx, podController, client.ObjectKeyFromObject(pod1))
		ExpectReconcileSucceeded(ctx, podController, client.ObjectKeyFromObject(pod2))

		ExpectManualBinding(ctx, env.Client, pod1, node)
		ExpectManualBinding(ctx, env.Client, pod2, node)
		ExpectReconcileSucceeded(ctx, podController, client.ObjectKeyFromObject(pod1))
		ExpectReconcileSucceeded(ctx, podController, client.ObjectKeyFromObject(pod2))

		ExpectResources(v1.ResourceList{v1.ResourceCPU: resource.MustParse("3.5")}, ExpectStateNodeExists(node).PodRequests())

		// delete the pods and the CPU usage should go down
		ExpectDeleted(ctx, env.Client, pod2)
		ExpectReconcileSucceeded(ctx, podController, client.ObjectKeyFromObject(pod2))
		ExpectResources(v1.ResourceList{v1.ResourceCPU: resource.MustParse("1.5")}, ExpectStateNodeExists(node).PodRequests())

		ExpectDeleted(ctx, env.Client, pod1)
		ExpectReconcileSucceeded(ctx, podController, client.ObjectKeyFromObject(pod1))
		ExpectResources(v1.ResourceList{v1.ResourceCPU: resource.MustParse("0")}, ExpectStateNodeExists(node).PodRequests())
	})
	It("should not add requests if the pod is terminal", func() {
		pod1 := test.UnschedulablePod(test.PodOptions{
			ResourceRequirements: v1.ResourceRequirements{
				Requests: map[v1.ResourceName]resource.Quantity{
					v1.ResourceCPU: resource.MustParse("1.5"),
				}},
			Phase: v1.PodFailed,
		})
		pod2 := test.UnschedulablePod(test.PodOptions{
			ResourceRequirements: v1.ResourceRequirements{
				Requests: map[v1.ResourceName]resource.Quantity{
					v1.ResourceCPU: resource.MustParse("2"),
				}},
			Phase: v1.PodSucceeded,
		})
		node := test.Node(test.NodeOptions{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
				v1alpha5.ProvisionerNameLabelKey: provisioner.Name,
				v1.LabelInstanceTypeStable:       cloudProvider.InstanceTypes[0].Name,
			}},
			Allocatable: map[v1.ResourceName]resource.Quantity{
				v1.ResourceCPU: resource.MustParse("4"),
			},
			ProviderID: test.RandomProviderID(),
		})
		ExpectApplied(ctx, env.Client, pod1, pod2)
		ExpectApplied(ctx, env.Client, node)

		ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))
		ExpectReconcileSucceeded(ctx, podController, client.ObjectKeyFromObject(pod1))
		ExpectReconcileSucceeded(ctx, podController, client.ObjectKeyFromObject(pod2))

		ExpectManualBinding(ctx, env.Client, pod1, node)
		ExpectManualBinding(ctx, env.Client, pod2, node)
		ExpectReconcileSucceeded(ctx, podController, client.ObjectKeyFromObject(pod1))
		ExpectReconcileSucceeded(ctx, podController, client.ObjectKeyFromObject(pod2))

		ExpectResources(v1.ResourceList{v1.ResourceCPU: resource.MustParse("0")}, ExpectStateNodeExists(node).PodRequests())
	})
	It("should stop tracking nodes that are deleted", func() {
		pod1 := test.UnschedulablePod(test.PodOptions{
			ResourceRequirements: v1.ResourceRequirements{
				Requests: map[v1.ResourceName]resource.Quantity{
					v1.ResourceCPU: resource.MustParse("1.5"),
				}},
		})
		node := test.Node(test.NodeOptions{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
				v1alpha5.ProvisionerNameLabelKey: provisioner.Name,
				v1.LabelInstanceTypeStable:       cloudProvider.InstanceTypes[0].Name,
			}},
			Allocatable: map[v1.ResourceName]resource.Quantity{
				v1.ResourceCPU: resource.MustParse("4"),
			},
			ProviderID: test.RandomProviderID(),
		})
		ExpectApplied(ctx, env.Client, pod1)
		ExpectApplied(ctx, env.Client, node)

		ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))
		ExpectReconcileSucceeded(ctx, podController, client.ObjectKeyFromObject(pod1))

		ExpectManualBinding(ctx, env.Client, pod1, node)
		ExpectReconcileSucceeded(ctx, podController, client.ObjectKeyFromObject(pod1))

		cluster.ForEachNode(func(n *state.StateNode) bool {
			ExpectResources(v1.ResourceList{v1.ResourceCPU: resource.MustParse("2.5")}, n.Available())
			ExpectResources(v1.ResourceList{v1.ResourceCPU: resource.MustParse("1.5")}, n.PodRequests())
			return true
		})

		// delete the node and the internal state should disappear as well
		ExpectDeleted(ctx, env.Client, node)
		ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))
		cluster.ForEachNode(func(n *state.StateNode) bool {
			Fail("shouldn't be called as the node was deleted")
			return true
		})
	})
	It("should track pods correctly if we miss events or they are consolidated", func() {
		pod1 := test.UnschedulablePod(test.PodOptions{
			ObjectMeta: metav1.ObjectMeta{Name: "stateful-set-pod"},
			ResourceRequirements: v1.ResourceRequirements{
				Requests: map[v1.ResourceName]resource.Quantity{
					v1.ResourceCPU: resource.MustParse("1.5"),
				}},
		})

		node1 := test.Node(test.NodeOptions{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
				v1alpha5.ProvisionerNameLabelKey: provisioner.Name,
				v1.LabelInstanceTypeStable:       cloudProvider.InstanceTypes[0].Name,
			}},
			Allocatable: map[v1.ResourceName]resource.Quantity{
				v1.ResourceCPU: resource.MustParse("4"),
			},
			ProviderID: test.RandomProviderID(),
		})
		ExpectApplied(ctx, env.Client, pod1, node1)
		ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node1))
		ExpectReconcileSucceeded(ctx, podController, client.ObjectKeyFromObject(pod1))

		ExpectManualBinding(ctx, env.Client, pod1, node1)
		ExpectReconcileSucceeded(ctx, podController, client.ObjectKeyFromObject(pod1))

		cluster.ForEachNode(func(n *state.StateNode) bool {
			ExpectResources(v1.ResourceList{v1.ResourceCPU: resource.MustParse("2.5")}, n.Available())
			ExpectResources(v1.ResourceList{v1.ResourceCPU: resource.MustParse("1.5")}, n.PodRequests())
			return true
		})

		ExpectDeleted(ctx, env.Client, pod1)

		// second node has more capacity
		node2 := test.Node(test.NodeOptions{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
				v1alpha5.ProvisionerNameLabelKey: provisioner.Name,
				v1.LabelInstanceTypeStable:       cloudProvider.InstanceTypes[0].Name,
			}},
			Allocatable: map[v1.ResourceName]resource.Quantity{
				v1.ResourceCPU: resource.MustParse("8"),
			},
			ProviderID: test.RandomProviderID(),
		})

		// and the pod can only bind to node2 due to the resource request
		pod2 := test.UnschedulablePod(test.PodOptions{
			ObjectMeta: metav1.ObjectMeta{Name: "stateful-set-pod"},
			ResourceRequirements: v1.ResourceRequirements{
				Requests: map[v1.ResourceName]resource.Quantity{
					v1.ResourceCPU: resource.MustParse("5.0"),
				}},
		})

		ExpectApplied(ctx, env.Client, pod2, node2)
		ExpectManualBinding(ctx, env.Client, pod2, node2)
		// deleted the pod and then recreated it, but simulated only receiving an event on the new pod after it has
		// bound and not getting the new node event entirely
		ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node2))
		ExpectReconcileSucceeded(ctx, podController, client.ObjectKeyFromObject(pod2))

		cluster.ForEachNode(func(n *state.StateNode) bool {
			if n.Node.Name == node1.Name {
				// not on node1 any longer, so it should be fully free
				ExpectResources(v1.ResourceList{v1.ResourceCPU: resource.MustParse("4")}, n.Available())
				ExpectResources(v1.ResourceList{v1.ResourceCPU: resource.MustParse("0")}, n.PodRequests())
			} else {
				ExpectResources(v1.ResourceList{v1.ResourceCPU: resource.MustParse("3")}, n.Available())
				ExpectResources(v1.ResourceList{v1.ResourceCPU: resource.MustParse("5")}, n.PodRequests())
			}
			return true
		})

	})
	// nolint:gosec
	It("should maintain a correct count of resource usage as pods are deleted/added", func() {
		var pods []*v1.Pod
		for i := 0; i < 100; i++ {
			pods = append(pods, test.UnschedulablePod(test.PodOptions{
				ResourceRequirements: v1.ResourceRequirements{
					Requests: map[v1.ResourceName]resource.Quantity{
						v1.ResourceCPU: resource.MustParse(fmt.Sprintf("%1.1f", rand.Float64()*2)),
					}},
			}))
		}
		node := test.Node(test.NodeOptions{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
				v1alpha5.ProvisionerNameLabelKey: provisioner.Name,
				v1.LabelInstanceTypeStable:       cloudProvider.InstanceTypes[0].Name,
			}},
			Allocatable: map[v1.ResourceName]resource.Quantity{
				v1.ResourceCPU:  resource.MustParse("200"),
				v1.ResourcePods: resource.MustParse("500"),
			},
			ProviderID: test.RandomProviderID(),
		})
		ExpectApplied(ctx, env.Client, node)
		ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))
		ExpectResources(v1.ResourceList{
			v1.ResourceCPU:  resource.MustParse("0"),
			v1.ResourcePods: resource.MustParse("0"),
		}, ExpectStateNodeExists(node).PodRequests())
		ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))

		sum := 0.0
		podCount := 0
		for _, pod := range pods {
			ExpectApplied(ctx, env.Client, pod)
			ExpectManualBinding(ctx, env.Client, pod, node)
			podCount++

			// extra reconciles shouldn't cause it to be multiply counted
			nReconciles := rand.Intn(3) + 1 // 1 to 3 reconciles
			for i := 0; i < nReconciles; i++ {
				ExpectReconcileSucceeded(ctx, podController, client.ObjectKeyFromObject(pod))
			}
			sum += pod.Spec.Containers[0].Resources.Requests.Cpu().AsApproximateFloat64()
			ExpectResources(v1.ResourceList{
				v1.ResourceCPU:  resource.MustParse(fmt.Sprintf("%1.1f", sum)),
				v1.ResourcePods: resource.MustParse(fmt.Sprintf("%d", podCount)),
			}, ExpectStateNodeExists(node).PodRequests())
		}

		for _, pod := range pods {
			ExpectDeleted(ctx, env.Client, pod)
			nReconciles := rand.Intn(3) + 1
			// or multiply removed
			for i := 0; i < nReconciles; i++ {
				ExpectReconcileSucceeded(ctx, podController, client.ObjectKeyFromObject(pod))
			}
			sum -= pod.Spec.Containers[0].Resources.Requests.Cpu().AsApproximateFloat64()
			podCount--
			ExpectResources(v1.ResourceList{
				v1.ResourceCPU:  resource.MustParse(fmt.Sprintf("%1.1f", sum)),
				v1.ResourcePods: resource.MustParse(fmt.Sprintf("%d", podCount)),
			}, ExpectStateNodeExists(node).PodRequests())
		}
		ExpectResources(v1.ResourceList{
			v1.ResourceCPU:  resource.MustParse("0"),
			v1.ResourcePods: resource.MustParse("0"),
		}, ExpectStateNodeExists(node).PodRequests())
	})
	It("should track daemonset requested resources separately", func() {
		ds := test.DaemonSet(
			test.DaemonSetOptions{PodOptions: test.PodOptions{
				ResourceRequirements: v1.ResourceRequirements{Requests: v1.ResourceList{
					v1.ResourceCPU:    resource.MustParse("1"),
					v1.ResourceMemory: resource.MustParse("2Gi")}},
			}},
		)
		ExpectApplied(ctx, env.Client, ds)
		Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(ds), ds)).To(Succeed())

		pod1 := test.UnschedulablePod(test.PodOptions{
			ResourceRequirements: v1.ResourceRequirements{
				Requests: map[v1.ResourceName]resource.Quantity{
					v1.ResourceCPU: resource.MustParse("1.5"),
				}},
		})

		dsPod := test.UnschedulablePod(test.PodOptions{
			ResourceRequirements: v1.ResourceRequirements{
				Requests: map[v1.ResourceName]resource.Quantity{
					v1.ResourceCPU:    resource.MustParse("1"),
					v1.ResourceMemory: resource.MustParse("2Gi"),
				}},
		})
		dsPod.OwnerReferences = append(dsPod.OwnerReferences, metav1.OwnerReference{
			APIVersion:         "apps/v1",
			Kind:               "DaemonSet",
			Name:               ds.Name,
			UID:                ds.UID,
			Controller:         ptr.Bool(true),
			BlockOwnerDeletion: ptr.Bool(true),
		})

		node := test.Node(test.NodeOptions{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
				v1alpha5.ProvisionerNameLabelKey: provisioner.Name,
				v1.LabelInstanceTypeStable:       cloudProvider.InstanceTypes[0].Name,
			}},
			Allocatable: map[v1.ResourceName]resource.Quantity{
				v1.ResourceCPU:    resource.MustParse("4"),
				v1.ResourceMemory: resource.MustParse("8Gi"),
			},
			ProviderID: test.RandomProviderID(),
		})
		ExpectApplied(ctx, env.Client, pod1, node)
		ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))

		ExpectManualBinding(ctx, env.Client, pod1, node)
		ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))
		ExpectReconcileSucceeded(ctx, podController, client.ObjectKeyFromObject(pod1))

		// daemonset pod isn't bound yet
		ExpectResources(v1.ResourceList{
			v1.ResourceCPU:    resource.MustParse("0"),
			v1.ResourceMemory: resource.MustParse("0"),
		}, ExpectStateNodeExists(node).DaemonSetRequests())
		ExpectResources(v1.ResourceList{
			v1.ResourceCPU: resource.MustParse("1.5"),
		}, ExpectStateNodeExists(node).PodRequests())

		ExpectApplied(ctx, env.Client, dsPod)
		ExpectReconcileSucceeded(ctx, podController, client.ObjectKeyFromObject(dsPod))
		ExpectManualBinding(ctx, env.Client, dsPod, node)
		ExpectReconcileSucceeded(ctx, podController, client.ObjectKeyFromObject(dsPod))

		// just the DS request portion
		ExpectResources(v1.ResourceList{
			v1.ResourceCPU:    resource.MustParse("1"),
			v1.ResourceMemory: resource.MustParse("2Gi"),
		}, ExpectStateNodeExists(node).DaemonSetRequests())
		// total request
		ExpectResources(v1.ResourceList{
			v1.ResourceCPU:    resource.MustParse("2.5"),
			v1.ResourceMemory: resource.MustParse("2Gi"),
		}, ExpectStateNodeExists(node).PodRequests())
	})
	It("should mark node for deletion when node is deleted", func() {
		node := test.Node(test.NodeOptions{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1alpha5.ProvisionerNameLabelKey: provisioner.Name,
					v1.LabelInstanceTypeStable:       cloudProvider.InstanceTypes[0].Name,
				},
				Finalizers: []string{v1alpha5.TerminationFinalizer},
			},
			Allocatable: map[v1.ResourceName]resource.Quantity{
				v1.ResourceCPU: resource.MustParse("4"),
			},
			ProviderID: test.RandomProviderID(),
		})
		ExpectApplied(ctx, env.Client, node)

		ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))
		ExpectStateNodeCount("==", 1)

		Expect(env.Client.Delete(ctx, node)).To(Succeed())

		ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))
		ExpectNodeExists(ctx, env.Client, node.Name)
		Expect(ExpectStateNodeExists(node).MarkedForDeletion()).To(BeTrue())
	})
	It("should mark node for deletion when machine is deleted", func() {
		machine := test.Machine(v1alpha5.Machine{
			ObjectMeta: metav1.ObjectMeta{
				Finalizers: []string{v1alpha5.TerminationFinalizer},
			},
			Spec: v1alpha5.MachineSpec{
				Requirements: []v1.NodeSelectorRequirement{
					{
						Key:      v1.LabelInstanceTypeStable,
						Operator: v1.NodeSelectorOpIn,
						Values:   []string{cloudProvider.InstanceTypes[0].Name},
					},
					{
						Key:      v1.LabelTopologyZone,
						Operator: v1.NodeSelectorOpIn,
						Values:   []string{"test-zone-1"},
					},
				},
				MachineTemplateRef: &v1alpha5.MachineTemplateRef{
					Name: "default",
				},
			},
			Status: v1alpha5.MachineStatus{
				ProviderID: test.RandomProviderID(),
				Capacity: v1.ResourceList{
					v1.ResourceCPU:              resource.MustParse("2"),
					v1.ResourceMemory:           resource.MustParse("32Gi"),
					v1.ResourceEphemeralStorage: resource.MustParse("20Gi"),
				},
				Allocatable: v1.ResourceList{
					v1.ResourceCPU:              resource.MustParse("1"),
					v1.ResourceMemory:           resource.MustParse("30Gi"),
					v1.ResourceEphemeralStorage: resource.MustParse("18Gi"),
				},
			},
		})
		node := test.Node(test.NodeOptions{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1alpha5.ProvisionerNameLabelKey: provisioner.Name,
					v1.LabelInstanceTypeStable:       cloudProvider.InstanceTypes[0].Name,
				},
				Finalizers: []string{v1alpha5.TerminationFinalizer},
			},
			ProviderID: machine.Status.ProviderID,
		})
		ExpectApplied(ctx, env.Client, machine, node)
		ExpectReconcileSucceeded(ctx, machineController, client.ObjectKeyFromObject(machine))
		ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))
		ExpectStateNodeCount("==", 1)

		Expect(env.Client.Delete(ctx, machine)).To(Succeed())
		ExpectReconcileSucceeded(ctx, machineController, client.ObjectKeyFromObject(machine))
		ExpectExists(ctx, env.Client, machine)

		Expect(ExpectStateNodeExistsForMachine(machine).MarkedForDeletion()).To(BeTrue())
		Expect(ExpectStateNodeExists(node).MarkedForDeletion()).To(BeTrue())
	})
	It("should mark node for deletion when nodeclaim is deleted", func() {
		nodeClaim := test.NodeClaim(v1beta1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Finalizers: []string{v1alpha5.TerminationFinalizer},
			},
			Spec: v1beta1.NodeClaimSpec{
				Requirements: []v1.NodeSelectorRequirement{
					{
						Key:      v1.LabelInstanceTypeStable,
						Operator: v1.NodeSelectorOpIn,
						Values:   []string{cloudProvider.InstanceTypes[0].Name},
					},
					{
						Key:      v1.LabelTopologyZone,
						Operator: v1.NodeSelectorOpIn,
						Values:   []string{"test-zone-1"},
					},
				},
				NodeClass: &v1beta1.NodeClassReference{
					Name: "default",
				},
			},
			Status: v1beta1.NodeClaimStatus{
				ProviderID: test.RandomProviderID(),
				Capacity: v1.ResourceList{
					v1.ResourceCPU:              resource.MustParse("2"),
					v1.ResourceMemory:           resource.MustParse("32Gi"),
					v1.ResourceEphemeralStorage: resource.MustParse("20Gi"),
				},
				Allocatable: v1.ResourceList{
					v1.ResourceCPU:              resource.MustParse("1"),
					v1.ResourceMemory:           resource.MustParse("30Gi"),
					v1.ResourceEphemeralStorage: resource.MustParse("18Gi"),
				},
			},
		})
		node := test.Node(test.NodeOptions{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1beta1.NodePoolLabelKey:   nodePool.Name,
					v1.LabelInstanceTypeStable: cloudProvider.InstanceTypes[0].Name,
				},
				Finalizers: []string{v1alpha5.TerminationFinalizer},
			},
			ProviderID: nodeClaim.Status.ProviderID,
		})
		ExpectApplied(ctx, env.Client, nodeClaim, node)
		ExpectReconcileSucceeded(ctx, nodeClaimController, client.ObjectKeyFromObject(nodeClaim))
		ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))
		ExpectStateNodeCount("==", 1)

		Expect(env.Client.Delete(ctx, nodeClaim)).To(Succeed())
		ExpectReconcileSucceeded(ctx, nodeClaimController, client.ObjectKeyFromObject(nodeClaim))
		ExpectExists(ctx, env.Client, nodeClaim)

		Expect(ExpectStateNodeExistsForNodeClaim(nodeClaim).MarkedForDeletion()).To(BeTrue())
		Expect(ExpectStateNodeExists(node).MarkedForDeletion()).To(BeTrue())
	})
	It("should nominate the node until the nomination time passes", func() {
		node := test.Node(test.NodeOptions{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1alpha5.ProvisionerNameLabelKey: provisioner.Name,
					v1.LabelInstanceTypeStable:       cloudProvider.InstanceTypes[0].Name,
				},
				Finalizers: []string{v1alpha5.TerminationFinalizer},
			},
			Allocatable: map[v1.ResourceName]resource.Quantity{
				v1.ResourceCPU: resource.MustParse("4"),
			},
			ProviderID: test.RandomProviderID(),
		})
		ExpectApplied(ctx, env.Client, node)
		ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))

		cluster.NominateNodeForPod(ctx, node.Spec.ProviderID)

		// Expect that the node is now nominated
		Expect(ExpectStateNodeExists(node).Nominated()).To(BeTrue())
		time.Sleep(time.Second * 10) // nomination window is 20s so it should still be nominated
		Expect(ExpectStateNodeExists(node).Nominated()).To(BeTrue())
		time.Sleep(time.Second * 11) // past 20s, node should no longer be nominated
		Expect(ExpectStateNodeExists(node).Nominated()).To(BeFalse())
	})
	It("should handle a node changing from no providerID to registering a providerID", func() {
		node := test.Node()
		ExpectApplied(ctx, env.Client, node)
		ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))

		ExpectStateNodeCount("==", 1)
		ExpectStateNodeExists(node)

		// Change the providerID; this mocks CCM adding the providerID onto the node after registration
		node.Spec.ProviderID = fmt.Sprintf("fake://%s", node.Name)
		ExpectApplied(ctx, env.Client, node)
		ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))

		ExpectStateNodeCount("==", 1)
		ExpectStateNodeExists(node)
	})
})

var _ = Describe("Pod Anti-Affinity", func() {
	It("should track pods with required anti-affinity", func() {
		pod := test.UnschedulablePod(test.PodOptions{
			ResourceRequirements: v1.ResourceRequirements{
				Requests: map[v1.ResourceName]resource.Quantity{
					v1.ResourceCPU: resource.MustParse("1.5"),
				}},
			PodAntiRequirements: []v1.PodAffinityTerm{
				{
					LabelSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"foo": "bar"},
					},
					TopologyKey: v1.LabelTopologyZone,
				},
			},
		})

		node := test.Node(test.NodeOptions{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
				v1alpha5.ProvisionerNameLabelKey: provisioner.Name,
				v1.LabelInstanceTypeStable:       cloudProvider.InstanceTypes[0].Name,
			}},
			Allocatable: map[v1.ResourceName]resource.Quantity{
				v1.ResourceCPU: resource.MustParse("4"),
			},
			ProviderID: test.RandomProviderID(),
		})

		ExpectApplied(ctx, env.Client, pod)
		ExpectApplied(ctx, env.Client, node)
		ExpectManualBinding(ctx, env.Client, pod, node)

		ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))
		ExpectReconcileSucceeded(ctx, podController, client.ObjectKeyFromObject(pod))
		foundPodCount := 0
		cluster.ForPodsWithAntiAffinity(func(p *v1.Pod, n *v1.Node) bool {
			foundPodCount++
			Expect(p.Name).To(Equal(pod.Name))
			return true
		})
		Expect(foundPodCount).To(BeNumerically("==", 1))
	})
	It("should not track pods with preferred anti-affinity", func() {
		pod := test.UnschedulablePod(test.PodOptions{
			ResourceRequirements: v1.ResourceRequirements{
				Requests: map[v1.ResourceName]resource.Quantity{
					v1.ResourceCPU: resource.MustParse("1.5"),
				}},
			PodAntiPreferences: []v1.WeightedPodAffinityTerm{
				{
					Weight: 15,
					PodAffinityTerm: v1.PodAffinityTerm{
						LabelSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{"foo": "bar"},
						},
						TopologyKey: v1.LabelTopologyZone,
					},
				},
			},
		})

		node := test.Node(test.NodeOptions{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
				v1alpha5.ProvisionerNameLabelKey: provisioner.Name,
				v1.LabelInstanceTypeStable:       cloudProvider.InstanceTypes[0].Name,
			}},
			Allocatable: map[v1.ResourceName]resource.Quantity{
				v1.ResourceCPU: resource.MustParse("4"),
			},
			ProviderID: test.RandomProviderID(),
		})

		ExpectApplied(ctx, env.Client, pod)
		ExpectApplied(ctx, env.Client, node)
		ExpectManualBinding(ctx, env.Client, pod, node)

		ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))
		ExpectReconcileSucceeded(ctx, podController, client.ObjectKeyFromObject(pod))
		foundPodCount := 0
		cluster.ForPodsWithAntiAffinity(func(p *v1.Pod, n *v1.Node) bool {
			foundPodCount++
			Fail("shouldn't track pods with preferred anti-affinity")
			return true
		})
		Expect(foundPodCount).To(BeNumerically("==", 0))
	})
	It("should stop tracking pods with required anti-affinity if the pod is deleted", func() {
		pod := test.UnschedulablePod(test.PodOptions{
			ResourceRequirements: v1.ResourceRequirements{
				Requests: map[v1.ResourceName]resource.Quantity{
					v1.ResourceCPU: resource.MustParse("1.5"),
				}},
			PodAntiRequirements: []v1.PodAffinityTerm{
				{
					LabelSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"foo": "bar"},
					},
					TopologyKey: v1.LabelTopologyZone,
				},
			},
		})

		node := test.Node(test.NodeOptions{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
				v1alpha5.ProvisionerNameLabelKey: provisioner.Name,
				v1.LabelInstanceTypeStable:       cloudProvider.InstanceTypes[0].Name,
			}},
			Allocatable: map[v1.ResourceName]resource.Quantity{
				v1.ResourceCPU: resource.MustParse("4"),
			},
			ProviderID: test.RandomProviderID(),
		})

		ExpectApplied(ctx, env.Client, pod)
		ExpectApplied(ctx, env.Client, node)
		ExpectManualBinding(ctx, env.Client, pod, node)

		ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))
		ExpectReconcileSucceeded(ctx, podController, client.ObjectKeyFromObject(pod))
		foundPodCount := 0
		cluster.ForPodsWithAntiAffinity(func(p *v1.Pod, n *v1.Node) bool {
			foundPodCount++
			Expect(p.Name).To(Equal(pod.Name))
			return true
		})
		Expect(foundPodCount).To(BeNumerically("==", 1))

		ExpectDeleted(ctx, env.Client, client.Object(pod))
		ExpectReconcileSucceeded(ctx, podController, client.ObjectKeyFromObject(pod))
		foundPodCount = 0
		cluster.ForPodsWithAntiAffinity(func(p *v1.Pod, n *v1.Node) bool {
			foundPodCount++
			Fail("should not be called as the pod was deleted")
			return true
		})
		Expect(foundPodCount).To(BeNumerically("==", 0))
	})
	It("should handle events out of order", func() {
		pod := test.UnschedulablePod(test.PodOptions{
			ResourceRequirements: v1.ResourceRequirements{
				Requests: map[v1.ResourceName]resource.Quantity{
					v1.ResourceCPU: resource.MustParse("1.5"),
				}},
			PodAntiRequirements: []v1.PodAffinityTerm{
				{
					LabelSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"foo": "bar"},
					},
					TopologyKey: v1.LabelTopologyZone,
				},
			},
		})

		node := test.Node(test.NodeOptions{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
				v1alpha5.ProvisionerNameLabelKey: provisioner.Name,
				v1.LabelInstanceTypeStable:       cloudProvider.InstanceTypes[0].Name,
			}},
			Allocatable: map[v1.ResourceName]resource.Quantity{
				v1.ResourceCPU: resource.MustParse("4"),
			},
			ProviderID: test.RandomProviderID(),
		})

		ExpectApplied(ctx, env.Client, pod)
		ExpectApplied(ctx, env.Client, node)
		ExpectManualBinding(ctx, env.Client, pod, node)

		ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))
		ExpectReconcileSucceeded(ctx, podController, client.ObjectKeyFromObject(pod))

		// simulate receiving the node deletion before the pod deletion
		ExpectDeleted(ctx, env.Client, node)
		ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))

		foundPodCount := 0
		cluster.ForPodsWithAntiAffinity(func(p *v1.Pod, n *v1.Node) bool {
			foundPodCount++
			return true
		})
		Expect(foundPodCount).To(BeNumerically("==", 0))
	})
})

var _ = Describe("Cluster State Sync", func() {
	It("should consider the cluster state synced when all nodes are tracked", func() {
		// Deploy 1000 nodes and sync them all with the cluster
		for i := 0; i < 1000; i++ {
			node := test.Node(test.NodeOptions{
				ProviderID: test.RandomProviderID(),
			})
			ExpectApplied(ctx, env.Client, node)
			ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))
		}
		Expect(cluster.Synced(ctx)).To(BeTrue())
	})
	It("should consider the cluster state synced when nodes don't have provider id", func() {
		// Deploy 1000 nodes and sync them all with the cluster
		for i := 0; i < 1000; i++ {
			node := test.Node()
			ExpectApplied(ctx, env.Client, node)
			ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))
		}
		Expect(cluster.Synced(ctx)).To(BeTrue())
	})
	It("should consider the cluster state synced when nodes register provider id", func() {
		// Deploy 1000 nodes and sync them all with the cluster
		var nodes []*v1.Node
		for i := 0; i < 1000; i++ {
			nodes = append(nodes, test.Node())
			ExpectApplied(ctx, env.Client, nodes[i])
			ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(nodes[i]))
		}
		Expect(cluster.Synced(ctx)).To(BeTrue())
		for i := 0; i < 1000; i++ {
			nodes[i].Spec.ProviderID = test.RandomProviderID()
			ExpectApplied(ctx, env.Client, nodes[i])
			ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(nodes[i]))
		}
		Expect(cluster.Synced(ctx)).To(BeTrue())
	})
	It("should consider the cluster state synced when all machines are tracked", func() {
		// Deploy 1000 machines and sync them all with the cluster
		for i := 0; i < 1000; i++ {
			machine := test.Machine(v1alpha5.Machine{
				Status: v1alpha5.MachineStatus{
					ProviderID: test.RandomProviderID(),
				},
			})
			ExpectApplied(ctx, env.Client, machine)
			ExpectReconcileSucceeded(ctx, machineController, client.ObjectKeyFromObject(machine))
		}
		Expect(cluster.Synced(ctx)).To(BeTrue())
	})
	It("should consider the cluster state synced when all nodeclaims are tracked", func() {
		// Deploy 1000 machines and sync them all with the cluster
		for i := 0; i < 1000; i++ {
			machine := test.NodeClaim(v1beta1.NodeClaim{
				Status: v1beta1.NodeClaimStatus{
					ProviderID: test.RandomProviderID(),
				},
			})
			ExpectApplied(ctx, env.Client, machine)
			ExpectReconcileSucceeded(ctx, nodeClaimController, client.ObjectKeyFromObject(machine))
		}
		Expect(cluster.Synced(ctx)).To(BeTrue())
	})
	It("should consider the cluster state synced when a combination of machines and nodes are tracked", func() {
		// Deploy 250 nodes to the cluster that also have machines
		for i := 0; i < 250; i++ {
			node := test.Node(test.NodeOptions{
				ProviderID: test.RandomProviderID(),
			})
			machine := test.Machine(v1alpha5.Machine{
				Status: v1alpha5.MachineStatus{
					ProviderID: node.Spec.ProviderID,
				},
			})
			ExpectApplied(ctx, env.Client, node, machine)
			ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))
			ExpectReconcileSucceeded(ctx, machineController, client.ObjectKeyFromObject(machine))
		}
		// Deploy 250 nodes to the cluster
		for i := 0; i < 250; i++ {
			node := test.Node(test.NodeOptions{
				ProviderID: test.RandomProviderID(),
			})
			ExpectApplied(ctx, env.Client, node)
			ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))
		}
		// Deploy 500 machines and sync them all with the cluster
		for i := 0; i < 500; i++ {
			machine := test.Machine(v1alpha5.Machine{
				Status: v1alpha5.MachineStatus{
					ProviderID: test.RandomProviderID(),
				},
			})
			ExpectApplied(ctx, env.Client, machine)
			ExpectReconcileSucceeded(ctx, machineController, client.ObjectKeyFromObject(machine))
		}
		Expect(cluster.Synced(ctx)).To(BeTrue())
	})
	It("should consider the cluster state synced when a combination of nodeclaims and node are tracked", func() {
		// Deploy 250 nodes to the cluster that also have nodeclaims
		for i := 0; i < 250; i++ {
			node := test.Node(test.NodeOptions{
				ProviderID: test.RandomProviderID(),
			})
			nodeClaim := test.NodeClaim(v1beta1.NodeClaim{
				Status: v1beta1.NodeClaimStatus{
					ProviderID: node.Spec.ProviderID,
				},
			})
			ExpectApplied(ctx, env.Client, node, nodeClaim)
			ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))
			ExpectReconcileSucceeded(ctx, nodeClaimController, client.ObjectKeyFromObject(nodeClaim))
		}
		// Deploy 250 nodes to the cluster
		for i := 0; i < 250; i++ {
			node := test.Node(test.NodeOptions{
				ProviderID: test.RandomProviderID(),
			})
			ExpectApplied(ctx, env.Client, node)
			ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))
		}
		// Deploy 500 nodeclaims and sync them all with the cluster
		for i := 0; i < 500; i++ {
			nodeClaim := test.NodeClaim(v1beta1.NodeClaim{
				Status: v1beta1.NodeClaimStatus{
					ProviderID: test.RandomProviderID(),
				},
			})
			ExpectApplied(ctx, env.Client, nodeClaim)
			ExpectReconcileSucceeded(ctx, nodeClaimController, client.ObjectKeyFromObject(nodeClaim))
		}
		Expect(cluster.Synced(ctx)).To(BeTrue())
	})
	It("should consider the cluster state synced when a combination of machines, nodeclaims, and nodes are tracked", func() {
		// Deploy 250 nodes to the cluster that also have machines
		for i := 0; i < 250; i++ {
			node := test.Node(test.NodeOptions{
				ProviderID: test.RandomProviderID(),
			})
			machine := test.Machine(v1alpha5.Machine{
				Status: v1alpha5.MachineStatus{
					ProviderID: node.Spec.ProviderID,
				},
			})
			ExpectApplied(ctx, env.Client, node, machine)
			ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))
			ExpectReconcileSucceeded(ctx, machineController, client.ObjectKeyFromObject(machine))
		}
		// Deploy 250 nodes to the cluster that also have nodeclaims
		for i := 0; i < 250; i++ {
			node := test.Node(test.NodeOptions{
				ProviderID: test.RandomProviderID(),
			})
			nodeClaim := test.NodeClaim(v1beta1.NodeClaim{
				Status: v1beta1.NodeClaimStatus{
					ProviderID: node.Spec.ProviderID,
				},
			})
			ExpectApplied(ctx, env.Client, node, nodeClaim)
			ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))
			ExpectReconcileSucceeded(ctx, nodeClaimController, client.ObjectKeyFromObject(nodeClaim))
		}
		// Deploy 250 nodes to the cluster
		for i := 0; i < 250; i++ {
			node := test.Node(test.NodeOptions{
				ProviderID: test.RandomProviderID(),
			})
			ExpectApplied(ctx, env.Client, node)
			ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))
		}
		// Deploy 500 machines and sync them all with the cluster
		for i := 0; i < 500; i++ {
			machine := test.Machine(v1alpha5.Machine{
				Status: v1alpha5.MachineStatus{
					ProviderID: test.RandomProviderID(),
				},
			})
			ExpectApplied(ctx, env.Client, machine)
			ExpectReconcileSucceeded(ctx, machineController, client.ObjectKeyFromObject(machine))
		}
		// Deploy 500 nodeclaims and sync them all with the cluster
		for i := 0; i < 500; i++ {
			nodeClaim := test.NodeClaim(v1beta1.NodeClaim{
				Status: v1beta1.NodeClaimStatus{
					ProviderID: test.RandomProviderID(),
				},
			})
			ExpectApplied(ctx, env.Client, nodeClaim)
			ExpectReconcileSucceeded(ctx, nodeClaimController, client.ObjectKeyFromObject(nodeClaim))
		}
		Expect(cluster.Synced(ctx)).To(BeTrue())
	})
	It("should consider the cluster state synced when the representation of nodes is the same", func() {
		// Deploy 500 machines to the cluster, apply the linked nodes, but don't sync them
		for i := 0; i < 500; i++ {
			machine := test.Machine(v1alpha5.Machine{
				Status: v1alpha5.MachineStatus{
					ProviderID: test.RandomProviderID(),
				},
			})
			node := test.Node(test.NodeOptions{
				ProviderID: machine.Status.ProviderID,
			})
			ExpectApplied(ctx, env.Client, machine)
			ExpectApplied(ctx, env.Client, node)
			ExpectReconcileSucceeded(ctx, machineController, client.ObjectKeyFromObject(machine))
			ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))
		}
		Expect(cluster.Synced(ctx)).To(BeTrue())
	})
	It("should consider the cluster state synced when the representation of nodes is the same for nodeclaims", func() {
		// Deploy 500 machines to the cluster, apply the linked nodes, but don't sync them
		for i := 0; i < 500; i++ {
			nodeClaim := test.NodeClaim(v1beta1.NodeClaim{
				Status: v1beta1.NodeClaimStatus{
					ProviderID: test.RandomProviderID(),
				},
			})
			node := test.Node(test.NodeOptions{
				ProviderID: nodeClaim.Status.ProviderID,
			})
			ExpectApplied(ctx, env.Client, nodeClaim)
			ExpectApplied(ctx, env.Client, node)
			ExpectReconcileSucceeded(ctx, nodeClaimController, client.ObjectKeyFromObject(nodeClaim))
			ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))
		}
		Expect(cluster.Synced(ctx)).To(BeTrue())
	})
	It("shouldn't consider the cluster state synced if a machine hasn't resolved its provider id", func() {
		// Deploy 1000 machines and sync them all with the cluster
		for i := 0; i < 1000; i++ {
			machine := test.Machine(v1alpha5.Machine{
				Status: v1alpha5.MachineStatus{
					ProviderID: test.RandomProviderID(),
				},
			})
			// One of them doesn't have its providerID
			if i == 900 {
				machine.Status.ProviderID = ""
			}
			ExpectApplied(ctx, env.Client, machine)
			ExpectReconcileSucceeded(ctx, machineController, client.ObjectKeyFromObject(machine))
		}
		Expect(cluster.Synced(ctx)).To(BeFalse())
	})
	It("shouldn't consider the cluster state synced if a nodeclaim hasn't resolved its provider id", func() {
		// Deploy 1000 machines and sync them all with the cluster
		for i := 0; i < 1000; i++ {
			nodeClaim := test.NodeClaim(v1beta1.NodeClaim{
				Status: v1beta1.NodeClaimStatus{
					ProviderID: test.RandomProviderID(),
				},
			})
			// One of them doesn't have its providerID
			if i == 900 {
				nodeClaim.Status.ProviderID = ""
			}
			ExpectApplied(ctx, env.Client, nodeClaim)
			ExpectReconcileSucceeded(ctx, nodeClaimController, client.ObjectKeyFromObject(nodeClaim))
		}
		Expect(cluster.Synced(ctx)).To(BeFalse())
	})
	It("shouldn't consider the cluster state synced if a machine isn't tracked", func() {
		// Deploy 1000 machines and sync them all with the cluster
		for i := 0; i < 1000; i++ {
			machine := test.Machine(v1alpha5.Machine{
				Status: v1alpha5.MachineStatus{
					ProviderID: test.RandomProviderID(),
				},
			})
			ExpectApplied(ctx, env.Client, machine)

			// One of them doesn't get synced with the reconciliation
			if i != 900 {
				ExpectReconcileSucceeded(ctx, machineController, client.ObjectKeyFromObject(machine))
			}
		}
		Expect(cluster.Synced(ctx)).To(BeFalse())
	})
	It("shouldn't consider the cluster state synced if a nodeclaim isn't tracked", func() {
		// Deploy 1000 machines and sync them all with the cluster
		for i := 0; i < 1000; i++ {
			nodeClaim := test.NodeClaim(v1beta1.NodeClaim{
				Status: v1beta1.NodeClaimStatus{
					ProviderID: test.RandomProviderID(),
				},
			})
			ExpectApplied(ctx, env.Client, nodeClaim)

			// One of them doesn't get synced with the reconciliation
			if i != 900 {
				ExpectReconcileSucceeded(ctx, nodeClaimController, client.ObjectKeyFromObject(nodeClaim))
			}
		}
		Expect(cluster.Synced(ctx)).To(BeFalse())
	})
	It("shouldn't consider the cluster state synced if a node isn't tracked", func() {
		// Deploy 1000 nodes and sync them all with the cluster
		for i := 0; i < 1000; i++ {
			node := test.Node(test.NodeOptions{
				ProviderID: test.RandomProviderID(),
			})
			ExpectApplied(ctx, env.Client, node)

			// One of them doesn't get synced with the reconciliation
			if i != 900 {
				ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))
			}
		}
		Expect(cluster.Synced(ctx)).To(BeFalse())
	})
})

var _ = Describe("DaemonSet Controller", func() {
	It("should not update daemonsetCache when daemonset pod is not present", func() {
		daemonset := test.DaemonSet(
			test.DaemonSetOptions{PodOptions: test.PodOptions{
				ResourceRequirements: v1.ResourceRequirements{Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("1"), v1.ResourceMemory: resource.MustParse("1Gi")}},
			}},
		)
		ExpectApplied(ctx, env.Client, daemonset)
		ExpectReconcileSucceeded(ctx, daemonsetController, client.ObjectKeyFromObject(daemonset))
		daemonsetPod := cluster.GetDaemonSetPod(daemonset)
		Expect(daemonsetPod).To(BeNil())
	})
	It("should update daemonsetCache when daemonset pod is created", func() {
		daemonset := test.DaemonSet(
			test.DaemonSetOptions{PodOptions: test.PodOptions{
				ResourceRequirements: v1.ResourceRequirements{Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("1"), v1.ResourceMemory: resource.MustParse("1Gi")}},
			}},
		)
		ExpectApplied(ctx, env.Client, daemonset)
		daemonsetPod := test.UnschedulablePod(
			test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion:         "apps/v1",
							Kind:               "DaemonSet",
							Name:               daemonset.Name,
							UID:                daemonset.UID,
							Controller:         ptr.Bool(true),
							BlockOwnerDeletion: ptr.Bool(true),
						},
					},
				},
			})
		daemonsetPod.Spec = daemonset.Spec.Template.Spec
		ExpectApplied(ctx, env.Client, daemonsetPod)
		ExpectReconcileSucceeded(ctx, daemonsetController, client.ObjectKeyFromObject(daemonset))

		Expect(cluster.GetDaemonSetPod(daemonset)).To(Equal(daemonsetPod))
	})
	It("should update daemonsetCache with the newest created pod", func() {
		daemonset := test.DaemonSet(
			test.DaemonSetOptions{PodOptions: test.PodOptions{
				ResourceRequirements: v1.ResourceRequirements{Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("1"), v1.ResourceMemory: resource.MustParse("1Gi")}},
			}},
		)
		ExpectApplied(ctx, env.Client, daemonset)
		daemonsetPod1 := test.UnschedulablePod(
			test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion:         "apps/v1",
							Kind:               "DaemonSet",
							Name:               daemonset.Name,
							UID:                daemonset.UID,
							Controller:         ptr.Bool(true),
							BlockOwnerDeletion: ptr.Bool(true),
						},
					},
				},
			})
		daemonsetPod1.Spec = daemonset.Spec.Template.Spec
		ExpectApplied(ctx, env.Client, daemonsetPod1)
		ExpectReconcileSucceeded(ctx, daemonsetController, client.ObjectKeyFromObject(daemonset))

		Expect(cluster.GetDaemonSetPod(daemonset)).To(Equal(daemonsetPod1))

		daemonsetPod2 := test.UnschedulablePod(
			test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion:         "apps/v1",
							Kind:               "DaemonSet",
							Name:               daemonset.Name,
							UID:                daemonset.UID,
							Controller:         ptr.Bool(true),
							BlockOwnerDeletion: ptr.Bool(true),
						},
					},
				},
			})
		time.Sleep(time.Second) // Making sure the two pods have different creationTime
		daemonsetPod2.Spec = daemonset.Spec.Template.Spec
		ExpectApplied(ctx, env.Client, daemonsetPod2)
		ExpectReconcileSucceeded(ctx, daemonsetController, client.ObjectKeyFromObject(daemonset))
		Expect(cluster.GetDaemonSetPod(daemonset)).To(Equal(daemonsetPod2))
	})
	It("should delete daemonset in cache when daemonset is deleted", func() {
		daemonset := test.DaemonSet(
			test.DaemonSetOptions{PodOptions: test.PodOptions{
				ResourceRequirements: v1.ResourceRequirements{Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("1"), v1.ResourceMemory: resource.MustParse("1Gi")}},
			}},
		)
		ExpectApplied(ctx, env.Client, daemonset)
		daemonsetPod := test.UnschedulablePod(
			test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion:         "apps/v1",
							Kind:               "DaemonSet",
							Name:               daemonset.Name,
							UID:                daemonset.UID,
							Controller:         ptr.Bool(true),
							BlockOwnerDeletion: ptr.Bool(true),
						},
					},
				},
			})
		daemonsetPod.Spec = daemonset.Spec.Template.Spec
		ExpectApplied(ctx, env.Client, daemonsetPod)
		ExpectReconcileSucceeded(ctx, daemonsetController, client.ObjectKeyFromObject(daemonset))

		Expect(cluster.GetDaemonSetPod(daemonset)).To(Equal(daemonsetPod))

		ExpectDeleted(ctx, env.Client, daemonset, daemonsetPod)
		ExpectReconcileSucceeded(ctx, daemonsetController, client.ObjectKeyFromObject(daemonset))

		Expect(cluster.GetDaemonSetPod(daemonset)).To(BeNil())
	})
})

var _ = Describe("Consolidated State", func() {
	It("should update the consolidated value when setting consolidation", func() {
		state := cluster.ConsolidationState()
		Expect(cluster.ConsolidationState()).To(Equal(state))

		// time must pass
		fakeClock.Step(1 * time.Second)

		cluster.MarkUnconsolidated()
		Expect(cluster.ConsolidationState()).ToNot(Equal(state))
	})
	It("should update the consolidated value when consolidation timeout (5m) has passed and state hasn't changed", func() {
		state := cluster.ConsolidationState()

		fakeClock.Step(time.Minute)
		Expect(cluster.ConsolidationState()).To(Equal(state))

		fakeClock.Step(time.Minute * 3)
		Expect(cluster.ConsolidationState()).To(Equal(state))

		fakeClock.Step(time.Minute * 2)
		Expect(cluster.ConsolidationState()).ToNot(Equal(state))
	})
	It("should cause consolidation state to change when a Provisioner is updated", func() {
		cluster.MarkUnconsolidated()
		fakeClock.Step(time.Minute)
		ExpectApplied(ctx, env.Client, provisioner)
		state := cluster.ConsolidationState()
		ExpectReconcileSucceeded(ctx, provisionerController, client.ObjectKeyFromObject(provisioner))
		Expect(cluster.ConsolidationState()).ToNot(Equal(state))
	})
	It("should cause consolidation state to change when a NodePool is updated", func() {
		cluster.MarkUnconsolidated()
		fakeClock.Step(time.Minute)
		ExpectApplied(ctx, env.Client, nodePool)
		state := cluster.ConsolidationState()
		ExpectReconcileSucceeded(ctx, nodePoolController, client.ObjectKeyFromObject(nodePool))
		Expect(cluster.ConsolidationState()).ToNot(Equal(state))
	})
})

func ExpectStateNodeCount(comparator string, count int) int {
	GinkgoHelper()
	c := 0
	cluster.ForEachNode(func(n *state.StateNode) bool {
		c++
		return true
	})
	Expect(c).To(BeNumerically(comparator, count))
	return c
}

func ExpectStateNodeExists(node *v1.Node) *state.StateNode {
	GinkgoHelper()
	var ret *state.StateNode
	cluster.ForEachNode(func(n *state.StateNode) bool {
		if n.Node.Name != node.Name {
			return true
		}
		ret = n.DeepCopy()
		return false
	})
	Expect(ret).ToNot(BeNil())
	return ret
}

func ExpectStateNodeExistsForMachine(machine *v1alpha5.Machine) *state.StateNode {
	GinkgoHelper()
	var ret *state.StateNode
	cluster.ForEachNode(func(n *state.StateNode) bool {
		if n.NodeClaim.Status.ProviderID != machine.Status.ProviderID {
			return true
		}
		ret = n.DeepCopy()
		return false
	})
	Expect(ret).ToNot(BeNil())
	return ret
}

func ExpectStateNodeExistsForNodeClaim(nodeClaim *v1beta1.NodeClaim) *state.StateNode {
	GinkgoHelper()
	var ret *state.StateNode
	cluster.ForEachNode(func(n *state.StateNode) bool {
		if n.NodeClaim.Status.ProviderID != nodeClaim.Status.ProviderID {
			return true
		}
		ret = n.DeepCopy()
		return false
	})
	Expect(ret).ToNot(BeNil())
	return ret
}

func ExpectStateNodeNotFoundForMachine(machine *v1alpha5.Machine) *state.StateNode {
	GinkgoHelper()
	var ret *state.StateNode
	cluster.ForEachNode(func(n *state.StateNode) bool {
		if n.NodeClaim.Status.ProviderID != machine.Status.ProviderID {
			return true
		}
		ret = n.DeepCopy()
		return false
	})
	Expect(ret).To(BeNil())
	return ret
}

func ExpectStateNodeNotFoundForNodeClaim(nodeClaim *v1beta1.NodeClaim) *state.StateNode {
	GinkgoHelper()
	var ret *state.StateNode
	cluster.ForEachNode(func(n *state.StateNode) bool {
		if n.NodeClaim.Status.ProviderID != nodeClaim.Status.ProviderID {
			return true
		}
		ret = n.DeepCopy()
		return false
	})
	Expect(ret).To(BeNil())
	return ret
}
