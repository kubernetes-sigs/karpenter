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

package termination_test

import (
	"sync"
	"time"

	"github.com/samber/lo"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/apis/v1beta1"
	"github.com/aws/karpenter-core/pkg/controllers/node/termination"
	"github.com/aws/karpenter-core/pkg/metrics"
	"github.com/aws/karpenter-core/pkg/test"
	nodeclaimutil "github.com/aws/karpenter-core/pkg/utils/nodeclaim"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"knative.dev/pkg/ptr"

	. "github.com/aws/karpenter-core/pkg/test/expectations"
)

var _ = Describe("Machine/Termination", func() {
	var node *v1.Node
	var machine *v1alpha5.Machine

	BeforeEach(func() {
		machine, node = test.MachineAndNode(v1alpha5.Machine{ObjectMeta: metav1.ObjectMeta{Finalizers: []string{v1beta1.TerminationFinalizer}}})
		node.Labels[v1alpha5.ProvisionerNameLabelKey] = test.Provisioner().Name
		cloudProvider.CreatedNodeClaims[node.Spec.ProviderID] = nodeclaimutil.New(machine)
		queue.Reset()
	})

	AfterEach(func() {
		ExpectCleanedUp(ctx, env.Client)
		fakeClock.SetTime(time.Now())
		cloudProvider.Reset()

		// Reset the metrics collectors
		metrics.NodesTerminatedCounter.Reset()
		termination.TerminationSummary.Reset()
	})

	Context("Reconciliation", func() {
		It("should delete nodes", func() {
			ExpectApplied(ctx, env.Client, node)
			Expect(env.Client.Delete(ctx, node)).To(Succeed())
			node = ExpectNodeExists(ctx, env.Client, node.Name)
			ExpectReconcileSucceeded(ctx, terminationController, client.ObjectKeyFromObject(node))
			ExpectNotFound(ctx, env.Client, node)
		})
		It("should delete machines associated with nodes", func() {
			ExpectApplied(ctx, env.Client, node, machine)
			Expect(env.Client.Delete(ctx, node)).To(Succeed())
			node = ExpectNodeExists(ctx, env.Client, node.Name)
			ExpectReconcileSucceeded(ctx, terminationController, client.ObjectKeyFromObject(node))
			ExpectExists(ctx, env.Client, machine)
			ExpectFinalizersRemoved(ctx, env.Client, machine)
			ExpectNotFound(ctx, env.Client, node, machine)
		})
		It("should not race if deleting nodes in parallel", func() {
			var nodes []*v1.Node
			for i := 0; i < 10; i++ {
				node = test.Node(test.NodeOptions{
					ObjectMeta: metav1.ObjectMeta{
						Finalizers: []string{v1alpha5.TerminationFinalizer},
					},
				})
				ExpectApplied(ctx, env.Client, node)
				Expect(env.Client.Delete(ctx, node)).To(Succeed())
				node = ExpectNodeExists(ctx, env.Client, node.Name)
				nodes = append(nodes, node)
			}

			var wg sync.WaitGroup
			// this is enough to trip the race detector
			for i := 0; i < 10; i++ {
				wg.Add(1)
				go func(node *v1.Node) {
					defer GinkgoRecover()
					defer wg.Done()
					ExpectReconcileSucceeded(ctx, terminationController, client.ObjectKeyFromObject(node))
				}(nodes[i])
			}
			wg.Wait()
			ExpectNotFound(ctx, env.Client, lo.Map(nodes, func(n *v1.Node, _ int) client.Object { return n })...)
		})
		It("should exclude nodes from load balancers when terminating", func() {
			// This is a kludge to prevent the node from being deleted before we can
			// inspect its labels
			podNoEvict := test.Pod(test.PodOptions{
				NodeName: node.Name,
				ObjectMeta: metav1.ObjectMeta{
					Annotations:     map[string]string{v1alpha5.DoNotEvictPodAnnotationKey: "true"},
					OwnerReferences: defaultOwnerRefs,
				},
			})

			ExpectApplied(ctx, env.Client, node, podNoEvict)

			Expect(env.Client.Delete(ctx, node)).To(Succeed())
			node = ExpectNodeExists(ctx, env.Client, node.Name)
			ExpectReconcileSucceeded(ctx, terminationController, client.ObjectKeyFromObject(node))
			node = ExpectNodeExists(ctx, env.Client, node.Name)
			Expect(node.Labels[v1.LabelNodeExcludeBalancers]).Should(Equal("karpenter"))
		})
		It("should not evict pods that tolerate unschedulable taint", func() {
			podEvict := test.Pod(test.PodOptions{NodeName: node.Name, ObjectMeta: metav1.ObjectMeta{OwnerReferences: defaultOwnerRefs}})
			podSkip := test.Pod(test.PodOptions{
				NodeName:    node.Name,
				Tolerations: []v1.Toleration{{Key: v1.TaintNodeUnschedulable, Operator: v1.TolerationOpExists, Effect: v1.TaintEffectNoSchedule}},
				ObjectMeta:  metav1.ObjectMeta{OwnerReferences: defaultOwnerRefs},
			})
			ExpectApplied(ctx, env.Client, node, podEvict, podSkip)

			// Trigger Termination Controller
			Expect(env.Client.Delete(ctx, node)).To(Succeed())
			node = ExpectNodeExists(ctx, env.Client, node.Name)
			ExpectReconcileSucceeded(ctx, terminationController, client.ObjectKeyFromObject(node))
			ExpectReconcileSucceeded(ctx, queue, client.ObjectKey{})

			// Expect node to exist and be draining
			ExpectNodeWithMachineDraining(env.Client, node.Name)

			// Expect podEvict to be evicting, and delete it
			ExpectEvicted(env.Client, podEvict)
			ExpectDeleted(ctx, env.Client, podEvict)

			// Reconcile to delete node
			node = ExpectNodeExists(ctx, env.Client, node.Name)
			ExpectReconcileSucceeded(ctx, terminationController, client.ObjectKeyFromObject(node))
			ExpectNotFound(ctx, env.Client, node)
		})
		It("should delete nodes that have pods without an ownerRef", func() {
			pod := test.Pod(test.PodOptions{
				NodeName: node.Name,
				ObjectMeta: metav1.ObjectMeta{
					OwnerReferences: nil,
				},
			})

			ExpectApplied(ctx, env.Client, node, pod)
			Expect(env.Client.Delete(ctx, node)).To(Succeed())
			node = ExpectNodeExists(ctx, env.Client, node.Name)
			ExpectReconcileSucceeded(ctx, terminationController, client.ObjectKeyFromObject(node))
			ExpectReconcileSucceeded(ctx, queue, client.ObjectKey{})

			// Expect pod with no owner ref to be enqueued for eviction
			ExpectEvicted(env.Client, pod)

			// Expect node to exist and be draining
			ExpectNodeWithMachineDraining(env.Client, node.Name)

			// Delete no owner refs pod to simulate successful eviction
			ExpectDeleted(ctx, env.Client, pod)

			// Reconcile node to evict pod
			node = ExpectNodeExists(ctx, env.Client, node.Name)
			ExpectReconcileSucceeded(ctx, terminationController, client.ObjectKeyFromObject(node))

			// Reconcile to delete node
			ExpectNotFound(ctx, env.Client, node)
		})
		It("should delete nodes with terminal pods", func() {
			podEvictPhaseSucceeded := test.Pod(test.PodOptions{
				NodeName: node.Name,
				Phase:    v1.PodSucceeded,
			})
			podEvictPhaseFailed := test.Pod(test.PodOptions{
				NodeName: node.Name,
				Phase:    v1.PodFailed,
			})

			ExpectApplied(ctx, env.Client, node, podEvictPhaseSucceeded, podEvictPhaseFailed)
			Expect(env.Client.Delete(ctx, node)).To(Succeed())
			node = ExpectNodeExists(ctx, env.Client, node.Name)
			// Trigger Termination Controller, which should ignore these pods and delete the node
			ExpectReconcileSucceeded(ctx, terminationController, client.ObjectKeyFromObject(node))
			ExpectNotFound(ctx, env.Client, node)
		})
		It("should fail to evict pods that violate a PDB", func() {
			minAvailable := intstr.FromInt(1)
			labelSelector := map[string]string{test.RandomName(): test.RandomName()}
			pdb := test.PodDisruptionBudget(test.PDBOptions{
				Labels: labelSelector,
				// Don't let any pod evict
				MinAvailable: &minAvailable,
			})
			podNoEvict := test.Pod(test.PodOptions{
				NodeName: node.Name,
				ObjectMeta: metav1.ObjectMeta{
					Labels:          labelSelector,
					OwnerReferences: defaultOwnerRefs,
				},
				Phase: v1.PodRunning,
			})

			ExpectApplied(ctx, env.Client, node, podNoEvict, pdb)

			// Trigger Termination Controller
			Expect(env.Client.Delete(ctx, node)).To(Succeed())
			node = ExpectNodeExists(ctx, env.Client, node.Name)
			ExpectReconcileSucceeded(ctx, terminationController, client.ObjectKeyFromObject(node))
			ExpectReconcileSucceeded(ctx, queue, client.ObjectKey{})

			// Expect node to exist and be draining
			ExpectNodeWithMachineDraining(env.Client, node.Name)

			// Expect podNoEvict to fail eviction due to PDB, and be retried
			Eventually(func() int {
				return queue.NumRequeues(client.ObjectKeyFromObject(podNoEvict))
			}).Should(BeNumerically(">=", 1))

			// Delete pod to simulate successful eviction
			ExpectDeleted(ctx, env.Client, podNoEvict)
			ExpectNotFound(ctx, env.Client, podNoEvict)

			// Reconcile to delete node
			node = ExpectNodeExists(ctx, env.Client, node.Name)
			ExpectReconcileSucceeded(ctx, terminationController, client.ObjectKeyFromObject(node))
			ExpectNotFound(ctx, env.Client, node)
		})
		It("should evict pods in order", func() {
			daemonEvict := test.DaemonSet()
			daemonNodeCritical := test.DaemonSet(test.DaemonSetOptions{PodOptions: test.PodOptions{PriorityClassName: "system-node-critical"}})
			daemonClusterCritical := test.DaemonSet(test.DaemonSetOptions{PodOptions: test.PodOptions{PriorityClassName: "system-cluster-critical"}})
			ExpectApplied(ctx, env.Client, node, daemonEvict, daemonNodeCritical, daemonClusterCritical)

			podEvict := test.Pod(test.PodOptions{NodeName: node.Name, ObjectMeta: metav1.ObjectMeta{OwnerReferences: defaultOwnerRefs}})
			podDaemonEvict := test.Pod(test.PodOptions{NodeName: node.Name, ObjectMeta: metav1.ObjectMeta{OwnerReferences: []metav1.OwnerReference{{
				APIVersion:         "apps/v1",
				Kind:               "DaemonSet",
				Name:               daemonEvict.Name,
				UID:                daemonEvict.UID,
				Controller:         ptr.Bool(true),
				BlockOwnerDeletion: ptr.Bool(true),
			}}}})
			podNodeCritical := test.Pod(test.PodOptions{NodeName: node.Name, PriorityClassName: "system-node-critical", ObjectMeta: metav1.ObjectMeta{OwnerReferences: defaultOwnerRefs}})
			podClusterCritical := test.Pod(test.PodOptions{NodeName: node.Name, PriorityClassName: "system-cluster-critical", ObjectMeta: metav1.ObjectMeta{OwnerReferences: defaultOwnerRefs}})
			podDaemonNodeCritical := test.Pod(test.PodOptions{NodeName: node.Name, PriorityClassName: "system-node-critical", ObjectMeta: metav1.ObjectMeta{OwnerReferences: []metav1.OwnerReference{{
				APIVersion:         "apps/v1",
				Kind:               "DaemonSet",
				Name:               daemonNodeCritical.Name,
				UID:                daemonNodeCritical.UID,
				Controller:         ptr.Bool(true),
				BlockOwnerDeletion: ptr.Bool(true),
			}}}})
			podDaemonClusterCritical := test.Pod(test.PodOptions{NodeName: node.Name, PriorityClassName: "system-cluster-critical", ObjectMeta: metav1.ObjectMeta{OwnerReferences: []metav1.OwnerReference{{
				APIVersion:         "apps/v1",
				Kind:               "DaemonSet",
				Name:               daemonClusterCritical.Name,
				UID:                daemonClusterCritical.UID,
				Controller:         ptr.Bool(true),
				BlockOwnerDeletion: ptr.Bool(true),
			}}}})

			ExpectApplied(ctx, env.Client, node, podEvict, podNodeCritical, podClusterCritical, podDaemonEvict, podDaemonNodeCritical, podDaemonClusterCritical)

			// Trigger Termination Controller
			Expect(env.Client.Delete(ctx, node)).To(Succeed())
			node = ExpectNodeExists(ctx, env.Client, node.Name)
			ExpectPodExists(ctx, env.Client, podEvict.Name, podEvict.Namespace)
			ExpectReconcileSucceeded(ctx, terminationController, client.ObjectKeyFromObject(node))
			ExpectReconcileSucceeded(ctx, queue, client.ObjectKey{})
			// Expect node to exist and be draining
			ExpectNodeWithMachineDraining(env.Client, node.Name)

			// Expect podEvict to be evicting, and delete it
			ExpectEvicted(env.Client, podEvict)
			ExpectDeleted(ctx, env.Client, podEvict)

			// Expect the noncritical Daemon pod to be evicted
			node = ExpectNodeExists(ctx, env.Client, node.Name)
			ExpectPodExists(ctx, env.Client, podDaemonEvict.Name, podDaemonEvict.Namespace)
			ExpectReconcileSucceeded(ctx, terminationController, client.ObjectKeyFromObject(node))
			ExpectReconcileSucceeded(ctx, queue, client.ObjectKey{})
			ExpectEvicted(env.Client, podDaemonEvict)
			ExpectDeleted(ctx, env.Client, podDaemonEvict)

			// Expect the critical pods to be evicted and deleted
			node = ExpectNodeExists(ctx, env.Client, node.Name)
			ExpectPodExists(ctx, env.Client, podNodeCritical.Name, podNodeCritical.Namespace)
			ExpectPodExists(ctx, env.Client, podClusterCritical.Name, podClusterCritical.Namespace)
			ExpectReconcileSucceeded(ctx, terminationController, client.ObjectKeyFromObject(node))
			ExpectReconcileSucceeded(ctx, queue, client.ObjectKey{})
			ExpectReconcileSucceeded(ctx, queue, client.ObjectKey{})
			ExpectEvicted(env.Client, podNodeCritical)
			ExpectEvicted(env.Client, podClusterCritical)
			ExpectDeleted(ctx, env.Client, podNodeCritical)
			ExpectDeleted(ctx, env.Client, podClusterCritical)

			// Expect the critical daemon pods to be evicted and deleted
			node = ExpectNodeExists(ctx, env.Client, node.Name)
			ExpectPodExists(ctx, env.Client, podDaemonNodeCritical.Name, podDaemonNodeCritical.Namespace)
			ExpectPodExists(ctx, env.Client, podDaemonClusterCritical.Name, podDaemonClusterCritical.Namespace)
			ExpectReconcileSucceeded(ctx, terminationController, client.ObjectKeyFromObject(node))
			ExpectReconcileSucceeded(ctx, queue, client.ObjectKey{})
			ExpectReconcileSucceeded(ctx, queue, client.ObjectKey{})
			ExpectEvicted(env.Client, podDaemonNodeCritical)
			ExpectEvicted(env.Client, podDaemonClusterCritical)
			ExpectDeleted(ctx, env.Client, podDaemonNodeCritical)
			ExpectDeleted(ctx, env.Client, podDaemonClusterCritical)

			// Reconcile to delete node
			node = ExpectNodeExists(ctx, env.Client, node.Name)
			ExpectReconcileSucceeded(ctx, terminationController, client.ObjectKeyFromObject(node))
			ExpectNotFound(ctx, env.Client, node)
		})
		It("should evict non-critical pods first", func() {
			podEvict := test.Pod(test.PodOptions{NodeName: node.Name, ObjectMeta: metav1.ObjectMeta{OwnerReferences: defaultOwnerRefs}})
			podNodeCritical := test.Pod(test.PodOptions{NodeName: node.Name, PriorityClassName: "system-node-critical", ObjectMeta: metav1.ObjectMeta{OwnerReferences: defaultOwnerRefs}})
			podClusterCritical := test.Pod(test.PodOptions{NodeName: node.Name, PriorityClassName: "system-cluster-critical", ObjectMeta: metav1.ObjectMeta{OwnerReferences: defaultOwnerRefs}})

			ExpectApplied(ctx, env.Client, node, podEvict, podNodeCritical, podClusterCritical)

			// Trigger Termination Controller
			Expect(env.Client.Delete(ctx, node)).To(Succeed())
			node = ExpectNodeExists(ctx, env.Client, node.Name)
			ExpectReconcileSucceeded(ctx, terminationController, client.ObjectKeyFromObject(node))
			ExpectReconcileSucceeded(ctx, queue, client.ObjectKey{})

			// Expect node to exist and be draining
			ExpectNodeWithMachineDraining(env.Client, node.Name)

			// Expect podEvict to be evicting, and delete it
			ExpectEvicted(env.Client, podEvict)
			ExpectDeleted(ctx, env.Client, podEvict)

			// Expect the critical pods to be evicted and deleted
			node = ExpectNodeExists(ctx, env.Client, node.Name)
			ExpectReconcileSucceeded(ctx, terminationController, client.ObjectKeyFromObject(node))
			ExpectReconcileSucceeded(ctx, queue, client.ObjectKey{})
			ExpectReconcileSucceeded(ctx, queue, client.ObjectKey{})
			ExpectEvicted(env.Client, podNodeCritical)
			ExpectEvicted(env.Client, podClusterCritical)
			ExpectDeleted(ctx, env.Client, podNodeCritical)
			ExpectDeleted(ctx, env.Client, podClusterCritical)

			// Reconcile to delete node
			node = ExpectNodeExists(ctx, env.Client, node.Name)
			ExpectReconcileSucceeded(ctx, terminationController, client.ObjectKeyFromObject(node))
			ExpectNotFound(ctx, env.Client, node)
		})
		It("should not evict static pods", func() {
			ExpectApplied(ctx, env.Client, node)
			podEvict := test.Pod(test.PodOptions{NodeName: node.Name, ObjectMeta: metav1.ObjectMeta{OwnerReferences: defaultOwnerRefs}})
			ExpectApplied(ctx, env.Client, node, podEvict)

			podNoEvict := test.Pod(test.PodOptions{
				NodeName: node.Name,
				ObjectMeta: metav1.ObjectMeta{
					OwnerReferences: []metav1.OwnerReference{{
						APIVersion: "v1",
						Kind:       "Node",
						Name:       node.Name,
						UID:        node.UID,
					}},
				},
			})
			ExpectApplied(ctx, env.Client, podNoEvict)

			Expect(env.Client.Delete(ctx, node)).To(Succeed())
			node = ExpectNodeExists(ctx, env.Client, node.Name)
			ExpectReconcileSucceeded(ctx, terminationController, client.ObjectKeyFromObject(node))
			ExpectReconcileSucceeded(ctx, queue, client.ObjectKey{})

			// Expect mirror pod to not be queued for eviction
			ExpectNotEnqueuedForEviction(queue, podNoEvict)

			// Expect podEvict to be enqueued for eviction then be successful
			ExpectEvicted(env.Client, podEvict)

			// Expect node to exist and be draining
			ExpectNodeWithMachineDraining(env.Client, node.Name)

			// Reconcile node to evict pod
			node = ExpectNodeExists(ctx, env.Client, node.Name)
			ExpectReconcileSucceeded(ctx, terminationController, client.ObjectKeyFromObject(node))

			// Delete pod to simulate successful eviction
			ExpectDeleted(ctx, env.Client, podEvict)

			// Reconcile to delete node
			node = ExpectNodeExists(ctx, env.Client, node.Name)
			ExpectReconcileSucceeded(ctx, terminationController, client.ObjectKeyFromObject(node))
			ExpectNotFound(ctx, env.Client, node)

		})
		It("should not delete nodes until all pods are deleted", func() {
			pods := test.Pods(2, test.PodOptions{NodeName: node.Name, ObjectMeta: metav1.ObjectMeta{OwnerReferences: defaultOwnerRefs}})
			ExpectApplied(ctx, env.Client, node, pods[0], pods[1])

			// Trigger Termination Controller
			Expect(env.Client.Delete(ctx, node)).To(Succeed())
			node = ExpectNodeExists(ctx, env.Client, node.Name)
			ExpectReconcileSucceeded(ctx, terminationController, client.ObjectKeyFromObject(node))
			ExpectReconcileSucceeded(ctx, queue, client.ObjectKey{})
			ExpectReconcileSucceeded(ctx, queue, client.ObjectKey{})

			// Expect the pods to be evicted
			ExpectEvicted(env.Client, pods[0], pods[1])

			// Expect node to exist and be draining, but not deleted
			node = ExpectNodeExists(ctx, env.Client, node.Name)
			ExpectReconcileSucceeded(ctx, terminationController, client.ObjectKeyFromObject(node))
			ExpectNodeWithMachineDraining(env.Client, node.Name)

			ExpectDeleted(ctx, env.Client, pods[1])

			// Expect node to exist and be draining, but not deleted
			node = ExpectNodeExists(ctx, env.Client, node.Name)
			ExpectReconcileSucceeded(ctx, terminationController, client.ObjectKeyFromObject(node))
			ExpectNodeWithMachineDraining(env.Client, node.Name)

			ExpectDeleted(ctx, env.Client, pods[0])

			// Reconcile to delete node
			node = ExpectNodeExists(ctx, env.Client, node.Name)
			ExpectReconcileSucceeded(ctx, terminationController, client.ObjectKeyFromObject(node))
			ExpectNotFound(ctx, env.Client, node)
		})
		It("should delete nodes with no underlying instance even if not fully drained", func() {
			pods := test.Pods(2, test.PodOptions{NodeName: node.Name, ObjectMeta: metav1.ObjectMeta{OwnerReferences: defaultOwnerRefs}})
			ExpectApplied(ctx, env.Client, node, pods[0], pods[1])

			// Trigger Termination Controller
			Expect(env.Client.Delete(ctx, node)).To(Succeed())
			node = ExpectNodeExists(ctx, env.Client, node.Name)
			ExpectReconcileSucceeded(ctx, terminationController, client.ObjectKeyFromObject(node))
			ExpectReconcileSucceeded(ctx, queue, client.ObjectKey{})
			ExpectReconcileSucceeded(ctx, queue, client.ObjectKey{})

			// Expect the pods to be evicted
			ExpectEvicted(env.Client, pods[0], pods[1])

			// Expect node to exist and be draining, but not deleted
			node = ExpectNodeExists(ctx, env.Client, node.Name)
			ExpectReconcileSucceeded(ctx, terminationController, client.ObjectKeyFromObject(node))
			ExpectNodeWithMachineDraining(env.Client, node.Name)

			// After this, the node still has one pod that is evicting.
			ExpectDeleted(ctx, env.Client, pods[1])

			// Remove the node from created machines so that the cloud provider returns DNE
			cloudProvider.CreatedNodeClaims = map[string]*v1beta1.NodeClaim{}

			// Reconcile to delete node
			node = ExpectNodeExists(ctx, env.Client, node.Name)
			ExpectReconcileSucceeded(ctx, terminationController, client.ObjectKeyFromObject(node))
			ExpectNotFound(ctx, env.Client, node)
		})
		It("should wait for pods to terminate", func() {
			pod := test.Pod(test.PodOptions{NodeName: node.Name, ObjectMeta: metav1.ObjectMeta{OwnerReferences: defaultOwnerRefs}})
			fakeClock.SetTime(time.Now()) // make our fake clock match the pod creation time
			ExpectApplied(ctx, env.Client, node, pod)

			// Before grace period, node should not delete
			Expect(env.Client.Delete(ctx, node)).To(Succeed())
			ExpectReconcileSucceeded(ctx, terminationController, client.ObjectKeyFromObject(node))
			ExpectReconcileSucceeded(ctx, queue, client.ObjectKey{})
			ExpectNodeExists(ctx, env.Client, node.Name)
			ExpectEvicted(env.Client, pod)

			// After grace period, node should delete. The deletion timestamps are from etcd which we can't control, so
			// to eliminate test-flakiness we reset the time to current time + 90 seconds instead of just advancing
			// the clock by 90 seconds.
			fakeClock.SetTime(time.Now().Add(90 * time.Second))
			ExpectReconcileSucceeded(ctx, terminationController, client.ObjectKeyFromObject(node))
			ExpectNotFound(ctx, env.Client, node)
		})
	})
	Context("Metrics", func() {
		It("should fire the terminationSummary metric when deleting nodes", func() {
			ExpectApplied(ctx, env.Client, node)
			Expect(env.Client.Delete(ctx, node)).To(Succeed())
			node = ExpectNodeExists(ctx, env.Client, node.Name)
			ExpectReconcileSucceeded(ctx, terminationController, client.ObjectKeyFromObject(node))

			m, ok := FindMetricWithLabelValues("karpenter_nodes_termination_time_seconds", map[string]string{"provisioner": ""})
			Expect(ok).To(BeTrue())
			Expect(m.GetSummary().GetSampleCount()).To(BeNumerically("==", 1))
		})
		It("should fire the nodesTerminated counter metric when deleting nodes", func() {
			ExpectApplied(ctx, env.Client, node)
			Expect(env.Client.Delete(ctx, node)).To(Succeed())
			node = ExpectNodeExists(ctx, env.Client, node.Name)
			ExpectReconcileSucceeded(ctx, terminationController, client.ObjectKeyFromObject(node))

			m, ok := FindMetricWithLabelValues("karpenter_nodes_terminated", map[string]string{"provisioner": ""})
			Expect(ok).To(BeTrue())
			Expect(lo.FromPtr(m.GetCounter().Value)).To(BeNumerically("==", 1))
		})
	})
})
