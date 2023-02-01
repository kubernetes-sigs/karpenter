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

package state

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/samber/lo"
	"go.uber.org/multierr"
	v1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/clock"
	"knative.dev/pkg/logging"
	"knative.dev/pkg/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/cloudprovider"
	"github.com/aws/karpenter-core/pkg/scheduling"
	podutils "github.com/aws/karpenter-core/pkg/utils/pod"
	"github.com/aws/karpenter-core/pkg/utils/sets"
)

// Cluster maintains cluster state that is often needed but expensive to compute.
type Cluster struct {
	kubeClient    client.Client
	cloudProvider cloudprovider.CloudProvider
	clock         clock.Clock

	mu               sync.RWMutex
	nodes            map[string]*Node                // provider id -> node
	bindings         map[types.NamespacedName]string // pod namespaced named -> node node
	nameToProviderID map[string]string               // node name -> provider id

	antiAffinityPods sync.Map // pod namespaced name -> *v1.Pod of pods that have required anti affinities

	// consolidated is a dirty bit that indicates that the cluster hasn't
	// changed since last consolidation and avoids recomputation.
	consolidated   atomic.Bool
	consolidatedAt atomic.Int64
}

func NewCluster(clk clock.Clock, client client.Client, cp cloudprovider.CloudProvider) *Cluster {
	return &Cluster{
		clock:            clk,
		kubeClient:       client,
		cloudProvider:    cp,
		nodes:            map[string]*Node{},
		bindings:         map[types.NamespacedName]string{},
		nameToProviderID: map[string]string{},
	}
}

// Synced validates that the Machines and the Nodes that are stored in the apiserver
// have the same representation in the cluster state. This is to ensure that our view
// of the cluster is as close to correct as it can be when we begin to perform operations
// utilizing the cluster state as our source of truth
func (c *Cluster) Synced(ctx context.Context) bool {
	machineList := &v1alpha5.MachineList{}
	if err := c.kubeClient.List(ctx, machineList); err != nil {
		logging.FromContext(ctx).Errorf("checking cluster state sync, %v", err)
		return false
	}
	nodeList := &v1.NodeList{}
	if err := c.kubeClient.List(ctx, nodeList); err != nil {
		logging.FromContext(ctx).Errorf("checking cluster state sync, %v", err)
		return false
	}
	c.mu.RLock()
	stateProviderIDs := sets.New(lo.Keys(c.nodes)...)
	c.mu.RUnlock()

	providerIDs := sets.New[string]()
	for _, machine := range machineList.Items {
		// If the machine hasn't resolved its provider id, then it hasn't resolved its status
		if machine.Status.ProviderID == "" {
			return false
		}
		providerIDs.Insert(machine.Status.ProviderID)
	}
	for _, node := range nodeList.Items {
		if node.Spec.ProviderID == "" {
			node.Spec.ProviderID = node.Name
		}
		providerIDs.Insert(node.Spec.ProviderID)
	}
	// The provider ids tracked in-memory should at least have all the data that is in the api-server
	// This doesn't ensure that the two states are exactly aligned (we could still not be tracking a node
	// that exists on the apiserver but not in the cluster state) but it ensures that we have a state
	// representation for every node/machine that exists on the apiserver
	return stateProviderIDs.IsSuperset(providerIDs)
}

// ForPodsWithAntiAffinity calls the supplied function once for each pod with required anti affinity terms that is
// currently bound to a node. The pod returned may not be up-to-date with respect to status, however since the
// anti-affinity terms can't be modified, they will be correct.
func (c *Cluster) ForPodsWithAntiAffinity(fn func(p *v1.Pod, n *v1.Node) bool) {
	c.antiAffinityPods.Range(func(key, value interface{}) bool {
		pod := value.(*v1.Pod)
		c.mu.RLock()
		defer c.mu.RUnlock()
		nodeName, ok := c.bindings[client.ObjectKeyFromObject(pod)]
		if !ok {
			return true
		}
		node, ok := c.nodes[c.nameToProviderID[nodeName]]
		if !ok || node.Node == nil {
			// if we receive the node deletion event before the pod deletion event, this can happen
			return true
		}
		return fn(pod, node.Node)
	})
}

// ForEachNode calls the supplied function once per node object that is being tracked. It is not safe to store the
// state.Node object, it should be only accessed from within the function provided to this method.
func (c *Cluster) ForEachNode(f func(n *Node) bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	for _, node := range c.nodes {
		if !f(node) {
			return
		}
	}
}

// Nodes creates a DeepCopy of all state nodes.
// NOTE: This is very inefficient so this should only be used when DeepCopying is absolutely necessary
func (c *Cluster) Nodes() Nodes {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return lo.Map(lo.Values(c.nodes), func(n *Node, _ int) *Node {
		return n.DeepCopy()
	})
}

