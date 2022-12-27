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
	"testing"
	"time"

	clock "k8s.io/utils/clock/testing"
	"knative.dev/pkg/ptr"

	"github.com/aws/karpenter-core/pkg/apis"
	"github.com/aws/karpenter-core/pkg/apis/config/settings"
	"github.com/aws/karpenter-core/pkg/apis/v1alpha1"
	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/operator/controller"
	"github.com/aws/karpenter-core/pkg/operator/scheme"

	"github.com/aws/karpenter-core/pkg/cloudprovider/fake"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/aws/karpenter-core/pkg/controllers/state"
	"github.com/aws/karpenter-core/pkg/utils/resources"

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
var nodeController controller.Controller
var podController controller.Controller
var machineController controller.Controller
var provisionerController controller.Controller
var cloudProvider *fake.CloudProvider
var provisioner *v1alpha5.Provisioner

func TestAPIs(t *testing.T) {
	ctx = TestContextWithLogger(t)
	RegisterFailHandler(Fail)
	RunSpecs(t, "Controllers/State")
}

var _ = BeforeSuite(func() {
	env = test.NewEnvironment(scheme.Scheme, apis.CRDs...)
})

var _ = AfterSuite(func() {
	Expect(env.Stop()).To(Succeed(), "Failed to stop environment")
})

var _ = BeforeEach(func() {
	ctx = settings.ToContext(ctx, test.Settings())
	cloudProvider = fake.NewCloudProvider()
	cloudProvider.InstanceTypes = fake.InstanceTypesAssorted()
	fakeClock = clock.NewFakeClock(time.Now())
	cluster = state.NewCluster(ctx, fakeClock, env.Client, cloudProvider)
	nodeController = state.NewNodeController(env.Client, cluster)
	podController = state.NewPodController(env.Client, cluster)
	machineController = state.NewMachineController(env.Client, cluster)
	provisionerController = state.NewProvisionerController(env.Client, cluster)
	provisioner = test.Provisioner(test.ProvisionerOptions{ObjectMeta: metav1.ObjectMeta{Name: "default"}})
	ExpectApplied(ctx, env.Client, provisioner)
})

