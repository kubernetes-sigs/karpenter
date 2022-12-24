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
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/patrickmn/go-cache"
	"github.com/samber/lo"
	"go.uber.org/multierr"
	v1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/clock"
	"knative.dev/pkg/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/aws/karpenter-core/pkg/apis/config/settings"
	"github.com/aws/karpenter-core/pkg/apis/v1alpha1"
	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/cloudprovider"
	"github.com/aws/karpenter-core/pkg/scheduling"
	podutils "github.com/aws/karpenter-core/pkg/utils/pod"
	"github.com/aws/karpenter-core/pkg/utils/resources"
)

// Cluster maintains cluster state that is often needed but expensive to compute.
type Cluster struct {
	kubeClient    client.Client
	cloudProvider cloudprovider.CloudProvider
	clock         clock.Clock

	// State: Node Status & Pod -> Node Binding
	mu sync.RWMutex

	nodes                map[string]*Node               // provider id -> node
	bindings             map[types.NamespacedName]*Node // pod namespaced name -> node name
	nodeNameToProviderID map[string]string              // node name -> providerID

	nominatedNodes   *cache.Cache // set of providerIDs nominated for pods
	antiAffinityPods sync.Map     // mapping of pod namespaced name -> *v1.Pod of pods that have required anti affinities

	// consolidationState is a number indicating the state of the cluster with respect to consolidation.  If this number
	// hasn't changed, it indicates that the cluster hasn't changed in a state which would enable consolidation if
	// it previously couldn't occur.
	consolidationState int64
}

func NewCluster(ctx context.Context, clk clock.Clock, client client.Client, cp cloudprovider.CloudProvider) *Cluster {
	// The nominationPeriod is how long we consider a node as 'likely to be used' after a pending pod was
	// nominated for it. This time can very depending on the batching window size + time spent scheduling
	// so we try to adjust based off the window size.
	nominationPeriod := 2 * settings.FromContext(ctx).BatchMaxDuration.Duration
	if nominationPeriod < 10*time.Second {
		nominationPeriod = 10 * time.Second
	}

	c := &Cluster{
		clock:                clk,
		kubeClient:           client,
		cloudProvider:        cp,
		nominatedNodes:       cache.New(nominationPeriod, 10*time.Second),
		nodes:                map[string]*Node{},
		bindings:             map[types.NamespacedName]*Node{},
		nodeNameToProviderID: map[string]string{},
	}
	return c
}

// Node is a cached version of a node in the cluster that maintains state which is expensive to compute every time it's
// needed.  This currently contains node utilization across all the allocatable resources, but will soon be used to
// compute topology information.
// +k8s:deepcopy-gen=true
type Node struct {
	Node *v1.Node
	// Capacity is the total resources on the node.
	Capacity v1.ResourceList
	// Allocatable is the total amount of resources on the node after os overhead.
	Allocatable v1.ResourceList
	// Available is allocatable minus anything allocated to pods.
	Available v1.ResourceList
	// Available is the total amount of resources that are available on the node.  This is the Allocatable minus the
	// resources requested by all pods bound to the node.
	// DaemonSetRequested is the total amount of resources that have been requested by daemon sets.  This allows users
	// of the Node to identify the remaining resources that we expect future daemonsets to consume.  This is already
	// included in the calculation for Available.
	DaemonSetRequested v1.ResourceList
	DaemonSetLimits    v1.ResourceList
	// HostPort usage of all pods that are bound to the node
	HostPortUsage *scheduling.HostPortUsage
	VolumeUsage   *scheduling.VolumeLimits
	VolumeLimits  scheduling.VolumeCount

	podRequests map[types.NamespacedName]v1.ResourceList
	podLimits   map[types.NamespacedName]v1.ResourceList

	// PodTotalRequests is the total resources on pods scheduled to this node
	PodTotalRequests v1.ResourceList
	// PodTotalLimits is the total resource limits scheduled to this node
	PodTotalLimits v1.ResourceList
	// MarkedForDeletion marks this node to say that there is some controller that is
	// planning to delete this node so consider pods that are present on it available for scheduling
	MarkedForDeletion bool
	Initialized       bool
}