// IsNodeNominated returns true if the given node was expected to have a pod bound to it during a recent scheduling
// batch
func (c *Cluster) IsNodeNominated(name string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if id, ok := c.nameToProviderID[name]; ok {
		return c.nodes[id].Nominated()
	}
	return false
}

// NominateNodeForPod records that a node was the target of a pending pod during a scheduling batch
func (c *Cluster) NominateNodeForPod(ctx context.Context, name string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if id, ok := c.nameToProviderID[name]; ok {
		c.nodes[id].Nominate(ctx) // extends nomination window if already nominated
	}
}

// UnmarkForDeletion removes the marking on the node as a node the controller intends to delete
func (c *Cluster) UnmarkForDeletion(names ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for _, name := range names {
		if id, ok := c.nameToProviderID[name]; ok {
			c.nodes[id].markedForDeletion = false
		}
	}
}

// MarkForDeletion marks the node as pending deletion in the internal cluster state
func (c *Cluster) MarkForDeletion(names ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for _, name := range names {
		if id, ok := c.nameToProviderID[name]; ok {
			c.nodes[id].markedForDeletion = true
		}
	}
}

func (c *Cluster) UpdateMachine(machine *v1alpha5.Machine) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if machine.Status.ProviderID == "" {
		return // We can't reconcile machines that don't yet have provider ids
	}
	n := c.newStateFromMachine(machine, c.nodes[machine.Status.ProviderID])
	c.nodes[machine.Status.ProviderID] = n
	c.nameToProviderID[machine.Name] = machine.Status.ProviderID
}

func (c *Cluster) DeleteMachine(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.cleanupMachine(name)
}

func (c *Cluster) UpdateNode(ctx context.Context, node *v1.Node) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if node.Spec.ProviderID == "" {
		node.Spec.ProviderID = node.Name
	}
	n, err := c.newStateFromNode(ctx, node, c.nodes[node.Spec.ProviderID])
	if err != nil {
		return err
	}
	c.nodes[node.Spec.ProviderID] = n
	c.nameToProviderID[node.Name] = node.Spec.ProviderID
	return nil
}

func (c *Cluster) DeleteNode(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.cleanupNode(name)
}

func (c *Cluster) UpdatePod(ctx context.Context, pod *v1.Pod) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	var err error
	if podutils.IsTerminal(pod) {
		c.updateNodeUsageFromPodCompletion(client.ObjectKeyFromObject(pod))
	} else {
		err = c.updateNodeUsageFromPod(ctx, pod)
	}
	c.updatePodAntiAffinities(pod)
	return err
}

func (c *Cluster) DeletePod(podKey types.NamespacedName) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.antiAffinityPods.Delete(podKey)
	c.updateNodeUsageFromPodCompletion(podKey)
	c.SetConsolidated(false)
}

func (c *Cluster) SetConsolidated(consolidated bool) {
	c.consolidated.Store(consolidated)
	c.consolidatedAt.Store(c.clock.Now().UnixMilli())
}

// ClusterConsolidationState returns a number representing the state of the cluster with respect to consolidation.  If
// consolidation can't occur and this number hasn't changed, there is no point in re-attempting consolidation. This
// allows reducing overall CPU utilization by pausing consolidation when the cluster is in a static state.
func (c *Cluster) Consolidated() bool {
	consolidatedAt := time.UnixMilli(c.consolidatedAt.Load())
	// If 5 minutes elapsed since the last time the consolidation state was changed, we change the state anyway. This
	// ensures that at least once every 5 minutes we consider consolidating our cluster in case something else has
	// changed (e.g. instance type availability) that we can't detect which would allow consolidation to occur.
	if c.clock.Now().After(consolidatedAt.Add(5 * time.Minute)) {
		c.SetConsolidated(false)
	}
	return c.consolidated.Load()
}

// Reset the cluster state for unit testing
func (c *Cluster) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nodes = map[string]*Node{}
	c.nameToProviderID = map[string]string{}
	c.bindings = map[types.NamespacedName]string{}
	c.antiAffinityPods = sync.Map{}
}

// WARNING
// Everything under this section of code assumes that you have already held a lock when you are calling into these functions
// and explicitly modifying the cluster state. If you do not hold the cluster state lock before calling any of these helpers
// you will hit race conditions and data corruption