var _ = AfterEach(func() {
	ExpectCleanedUp(ctx, env.Client)
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
			}})
		ExpectApplied(ctx, env.Client, pod1, pod2)
		ExpectApplied(ctx, env.Client, node)

		ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))
		ExpectReconcileSucceeded(ctx, podController, client.ObjectKeyFromObject(pod1))
		ExpectReconcileSucceeded(ctx, podController, client.ObjectKeyFromObject(pod2))

		// two pods, but neither is bound to the node so the node's CPU requests should be zero
		ExpectNodeResourceRequest(node.Name, v1.ResourceCPU, "0.0")
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
			}})
		ExpectApplied(ctx, env.Client, pod1, pod2)
		ExpectApplied(ctx, env.Client, node)

		ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))
		ExpectReconcileSucceeded(ctx, podController, client.ObjectKeyFromObject(pod1))
		ExpectReconcileSucceeded(ctx, podController, client.ObjectKeyFromObject(pod2))

		ExpectManualBinding(ctx, env.Client, pod1, node)
		ExpectReconcileSucceeded(ctx, podController, client.ObjectKeyFromObject(pod1))
		ExpectReconcileSucceeded(ctx, podController, client.ObjectKeyFromObject(pod2))

		ExpectNodeResourceRequest(node.Name, v1.ResourceCPU, "1.5")

		ExpectManualBinding(ctx, env.Client, pod2, node)
		ExpectReconcileSucceeded(ctx, podController, client.ObjectKeyFromObject(pod2))
		ExpectNodeResourceRequest(node.Name, v1.ResourceCPU, "3.5")
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
			}})

		// simulate a node that already exists in our cluster
		ExpectApplied(ctx, env.Client, pod1, pod2)
		ExpectApplied(ctx, env.Client, node)
		ExpectManualBinding(ctx, env.Client, pod1, node)
		ExpectManualBinding(ctx, env.Client, pod2, node)

		// that we just noticed
		ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))
		ExpectNodeResourceRequest(node.Name, v1.ResourceCPU, "3.5")
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
			}})
		ExpectApplied(ctx, env.Client, pod1, pod2)
		ExpectApplied(ctx, env.Client, node)

		ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))
		ExpectReconcileSucceeded(ctx, podController, client.ObjectKeyFromObject(pod1))
		ExpectReconcileSucceeded(ctx, podController, client.ObjectKeyFromObject(pod2))

		ExpectManualBinding(ctx, env.Client, pod1, node)
		ExpectManualBinding(ctx, env.Client, pod2, node)
		ExpectReconcileSucceeded(ctx, podController, client.ObjectKeyFromObject(pod1))
		ExpectReconcileSucceeded(ctx, podController, client.ObjectKeyFromObject(pod2))

		ExpectNodeResourceRequest(node.Name, v1.ResourceCPU, "3.5")

		// delete the pods and the CPU usage should go down
		ExpectDeleted(ctx, env.Client, pod2)
		ExpectReconcileSucceeded(ctx, podController, client.ObjectKeyFromObject(pod2))
		ExpectNodeResourceRequest(node.Name, v1.ResourceCPU, "1.5")

		ExpectDeleted(ctx, env.Client, pod1)
		ExpectReconcileSucceeded(ctx, podController, client.ObjectKeyFromObject(pod1))
		ExpectNodeResourceRequest(node.Name, v1.ResourceCPU, "0")
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

		ExpectNodeResourceRequest(node.Name, v1.ResourceCPU, "0")
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
			}})
		ExpectApplied(ctx, env.Client, pod1)
		ExpectApplied(ctx, env.Client, node)

		ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))
		ExpectReconcileSucceeded(ctx, podController, client.ObjectKeyFromObject(pod1))

		ExpectManualBinding(ctx, env.Client, pod1, node)
		ExpectReconcileSucceeded(ctx, podController, client.ObjectKeyFromObject(pod1))

		cluster.ForEachNode(func(n *state.Node) bool {
			available := n.Available
			requested := resources.Subtract(n.Node.Status.Allocatable, available)
			Expect(available.Cpu().AsApproximateFloat64()).To(BeNumerically("~", 2.5))
			Expect(requested.Cpu().AsApproximateFloat64()).To(BeNumerically("~", 1.5))
			return true
		})

		// delete the node and the internal state should disappear as well
		ExpectDeleted(ctx, env.Client, node)
		ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))
		cluster.ForEachNode(func(n *state.Node) bool {
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
			}})
		ExpectApplied(ctx, env.Client, pod1, node1)
		ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node1))
		ExpectReconcileSucceeded(ctx, podController, client.ObjectKeyFromObject(pod1))

		ExpectManualBinding(ctx, env.Client, pod1, node1)
		ExpectReconcileSucceeded(ctx, podController, client.ObjectKeyFromObject(pod1))

		cluster.ForEachNode(func(n *state.Node) bool {
			available := n.Available
			requested := resources.Subtract(n.Node.Status.Allocatable, available)
			Expect(available.Cpu().AsApproximateFloat64()).To(BeNumerically("~", 2.5))
			Expect(requested.Cpu().AsApproximateFloat64()).To(BeNumerically("~", 1.5))
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
			}})

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

		ExpectReconcileSucceeded(ctx, podController, client.ObjectKeyFromObject(pod2))
		ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node2))
		ExpectReconcileSucceeded(ctx, podController, client.ObjectKeyFromObject(pod2))

		cluster.ForEachNode(func(n *state.Node) bool {
			available := n.Available
			requested := resources.Subtract(n.Node.Status.Allocatable, available)
			if n.Node.Name == node1.Name {
				// not on node1 any longer, so it should be fully free
				Expect(available.Cpu().AsApproximateFloat64()).To(BeNumerically("~", 4))
				Expect(requested.Cpu().AsApproximateFloat64()).To(BeNumerically("~", 0))
			} else {
				Expect(available.Cpu().AsApproximateFloat64()).To(BeNumerically("~", 3.0))
				Expect(requested.Cpu().AsApproximateFloat64()).To(BeNumerically("~", 5.0))
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
			}})
		ExpectApplied(ctx, env.Client, node)
		ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))
		ExpectNodeResourceRequest(node.Name, v1.ResourceCPU, "0.0")
		ExpectNodeResourceRequest(node.Name, v1.ResourcePods, "0")

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
			ExpectNodeResourceRequest(node.Name, v1.ResourceCPU, fmt.Sprintf("%1.1f", sum))
			ExpectNodeResourceRequest(node.Name, v1.ResourcePods, fmt.Sprintf("%d", podCount))
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
			ExpectNodeResourceRequest(node.Name, v1.ResourceCPU, fmt.Sprintf("%1.1f", sum))
			ExpectNodeResourceRequest(node.Name, v1.ResourcePods, fmt.Sprintf("%d", podCount))
		}
		ExpectNodeResourceRequest(node.Name, v1.ResourceCPU, "0.0")
		ExpectNodeResourceRequest(node.Name, v1.ResourcePods, "0")
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
			}})
		ExpectApplied(ctx, env.Client, pod1, node)
		ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))

		ExpectManualBinding(ctx, env.Client, pod1, node)
		ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))
		ExpectReconcileSucceeded(ctx, podController, client.ObjectKeyFromObject(pod1))

		// daemonset pod isn't bound yet
		ExpectNodeDaemonSetRequested(node.Name, v1.ResourceCPU, "0")
		ExpectNodeDaemonSetRequested(node.Name, v1.ResourceMemory, "0")
		ExpectNodeResourceRequest(node.Name, v1.ResourceCPU, "1.5")

		ExpectApplied(ctx, env.Client, dsPod)
		ExpectReconcileSucceeded(ctx, podController, client.ObjectKeyFromObject(dsPod))
		ExpectManualBinding(ctx, env.Client, dsPod, node)
		ExpectReconcileSucceeded(ctx, podController, client.ObjectKeyFromObject(dsPod))

		// just the DS request portion
		ExpectNodeDaemonSetRequested(node.Name, v1.ResourceCPU, "1")
		ExpectNodeDaemonSetRequested(node.Name, v1.ResourceMemory, "2Gi")
		// total request
		ExpectNodeResourceRequest(node.Name, v1.ResourceCPU, "2.5")
		ExpectNodeResourceRequest(node.Name, v1.ResourceMemory, "2Gi")
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
			}},
		)
		ExpectApplied(ctx, env.Client, node)

		ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))
		Expect(env.Client.Delete(ctx, node)).To(Succeed())

		ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))
		ExpectNodeExists(ctx, env.Client, node.Name)
		ExpectNodeDeletionMarked(node.Name)
	})
})