// ForPodsWithAntiAffinity calls the supplied function once for each pod with required anti affinity terms that is
// currently bound to a node. The pod returned may not be up-to-date with respect to status, however since the
// anti-affinity terms can't be modified, they will be correct.
func (c *Cluster) ForPodsWithAntiAffinity(fn func(p *v1.Pod, n *v1.Node) bool) {
	c.antiAffinityPods.Range(func(key, value interface{}) bool {
		pod := value.(*v1.Pod)
		c.mu.RLock()
		defer c.mu.RUnlock()
		node, ok := c.bindings[client.ObjectKeyFromObject(pod)]
		if !ok {
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
	var nodes []*Node
	for _, node := range c.nodes {
		nodes = append(nodes, node)
	}
	// sort nodes by creation time, so we provide a consistent ordering
	sort.Slice(nodes, func(a, b int) bool {
		if nodes[a].Node.CreationTimestamp != nodes[b].Node.CreationTimestamp {
			return nodes[a].Node.CreationTimestamp.Time.Before(nodes[b].Node.CreationTimestamp.Time)
		}
		// sometimes we get nodes created in the same second, so sort again by node UID to provide a consistent ordering
		return nodes[a].Node.UID < nodes[b].Node.UID
	})

	for _, node := range nodes {
		if !f(node) {
			return
		}
	}
}

// IsNodeNominated returns true if the given node was expected to have a pod bound to it during a recent scheduling
// batch
func (c *Cluster) IsNodeNominated(name string) bool {
	if id, ok := c.nodeNameToProviderID[name]; ok {
		_, exists := c.nominatedNodes.Get(id)
		return exists
	}
	return false
}

// NominateNodeForPod records that a node was the target of a pending pod during a scheduling batch
func (c *Cluster) NominateNodeForPod(name string) {
	if id, ok := c.nodeNameToProviderID[name]; ok {
		c.nominatedNodes.SetDefault(id, nil)
	}
}

// UnmarkForDeletion removes the marking on the node as a node the controller intends to delete
func (c *Cluster) UnmarkForDeletion(names ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for _, name := range names {
		if id, ok := c.nodeNameToProviderID[name]; ok {
			if _, ok = c.nodes[id]; ok {
				c.nodes[id].MarkedForDeletion = false
			}
		}
	}
}

// MarkForDeletion marks the node as pending deletion in the internal cluster state
func (c *Cluster) MarkForDeletion(names ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for _, name := range names {
		if id, ok := c.nodeNameToProviderID[name]; ok {
			if _, ok = c.nodes[id]; ok {
				c.nodes[id].MarkedForDeletion = true
			}
		}
	}
}

// newNode always returns a node, even if some portion of the update has failed
func (c *Cluster) newNode(ctx context.Context, node *v1.Node) (*Node, error) {
	n := &Node{
		Node:              node,
		Capacity:          v1.ResourceList{},
		Allocatable:       v1.ResourceList{},
		Available:         v1.ResourceList{},
		HostPortUsage:     scheduling.NewHostPortUsage(),
		VolumeUsage:       scheduling.NewVolumeLimits(c.kubeClient),
		VolumeLimits:      scheduling.VolumeCount{},
		podRequests:       map[types.NamespacedName]v1.ResourceList{},
		podLimits:         map[types.NamespacedName]v1.ResourceList{},
		MarkedForDeletion: !node.DeletionTimestamp.IsZero(),
		Initialized:       node.Labels[v1alpha5.LabelNodeInitialized] == "true",
	}
	if err := multierr.Combine(
		c.populateCapacity(ctx, node, n),
		c.populateVolumeLimits(ctx, node, n),
		c.populateResourceRequests(ctx, node, n),
	); err != nil {
		return nil, err
	}
	return n, nil
}

func (c *Cluster) newInflightNode(machine *v1alpha1.Machine) *Node {
	return &Node{
		Node:              machine.ToNode(),
		Capacity:          machine.Status.Capacity,
		Allocatable:       machine.Status.Allocatable,
		MarkedForDeletion: !machine.DeletionTimestamp.IsZero(),
		Initialized:       false,
	}
}

// nolint:gocyclo
// TODO joinnis: Note that this entire function can be removed and reduced down to checking the
// Node status for the capacity and allocatable when we have migrated away from using node labels and we have fully
// migrated everyone over to using the Machine CR
func (c *Cluster) populateCapacity(ctx context.Context, node *v1.Node, n *Node) error {
	// Use node's values if initialized
	if node.Labels[v1alpha5.LabelNodeInitialized] == "true" {
		n.Allocatable = node.Status.Allocatable
		n.Capacity = node.Status.Capacity
		return nil
	}
	// Fallback to instance type capacity otherwise
	provisioner := &v1alpha5.Provisioner{}
	// In flight nodes not owned by karpenter are not included in calculations
	if _, ok := node.Labels[v1alpha5.ProvisionerNameLabelKey]; !ok {
		return nil
	}
	if err := c.kubeClient.Get(ctx, client.ObjectKey{Name: node.Labels[v1alpha5.ProvisionerNameLabelKey]}, provisioner); err != nil {
		// Nodes that are not owned by an existing provisioner are not included in calculations
		return client.IgnoreNotFound(fmt.Errorf("getting provisioner, %w", err))
	}
	instanceTypes, err := c.cloudProvider.GetInstanceTypes(ctx, provisioner)
	if err != nil {
		return err
	}
	instanceType, ok := lo.Find(instanceTypes, func(it *cloudprovider.InstanceType) bool { return it.Name == node.Labels[v1.LabelInstanceTypeStable] })
	if !ok {
		return fmt.Errorf("instance type '%s' not found", node.Labels[v1.LabelInstanceTypeStable])
	}

	n.Capacity = lo.Assign(node.Status.Capacity) // ensure map not nil
	// Use instance type resource value if resource isn't currently registered in .Status.Capacity
	for resourceName, quantity := range instanceType.Capacity {
		if resources.IsZero(node.Status.Capacity[resourceName]) {
			n.Capacity[resourceName] = quantity
		}
	}
	n.Allocatable = lo.Assign(node.Status.Allocatable) // ensure map not nil
	// Use instance type resource value if resource isn't currently registered in .Status.Allocatable
	for resourceName, quantity := range instanceType.Capacity {
		if resources.IsZero(node.Status.Allocatable[resourceName]) {
			n.Allocatable[resourceName] = quantity
		}
	}
	return nil
}

func (c *Cluster) populateResourceRequests(ctx context.Context, node *v1.Node, n *Node) error {
	var pods v1.PodList
	if err := c.kubeClient.List(ctx, &pods, client.MatchingFields{"spec.nodeName": node.Name}); err != nil {
		return fmt.Errorf("listing pods, %w", err)
	}
	var requested []v1.ResourceList
	var limits []v1.ResourceList
	var daemonsetRequested []v1.ResourceList
	var daemonsetLimits []v1.ResourceList
	for i := range pods.Items {
		pod := &pods.Items[i]
		if podutils.IsTerminal(pod) {
			continue
		}
		requests := resources.RequestsForPods(pod)
		podLimits := resources.LimitsForPods(pod)
		podKey := client.ObjectKeyFromObject(pod)
		n.podRequests[podKey] = requests
		n.podLimits[podKey] = podLimits
		c.bindings[podKey] = n
		if podutils.IsOwnedByDaemonSet(pod) {
			daemonsetRequested = append(daemonsetRequested, requests)
			daemonsetLimits = append(daemonsetLimits, podLimits)
		}
		requested = append(requested, requests)
		limits = append(limits, podLimits)
		n.HostPortUsage.Add(ctx, pod)
		n.VolumeUsage.Add(ctx, pod)
	}

	n.DaemonSetRequested = resources.Merge(daemonsetRequested...)
	n.DaemonSetLimits = resources.Merge(daemonsetLimits...)
	n.PodTotalRequests = resources.Merge(requested...)
	n.PodTotalLimits = resources.Merge(limits...)
	n.Available = resources.Subtract(n.Allocatable, resources.Merge(requested...))
	return nil
}

func (c *Cluster) populateVolumeLimits(ctx context.Context, node *v1.Node, n *Node) error {
	var csiNode storagev1.CSINode
	if err := c.kubeClient.Get(ctx, client.ObjectKey{Name: node.Name}, &csiNode); err != nil {
		return client.IgnoreNotFound(fmt.Errorf("getting CSINode to determine volume limit for %s, %w", node.Name, err))
	}

	for _, driver := range csiNode.Spec.Drivers {
		if driver.Allocatable == nil {
			continue
		}
		n.VolumeLimits[driver.Name] = int(ptr.Int32Value(driver.Allocatable.Count))
	}
	return nil
}

func (c *Cluster) DeleteMachine(machineName string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if id, ok := c.nodeNameToProviderID[machineName]; ok {
		delete(c.nodes, id)
		delete(c.nodeNameToProviderID, machineName)
		c.recordConsolidationChange()
	}
}

func (c *Cluster) DeleteNode(nodeName string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if id, ok := c.nodeNameToProviderID[nodeName]; ok {
		delete(c.nodes, id)
		delete(c.nodeNameToProviderID, nodeName)
		c.recordConsolidationChange()
	}
}

func (c *Cluster) UpdateMachine(ctx context.Context, machine *v1alpha1.Machine) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	// We don't consider machines without providerIDs
	if machine.Status.ProviderID == "" {
		return nil
	}
	// See if there is a node associated with this providerID
	node, err := c.getNodeByProviderID(ctx, machine.Status.ProviderID)
	if err != nil {
		return fmt.Errorf("getting node by providerID [%s], %w", machine.Status.ProviderID, err)
	}

	oldNode, foundOldNode := c.nodes[machine.Status.ProviderID]
	stateNode, err := c.makeMachineOwnedNode(ctx, machine, node)
	if err != nil {
		return fmt.Errorf("creating state node, %w", err)
	}
	if foundOldNode {
		stateNode.MarkedForDeletion = stateNode.MarkedForDeletion || oldNode.MarkedForDeletion
		if shouldTriggerConsolidation(oldNode, stateNode) {
			c.recordConsolidationChange()
		}
	}
	c.updateStateNode(machine.Status.ProviderID, stateNode)
	c.nodeNameToProviderID[stateNode.Node.Name] = machine.Status.ProviderID
	return nil
}

// UpdateNode is called for every node reconciliation
func (c *Cluster) UpdateNode(ctx context.Context, node *v1.Node) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	// If node doesn't have ProviderID, we mock it as the UID
	if node.Spec.ProviderID == "" {
		node.Spec.ProviderID = string(node.UID)
	}
	// See if there is a machine associated with this providerID
	machine, err := c.getMachineByProviderID(ctx, node.Spec.ProviderID)
	if err != nil {
		return fmt.Errorf("getting machine for node, %w", err)
	}

	oldNode, foundOldNode := c.nodes[node.Spec.ProviderID]
	var stateNode *Node

	// This is a machine-owned node
	if machine != nil {
		if stateNode, err = c.makeMachineOwnedNode(ctx, machine, node); err != nil {
			return fmt.Errorf("creating state node, %w", err)
		}
	} else {
		if stateNode, err = c.newNode(ctx, node); err != nil {
			return fmt.Errorf("creating state node, %w", err)
		}
	}
	if foundOldNode {
		stateNode.MarkedForDeletion = stateNode.MarkedForDeletion || oldNode.MarkedForDeletion
		if shouldTriggerConsolidation(oldNode, stateNode) {
			c.recordConsolidationChange()
		}
	}
	c.updateStateNode(node.Spec.ProviderID, stateNode)
	c.nodeNameToProviderID[stateNode.Node.Name] = node.Spec.ProviderID
	return nil
}

func (c *Cluster) updateStateNode(providerID string, stateNode *Node) {
	// Node pointer should be updated if it exists, else we have to create a new entry in the map
	if _, ok := c.nodes[providerID]; ok {
		*c.nodes[providerID] = *stateNode
	} else {
		c.nodes[providerID] = stateNode
	}
}

func (c *Cluster) makeMachineOwnedNode(ctx context.Context, machine *v1alpha1.Machine, node *v1.Node) (*Node, error) {
	// If this machine isn't initialized, use the machine as its representation in state
	if node == nil || machine.StatusConditions().GetCondition(v1alpha1.MachineInitialized) == nil ||
		machine.StatusConditions().GetCondition(v1alpha1.MachineInitialized).Status != v1.ConditionTrue {
		return c.newInflightNode(machine), nil
	}
	stateNode, err := c.newNode(ctx, node)
	if err != nil {
		return nil, fmt.Errorf("creating new node, %w", err)
	}
	stateNode.Initialized = true
	stateNode.MarkedForDeletion = stateNode.MarkedForDeletion || !machine.DeletionTimestamp.IsZero()
	return stateNode, nil
}

func (c *Cluster) getMachineByProviderID(ctx context.Context, providerID string) (*v1alpha1.Machine, error) {
	machineList := &v1alpha1.MachineList{}
	if err := c.kubeClient.List(ctx, machineList, client.MatchingFields{"status.providerID": providerID}); err != nil {
		return nil, fmt.Errorf("listing machines for providerID [%s], %w", providerID, err)
	}
	if len(machineList.Items) > 1 {
		return nil, fmt.Errorf("multiple machines mapped to providerID [%s]", providerID)
	}
	if len(machineList.Items) == 0 {
		return nil, nil
	}
	return &machineList.Items[0], nil
}

func (c *Cluster) getNodeByProviderID(ctx context.Context, providerID string) (*v1.Node, error) {
	nodeList := &v1.NodeList{}
	if err := c.kubeClient.List(ctx, nodeList, client.MatchingFields{"spec.providerID": providerID}); err != nil {
		return nil, fmt.Errorf("listing nodes for providerID [%s], %w", providerID, err)
	}
	if len(nodeList.Items) > 1 {
		return nil, fmt.Errorf("multiple nodes mapped to providerID [%s]", providerID)
	}
	if len(nodeList.Items) == 0 {
		return nil, nil
	}
	return &nodeList.Items[0], nil
}

// ClusterConsolidationState returns a number representing the state of the cluster with respect to consolidation.  If
// consolidation can't occur and this number hasn't changed, there is no point in re-attempting consolidation. This
// allows reducing overall CPU utilization by pausing consolidation when the cluster is in a static state.
func (c *Cluster) ClusterConsolidationState() int64 {
	cs := atomic.LoadInt64(&c.consolidationState)
	// If 5 minutes elapsed since the last time the consolidation state was changed, we change the state anyway. This
	// ensures that at least once every 5 minutes we consider consolidating our cluster in case something else has
	// changed (e.g. instance type availability) that we can't detect which would allow consolidation to occur.
	if c.clock.Now().After(time.UnixMilli(cs).Add(5 * time.Minute)) {
		c.recordConsolidationChange()
		return atomic.LoadInt64(&c.consolidationState)
	}
	return cs
}

// DeletePod is called when the pod has been deleted
func (c *Cluster) DeletePod(podKey types.NamespacedName) {
	c.antiAffinityPods.Delete(podKey)
	c.updateNodeUsageFromPodCompletion(podKey)
	c.recordConsolidationChange()
}

// UpdatePod is called every time the pod is reconciled
func (c *Cluster) UpdatePod(ctx context.Context, pod *v1.Pod) error {
	if podutils.IsTerminal(pod) {
		c.updateNodeUsageFromPodCompletion(client.ObjectKeyFromObject(pod))
	} else {
		if err := c.updateNodeUsageFromPod(ctx, pod); err != nil {
			return fmt.Errorf("updating node usage for pod, %w", err)
		}
	}
	c.updatePodAntiAffinities(pod)
	return nil
}

func (c *Cluster) updatePodAntiAffinities(pod *v1.Pod) {
	// We intentionally don't track inverse anti-affinity preferences. We're not
	// required to enforce them, so it just adds complexity for very little
	// value. The problem with them comes from the relaxation process, the pod
	// we are relaxing is not the pod with the anti-affinity term.
	if podutils.HasRequiredPodAntiAffinity(pod) {
		c.antiAffinityPods.Store(client.ObjectKeyFromObject(pod), pod)
	} else {
		c.antiAffinityPods.Delete(client.ObjectKeyFromObject(pod))
	}
}

// updateNodeUsageFromPod is called every time a reconcile event occurs for the pod. If the pods binding has changed
// (unbound to bound), we need to update the resource requests on the node.
func (c *Cluster) updateNodeUsageFromPod(ctx context.Context, pod *v1.Pod) error {
	podKey := client.ObjectKeyFromObject(pod)

	// nothing to do if the pod isn't bound, checking early allows avoiding unnecessary locking
	if pod.Spec.NodeName == "" {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	// If we haven't gotten the node update yet, we should error, retry and wait for the node update
	providerID, ok := c.nodeNameToProviderID[pod.Spec.NodeName]
	if !ok {
		return fmt.Errorf("bound node not found in cluster state")
	}
	node, ok := c.nodes[providerID]
	if !ok {
		return fmt.Errorf("bound node not found in cluster state")
	}

	if n, ok := c.bindings[podKey]; ok {
		// No need to update anything if pod binding hasn't changed
		if n.Node.Name == pod.Spec.NodeName {
			return nil
		}
		// If the binding changed, we know the pod was deleted and re-created, so we should
		// delete the pod resources from the old node state
		delete(c.bindings, podKey)
		delete(n.podRequests, podKey)
		delete(n.podLimits, podKey)

		n.Available = resources.Merge(n.Available, n.podRequests[podKey])
		n.PodTotalRequests = resources.Subtract(n.PodTotalRequests, n.podRequests[podKey])
		n.PodTotalLimits = resources.Subtract(n.PodTotalLimits, n.podLimits[podKey])
		n.HostPortUsage.DeletePod(podKey)
	}

	// sum the newly bound pod's requests and limits into the existing node and record the binding
	podRequests := resources.RequestsForPods(pod)
	podLimits := resources.LimitsForPods(pod)
	// our available capacity goes down by the amount that the pod had requested
	node.Available = resources.Subtract(node.Available, podRequests)
	node.PodTotalRequests = resources.Merge(node.PodTotalRequests, podRequests)
	node.PodTotalLimits = resources.Merge(node.PodTotalLimits, podLimits)
	// if it's a daemonset, we track what it has requested separately
	if podutils.IsOwnedByDaemonSet(pod) {
		node.DaemonSetRequested = resources.Merge(node.DaemonSetRequested, podRequests)
		node.DaemonSetLimits = resources.Merge(node.DaemonSetRequested, podLimits)
	}
	node.HostPortUsage.Add(ctx, pod)
	node.VolumeUsage.Add(ctx, pod)
	node.podRequests[podKey] = podRequests
	node.podLimits[podKey] = podLimits
	c.bindings[podKey] = node
	return nil
}

func (c *Cluster) updateNodeUsageFromPodCompletion(podKey types.NamespacedName) {
	c.mu.Lock()
	defer c.mu.Unlock()

	n, bindingKnown := c.bindings[podKey]
	if !bindingKnown {
		// we didn't think the pod was bound, so we weren't tracking it and don't need to do anything
		return
	}
	delete(c.bindings, podKey)

	// pod has been deleted so our available capacity increases by the resources that had been
	// requested by the pod
	n.Available = resources.Merge(n.Available, n.podRequests[podKey])
	n.PodTotalRequests = resources.Subtract(n.PodTotalRequests, n.podRequests[podKey])
	n.PodTotalLimits = resources.Subtract(n.PodTotalLimits, n.podLimits[podKey])
	delete(n.podRequests, podKey)
	delete(n.podLimits, podKey)
	n.HostPortUsage.DeletePod(podKey)
	n.VolumeUsage.DeletePod(podKey)

	// We can't easily track the changes to the DaemonsetRequested here as we no longer have the pod.  We could keep up
	// with this separately, but if a daemonset pod is being deleted, it usually means the node is going down.  In the
	// worst case we will resync to correct this.
}

func (c *Cluster) recordConsolidationChange() {
	atomic.StoreInt64(&c.consolidationState, c.clock.Now().UnixMilli())
}

func shouldTriggerConsolidation(oldNode, newNode *Node) bool {
	return oldNode.Initialized != newNode.Initialized ||
		oldNode.MarkedForDeletion != newNode.MarkedForDeletion
}

// Reset the cluster state for unit testing
func (c *Cluster) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nodes = map[string]*Node{}
	c.bindings = map[types.NamespacedName]*Node{}
	c.antiAffinityPods = sync.Map{}
}