func (c *Cluster) newStateFromMachine(machine *v1alpha5.Machine, oldNode *Node) *Node {
	if oldNode == nil {
		oldNode = NewNode()
	}
	n := &Node{
		Node:              oldNode.Node,
		Machine:           machine,
		hostPortUsage:     oldNode.hostPortUsage,
		volumeUsage:       oldNode.volumeUsage,
		daemonSetRequests: oldNode.daemonSetRequests,
		daemonSetLimits:   oldNode.daemonSetLimits,
		podRequests:       oldNode.podRequests,
		podLimits:         oldNode.podLimits,
		markedForDeletion: oldNode.markedForDeletion,
		nominatedUntil:    oldNode.nominatedUntil,
	}
	// Cleanup the old machine with its old providerID if its providerID changes
	// This can happen since nodes don't get created with providerIDs. Rather, CCM picks up the
	// created node and injects the providerID into the spec.providerID
	if id, ok := c.nameToProviderID[machine.Name]; ok && id != machine.Status.ProviderID {
		c.cleanupMachine(machine.Name)
	}
	c.triggerConsolidationOnChange(oldNode, n)
	return n
}

func (c *Cluster) cleanupMachine(name string) {
	if id := c.nameToProviderID[name]; id != "" {
		if c.nodes[id].Node == nil {
			delete(c.nodes, id)
		} else {
			c.nodes[id].Machine = nil
		}
		delete(c.nameToProviderID, name)
		c.SetConsolidated(false)
	}
}

func (c *Cluster) newStateFromNode(ctx context.Context, node *v1.Node, oldNode *Node) (*Node, error) {
	if oldNode == nil {
		oldNode = NewNode()
	}
	n := &Node{
		Node:              node,
		Machine:           oldNode.Machine,
		hostPortUsage:     scheduling.NewHostPortUsage(),
		volumeUsage:       scheduling.NewVolumeLimits(c.kubeClient),
		volumeLimits:      scheduling.VolumeCount{},
		daemonSetRequests: map[types.NamespacedName]v1.ResourceList{},
		daemonSetLimits:   map[types.NamespacedName]v1.ResourceList{},
		podRequests:       map[types.NamespacedName]v1.ResourceList{},
		podLimits:         map[types.NamespacedName]v1.ResourceList{},
		markedForDeletion: oldNode.markedForDeletion,
		nominatedUntil:    oldNode.nominatedUntil,
	}
	if err := multierr.Combine(
		c.populateStartupTaints(ctx, n),
		c.populateInflight(ctx, n),
		c.populateResourceRequests(ctx, n),
		c.populateVolumeLimits(ctx, n),
	); err != nil {
		return nil, err
	}
	// Cleanup the old node with its old providerID if its providerID changes
	// This can happen since nodes don't get created with providerIDs. Rather, CCM picks up the
	// created node and injects the providerID into the spec.providerID
	if id, ok := c.nameToProviderID[node.Name]; ok && id != node.Spec.ProviderID {
		c.cleanupNode(node.Name)
	}
	c.triggerConsolidationOnChange(oldNode, n)
	return n, nil
}

func (c *Cluster) cleanupNode(name string) {
	if id := c.nameToProviderID[name]; id != "" {
		if c.nodes[id].Machine == nil {
			delete(c.nodes, id)
		} else {
			c.nodes[id].Node = nil
		}
		delete(c.nameToProviderID, name)
		c.SetConsolidated(false)
	}
}

func (c *Cluster) populateStartupTaints(ctx context.Context, n *Node) error {
	if !n.Owned() {
		return nil
	}
	provisioner := &v1alpha5.Provisioner{}
	if err := c.kubeClient.Get(ctx, client.ObjectKey{Name: n.Labels()[v1alpha5.ProvisionerNameLabelKey]}, provisioner); err != nil {
		return client.IgnoreNotFound(fmt.Errorf("getting provisioner, %w", err))
	}
	n.startupTaints = provisioner.Spec.StartupTaints
	return nil
}

func (c *Cluster) populateInflight(ctx context.Context, n *Node) error {
	if !n.Owned() {
		return nil
	}
	provisioner := &v1alpha5.Provisioner{}
	if err := c.kubeClient.Get(ctx, client.ObjectKey{Name: n.Labels()[v1alpha5.ProvisionerNameLabelKey]}, provisioner); err != nil {
		return client.IgnoreNotFound(fmt.Errorf("getting provisioner, %w", err))
	}
	instanceTypes, err := c.cloudProvider.GetInstanceTypes(ctx, provisioner)
	if err != nil {
		return err
	}
	instanceType, ok := lo.Find(instanceTypes, func(it *cloudprovider.InstanceType) bool {
		return it.Name == n.Labels()[v1.LabelInstanceTypeStable]
	})
	if !ok {
		return fmt.Errorf("instance type '%s' not found", n.Labels()[v1.LabelInstanceTypeStable])
	}
	n.inflightCapacity = instanceType.Capacity
	n.inflightAllocatable = instanceType.Allocatable()
	return nil
}