var _ = Describe("Machine (in-flight) Level", func() {
	It("should create an in-flight node when the machine is created", func() {
		machine := test.Machine(test.MachineOptions{
			Allocatable: v1.ResourceList{
				v1.ResourceCPU:    resource.MustParse("2"),
				v1.ResourceMemory: resource.MustParse("2Mi"),
			},
			ProviderID: "test",
		})
		ExpectApplied(ctx, env.Client, machine.DeepCopy())

		ExpectReconcileSucceeded(ctx, machineController, client.ObjectKeyFromObject(machine))
		ExpectNodeResourceRequest(machine.Name, v1.ResourceCPU, "2")
		ExpectNodeResourceRequest(machine.Name, v1.ResourceMemory, "2Mi")
	})
	It("should use machine allocatable until the machine is initialized", func() {
		machine := test.Machine(test.MachineOptions{
			Allocatable: v1.ResourceList{
				v1.ResourceCPU:    resource.MustParse("2"),
				v1.ResourceMemory: resource.MustParse("2Mi"),
			},
			ProviderID: "test",
		})
		node := test.Node(test.NodeOptions{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
				v1alpha5.MachineNameLabelKey: machine.Name,
			}},
			Allocatable: map[v1.ResourceName]resource.Quantity{
				v1.ResourceCPU:    resource.MustParse("4"),
				v1.ResourceMemory: resource.MustParse("4Mi"),
			},
			ProviderID: "test",
		})
		// We deep-copy the machine here since we need the status
		ExpectApplied(ctx, env.Client, node, machine.DeepCopy())
		ExpectReconcileSucceeded(ctx, machineController, client.ObjectKeyFromObject(machine))
		ExpectReconcileSucceeded(ctx, nodeController, client.ObjectKeyFromObject(node))

		Expect(Nodes()).To(HaveLen(1))
		ExpectNodeResourceRequest(machine.Name, v1.ResourceCPU, "2")
		ExpectNodeResourceRequest(machine.Name, v1.ResourceMemory, "2Mi")

		machine.StatusConditions().MarkTrue(v1alpha1.MachineInitialized)
		ExpectApplied(ctx, env.Client, machine)
		ExpectReconcileSucceeded(ctx, machineController, client.ObjectKeyFromObject(machine))

		Expect(Nodes()).To(HaveLen(1))
		ExpectNodeResourceRequest(machine.Name, v1.ResourceCPU, "4")
		ExpectNodeResourceRequest(machine.Name, v1.ResourceMemory, "4Mi")
	})
	It("should migrate an in-flight node to an actual node in state", func() {
		machine := test.Machine(test.MachineOptions{
			Allocatable: v1.ResourceList{
				v1.ResourceCPU:    resource.MustParse("2"),
				v1.ResourceMemory: resource.MustParse("2Mi"),
			},
			ProviderID: "test",
		})
		ExpectApplied(ctx, env.Client, machine.DeepCopy())
		ExpectReconcileSucceeded(ctx, machineController, client.ObjectKeyFromObject(machine))

		Expect(Nodes()).To(HaveLen(1))

		node := test.Node(test.NodeOptions{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
				v1alpha5.MachineNameLabelKey: machine.Name,
			}},
			Allocatable: map[v1.ResourceName]resource.Quantity{
				v1.ResourceCPU:    resource.MustParse("4"),
				v1.ResourceMemory: resource.MustParse("4Mi"),
			}},
		)
		ExpectApplied(ctx, env.Client, node)
		machine.StatusConditions().MarkTrue(v1alpha1.MachineInitialized)
		ExpectReconcileSucceeded(ctx, machineController, client.ObjectKeyFromObject(node))

		// In-flight node should be overridden with the actual node
		Expect(Nodes()).To(HaveLen(1))
		ExpectNodeResourceRequest(node.Name, v1.ResourceCPU, "4")
		ExpectNodeResourceRequest(node.Name, v1.ResourceMemory, "4Mi")
	})
	It("should have MarkedForDeletion for an in-flight node", func() {
		machine := test.Machine(test.MachineOptions{
			Allocatable: v1.ResourceList{
				v1.ResourceCPU:    resource.MustParse("2"),
				v1.ResourceMemory: resource.MustParse("2Mi"),
			},
			ProviderID: "test",
		})
		ExpectApplied(ctx, env.Client, machine.DeepCopy())
		ExpectReconcileSucceeded(ctx, machineController, client.ObjectKeyFromObject(machine))

		cluster.MarkForDeletion(machine.Name)

		// Node should be marked for deletion
		nodes := Nodes()
		Expect(nodes).To(HaveLen(1))
		Expect(nodes[0].MarkedForDeletion).To(BeTrue())

		// Update the machine and keep the MarkedForDeletion
		machine.Labels["test-label"] = "test-value"
		ExpectApplied(ctx, env.Client, machine.DeepCopy())
		ExpectReconcileSucceeded(ctx, machineController, client.ObjectKeyFromObject(machine))

		// Node should maintain marked for deletion
		nodes = Nodes()
		Expect(nodes).To(HaveLen(1))
		Expect(nodes[0].MarkedForDeletion).To(BeTrue())
	})
	It("should maintain MarkedForDeletion when moving from in-flight to real node", func() {
		machine := test.Machine(test.MachineOptions{
			Allocatable: v1.ResourceList{
				v1.ResourceCPU:    resource.MustParse("2"),
				v1.ResourceMemory: resource.MustParse("2Mi"),
			},
		})
		ExpectApplied(ctx, env.Client, machine.DeepCopy())
		ExpectReconcileSucceeded(ctx, machineController, client.ObjectKeyFromObject(machine))

		cluster.MarkForDeletion(machine.Name)
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
			}})

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
			}})

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
			}})

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
			}})

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