func (c *Cluster) populateVolumeLimits(ctx context.Context, n *Node) error {
	var csiNode storagev1.CSINode
	if err := c.kubeClient.Get(ctx, client.ObjectKey{Name: n.Node.Name}, &csiNode); err != nil {
		return client.IgnoreNotFound(fmt.Errorf("getting CSINode to determine volume limit for %s, %w", n.Node.Name, err))
	}
	for _, driver := range csiNode.Spec.Drivers {
		if driver.Allocatable == nil {
			continue
		}
		n.volumeLimits[driver.Name] = int(ptr.Int32Value(driver.Allocatable.Count))
	}
	return nil
}

func (c *Cluster) populateResourceRequests(ctx context.Context, n *Node) error {
	var pods v1.PodList
	if err := c.kubeClient.List(ctx, &pods, client.MatchingFields{"spec.nodeName": n.Node.Name}); err != nil {
		return fmt.Errorf("listing pods, %w", err)
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if podutils.IsTerminal(pod) {
			continue
		}
		c.cleanupOldBindings(pod)
		n.updateForPod(ctx, pod)
		c.bindings[client.ObjectKeyFromObject(pod)] = pod.Spec.NodeName
	}
	return nil
}

// updateNodeUsageFromPod is called every time a reconcile event occurs for the pod. If the pods binding has changed
// (unbound to bound), we need to update the resource requests on the node.
func (c *Cluster) updateNodeUsageFromPod(ctx context.Context, pod *v1.Pod) error {
	// nothing to do if the pod isn't bound, checking early allows avoiding unnecessary locking
	if pod.Spec.NodeName == "" {
		return nil
	}

	n, ok := c.nodes[c.nameToProviderID[pod.Spec.NodeName]]
	if !ok {
		// the node must exist for us to update the resource requests on the node
		return errors.NewNotFound(schema.GroupResource{Resource: "Node"}, pod.Spec.NodeName)
	}
	c.cleanupOldBindings(pod)
	n.updateForPod(ctx, pod)
	c.bindings[client.ObjectKeyFromObject(pod)] = pod.Spec.NodeName
	return nil
}

func (c *Cluster) updateNodeUsageFromPodCompletion(podKey types.NamespacedName) {
	nodeName, bindingKnown := c.bindings[podKey]
	if !bindingKnown {
		// we didn't think the pod was bound, so we weren't tracking it and don't need to do anything
		return
	}

	delete(c.bindings, podKey)
	n, ok := c.nodes[c.nameToProviderID[nodeName]]
	if !ok {
		// we weren't tracking the node yet, so nothing to do
		return
	}
	n.cleanupForPod(podKey)
}

func (c *Cluster) cleanupOldBindings(pod *v1.Pod) {
	if oldNodeName, bindingKnown := c.bindings[client.ObjectKeyFromObject(pod)]; bindingKnown {
		if oldNodeName == pod.Spec.NodeName {
			// we are already tracking the pod binding, so nothing to update
			return
		}
		// the pod has switched nodes, this can occur if a pod name was re-used, and it was deleted/re-created rapidly,
		// binding to a different node the second time
		if oldNode, ok := c.nodes[c.nameToProviderID[oldNodeName]]; ok {
			// we were tracking the old node, so we need to reduce its capacity by the amount of the pod that left
			oldNode.cleanupForPod(client.ObjectKeyFromObject(pod))
			delete(c.bindings, client.ObjectKeyFromObject(pod))
		}
	}
	// new pod binding has occurred
	c.SetConsolidated(false)
}

func (c *Cluster) updatePodAntiAffinities(pod *v1.Pod) {
	// We intentionally don't track inverse anti-affinity preferences. We're not
	// required to enforce them so it just adds complexity for very little
	// value. The problem with them comes from the relaxation process, the pod
	// we are relaxing is not the pod with the anti-affinity term.
	if podKey := client.ObjectKeyFromObject(pod); podutils.HasRequiredPodAntiAffinity(pod) {
		c.antiAffinityPods.Store(podKey, pod)
	} else {
		c.antiAffinityPods.Delete(podKey)
	}
}

func (c *Cluster) triggerConsolidationOnChange(old, new *Node) {
	if old == nil || new == nil {
		c.SetConsolidated(false)
		return
	}
	if old.Initialized() != new.Initialized() {
		c.SetConsolidated(false)
		return
	}
	if old.MarkedForDeletion() != new.MarkedForDeletion() {
		c.SetConsolidated(false)
		return
	}
}