var _ = Describe("Provisioner Spec Updates", func() {
	It("should cause consolidation state to change when a provisioner is updated", func() {
		oldConsolidationState := cluster.ClusterConsolidationState()
		fakeClock.Step(time.Minute)
		provisioner.Spec.Consolidation = &v1alpha5.Consolidation{Enabled: ptr.Bool(true)}
		ExpectApplied(ctx, env.Client, provisioner)
		ExpectReconcileSucceeded(ctx, provisionerController, client.ObjectKeyFromObject(provisioner))

		Expect(oldConsolidationState).To(BeNumerically("<", cluster.ClusterConsolidationState()))
	})
})

func Nodes() []*state.Node {
	var nodes []*state.Node
	cluster.ForEachNode(func(n *state.Node) bool {
		nodes = append(nodes, n)
		return true
	})
	return nodes
}

func ExpectNodeResourceRequest(name string, resourceName v1.ResourceName, amount string) {
	cluster.ForEachNode(func(n *state.Node) bool {
		if n.Node.Name != name {
			return true
		}
		requested := resources.Subtract(n.Node.Status.Allocatable, n.Available)

		nodeRequest := requested[resourceName]
		expected := resource.MustParse(amount)
		ExpectWithOffset(1, nodeRequest.AsApproximateFloat64()).To(BeNumerically("~", expected.AsApproximateFloat64(), 0.001))
		return false
	})
}

func ExpectNodeDaemonSetRequested(name string, resourceName v1.ResourceName, amount string) {
	cluster.ForEachNode(func(n *state.Node) bool {
		if n.Node.Name != name {
			return true
		}
		dsReq := n.DaemonSetRequested[resourceName]
		expected := resource.MustParse(amount)
		ExpectWithOffset(1, dsReq.AsApproximateFloat64()).To(BeNumerically("~", expected.AsApproximateFloat64(), 0.001))
		return false
	})
}

func ExpectNodeDeletionMarked(name string) {
	cluster.ForEachNode(func(n *state.Node) bool {
		if n.Node.Name != name {
			return true
		}
		ExpectWithOffset(1, n.MarkedForDeletion).To(BeTrue())
		return false
	})
}
