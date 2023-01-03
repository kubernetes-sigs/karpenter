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
	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/cloudprovider"
	"github.com/aws/karpenter-core/pkg/scheduling"
	podutils "github.com/aws/karpenter-core/pkg/utils/pod"
	"github.com/aws/karpenter-core/pkg/utils/resources"
)

const nominationKey = "nominated"

// Cluster maintains cluster state that is often needed but expensive to compute.
type Cluster struct {
	kubeClient    client.Client
	cloudProvider cloudprovider.CloudProvider
	clock         clock.Clock

	// State: Node Status & Pod -> Node Binding
	mu                   sync.RWMutex
	nodes                map[string]*Node                // provider id -> node
	bindings             map[types.NamespacedName]string // pod namespaced named -> node node
	nodeNameToProviderID map[string]string               // node name -> provider id
	antiAffinityPods     sync.Map                        // mapping of pod namespaced name to *v1.Pod of pods that have required anti affinities

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
		clock:          clk,
		kubeClient:     client,
		cloudProvider:  cp,
		nominatedNodes: cache.New(nominationPeriod, 10*time.Second),
		nodes:          map[string]*Node{},
		bindings:       map[types.NamespacedName]string{},
	}
	return c
}

// Node is a cached version of a node in the cluster that maintains state which is expensive to compute every time it's
// needed.  This currently contains node utilization across all the allocatable resources, but will soon be used to
// compute topology information.
// +k8s:deepcopy-gen=true
type Node struct {
	node    *v1.Node
	machine *v1alpha5.Machine

	// daemonSetRequests is the total amount of resources that have been requested by daemon sets.  This allows users
	// of the Node to identify the remaining resources that we expect future daemonsets to consume.  This is already
	// included in the calculation for Available.
	daemonSetRequests map[types.NamespacedName]v1.ResourceList
	daemonSetLimits   map[types.NamespacedName]v1.ResourceList

	podRequests map[types.NamespacedName]v1.ResourceList
	podLimits   map[types.NamespacedName]v1.ResourceList

	hostPortUsage *scheduling.HostPortUsage
	volumeUsage   *scheduling.VolumeLimits
	volumeLimits  scheduling.VolumeCount

	markedForDeletion bool
	nominated         *cache.Cache // stores a single key "nominated"
}

func (n *Node) Node() *v1.Node {
	// TODO @joinnis: update this to look at both machine and node
	return n.node
}

func (n *Node) Initialized() bool {
	// TODO @joinnis: update this to look at both machine and node
	return n.node.Labels[v1alpha5.LabelNodeInitialized] == "true"
}

func (n *Node) Capacity() v1.ResourceList {
	// TODO @joinnis: update this to look at both machine and node
	return n.node.Status.Capacity
}

func (n *Node) Allocatable() v1.ResourceList {
	// TODO @joinnis: update this to look at both machine and node
	return n.node.Status.Allocatable
}

// Available is allocatable minus anything allocated to pods.
func (n *Node) Available() v1.ResourceList {
	// TODO @joinnis: update this to look at both machine and node
	return resources.Subtract(n.Allocatable(), n.PodRequests())
}

func (n *Node) DaemonSetRequests() v1.ResourceList {
	return n.daemonSetRequests
}

func (n *Node) DaemonSetLimits() v1.ResourceList {
	return n.daemonSetLimits
}

func (n *Node) HostPortUsage() *scheduling.HostPortUsage {
	return n.hostPortUsage
}

func (n *Node) VolumeUsage() *scheduling.VolumeLimits {
	return n.volumeUsage
}

func (n *Node) VolumeLimits() scheduling.VolumeCount {
	return n.volumeLimits
}

func (n *Node) PodRequests() v1.ResourceList {
	return resources.Merge(lo.Values(n.podRequests)...)
}

func (n *Node) PodLimits() v1.ResourceList {
	return resources.Merge(lo.Values(n.podLimits)...)
}

func (n *Node) MarkedForDeletion() bool {
	// TODO: update this to also look at the deletion timestamp
	return n.markedForDeletion
}

func (n *Node) Nominate() {
	n.nominated.SetDefault(nominationKey, true)
}

func (n *Node) IsNominated() bool {
	_, ok := n.nominated.Get(nominationKey)
	return ok
}

func (n *Node) Owned() bool {
	// TODO: update this to also look at the deletion timestamp
	return n.node.Labels[v1alpha5.ProvisionerNameLabelKey] != ""
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
		node, ok := c.nodes[nodeName]
		if !ok {
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
	var nodes []*Node
	for _, node := range c.nodes {
		nodes = append(nodes, node)
	}
	// sort nodes by creation time so we provide a consistent ordering
	sort.Slice(nodes, func(a, b int) bool {
		if nodes[a].Node().CreationTimestamp != nodes[b].Node().CreationTimestamp {
			return nodes[a].Node().CreationTimestamp.Time.Before(nodes[b].Node().CreationTimestamp.Time)
		}
		// sometimes we get nodes created in the same second, so sort again by node UID to provide a consistent ordering
		return nodes[a].Node().UID < nodes[b].Node().UID
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
		return c.nodes[id].IsNominated()
	}
	return false
}

// NominateNodeForPod records that a node was the target of a pending pod during a scheduling batch
func (c *Cluster) NominateNodeForPod(name string) error {
	if id, ok := c.nodeNameToProviderID[name]; ok {
		c.nodes[id].Nominate()
		return nil
	}
	return fmt.Errorf("node name doesn't exist in state")
}

// UnmarkForDeletion removes the marking on the node as a node the controller intends to delete
func (c *Cluster) UnmarkForDeletion(names ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for _, name := range names {
		if id, ok := c.nodeNameToProviderID[name]; ok {
			c.nodes[id].markedForDeletion = false
		}
	}
}

// MarkForDeletion marks the node as pending deletion in the internal cluster state
func (c *Cluster) MarkForDeletion(names ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for _, name := range names {
		if id, ok := c.nodeNameToProviderID[name]; ok {
			c.nodes[id].markedForDeletion = true
		}
	}
}

// newNode always returns a node, even if some portion of the update has failed
func (c *Cluster) newNode(ctx context.Context, node *v1.Node) (*Node, error) {
	n := &Node{
		node:              node,
		HostPortUsage:     scheduling.NewHostPortUsage(),
		VolumeUsage:       scheduling.NewVolumeLimits(c.kubeClient),
		VolumeLimits:      scheduling.VolumeCount{},
		MarkedForDeletion: !node.DeletionTimestamp.IsZero(),
		podRequests:       map[types.NamespacedName]v1.ResourceList{},
		podLimits:         map[types.NamespacedName]v1.ResourceList{},
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

// nolint:gocyclo
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
	for i := range pods.Items {
		pod := &pods.Items[i]
		if podutils.IsTerminal(pod) {
			continue
		}
		podKey := client.ObjectKeyFromObject(pod)

		n.podRequests[podKey] = resources.RequestsForPods(pod)
		n.podLimits[podKey] = resources.LimitsForPods(pod)
		c.bindings[podKey] = pod.Spec.NodeName // TODO @joinnis: Potentially change this later
		if podutils.IsOwnedByDaemonSet(pod) {
			n.daemonSetRequests[podKey] = resources.RequestsForPods(pod)
			n.daemonSetLimits[podKey] = resources.LimitsForPods(pod)
		}
		n.hostPortUsage.Add(ctx, pod)
		n.volumeUsage.Add(ctx, pod)
	}
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
		n.volumeLimits[driver.Name] = int(ptr.Int32Value(driver.Allocatable.Count))
	}
	return nil
}

func (c *Cluster) DeleteNode(nodeName string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.nodes, nodeName)
	c.recordConsolidationChange()
}

// UpdateNode is called for every node reconciliation
func (c *Cluster) UpdateNode(ctx context.Context, node *v1.Node) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	n, err := c.newNode(ctx, node)
	if err != nil {
		return err
	}

	oldNode, ok := c.nodes[node.Name]
	// If the old node existed and its initialization status changed, we want to reconsider consolidation.  This handles
	// a situation where we re-start with an unready node and it becomes ready later.
	if ok {
		if oldNode.Node.Labels[v1alpha5.LabelNodeInitialized] != n.Node.Labels[v1alpha5.LabelNodeInitialized] {
			c.recordConsolidationChange()
		}
		// We mark the node for deletion either:
		// 1. If the DeletionTimestamp is set (the node is explicitly being deleted)
		// 2. If the last state of the node has the node MarkedForDeletion
		n.MarkedForDeletion = n.MarkedForDeletion || oldNode.MarkedForDeletion
	}
	c.nodes[node.Name] = n
	return nil
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

func (c *Cluster) updateNodeUsageFromPodCompletion(podKey types.NamespacedName) {
	c.mu.Lock()
	defer c.mu.Unlock()

	nodeName, bindingKnown := c.bindings[podKey]
	if !bindingKnown {
		// we didn't think the pod was bound, so we weren't tracking it and don't need to do anything
		return
	}

	delete(c.bindings, podKey)
	n, ok := c.nodes[nodeName]
	if !ok {
		// we weren't tracking the node yet, so nothing to do
		return
	}
	// pod has been deleted so our available capacity increases by the resources that had been
	// requested by the pod
	delete(n.podRequests, podKey)
	delete(n.podLimits, podKey)
	n.hostPortUsage.DeletePod(podKey)
	n.volumeUsage.DeletePod(podKey)

	// We can't easily track the changes to the DaemonsetRequested here as we no longer have the pod.  We could keep up
	// with this separately, but if a daemonset pod is being deleted, it usually means the node is going down.  In the
	// worst case we will resync to correct this.
}

// UpdatePod is called every time the pod is reconciled
func (c *Cluster) UpdatePod(ctx context.Context, pod *v1.Pod) error {
	var err error
	if podutils.IsTerminal(pod) {
		c.updateNodeUsageFromPodCompletion(client.ObjectKeyFromObject(pod))
	} else {
		err = c.updateNodeUsageFromPod(ctx, pod)
	}
	c.updatePodAntiAffinities(pod)
	return err
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

// updateNodeUsageFromPod is called every time a reconcile event occurs for the pod. If the pods binding has changed
// (unbound to bound), we need to update the resource requests on the node.
func (c *Cluster) updateNodeUsageFromPod(ctx context.Context, pod *v1.Pod) error {
	// nothing to do if the pod isn't bound, checking early allows avoiding unnecessary locking
	if pod.Spec.NodeName == "" {
		return nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	podKey := client.ObjectKeyFromObject(pod)
	oldNodeName, bindingKnown := c.bindings[podKey]
	if bindingKnown {
		if oldNodeName == pod.Spec.NodeName {
			// we are already tracking the pod binding, so nothing to update
			return nil
		}
		// the pod has switched nodes, this can occur if a pod name was re-used and it was deleted/re-created rapidly,
		// binding to a different node the second time
		n, ok := c.nodes[oldNodeName]
		if ok {
			// we were tracking the old node, so we need to reduce its capacity by the amount of the pod that has
			// left it
			delete(c.bindings, podKey)
			n.hostPortUsage.DeletePod(podKey)
			delete(n.podRequests, podKey)
			delete(n.podLimits, podKey)
		}
	} else {
		// new pod binding has occurred
		c.recordConsolidationChange()
	}

	// did we notice that the pod is bound to a node and didn't know about the node before?
	n, ok := c.nodes[pod.Spec.NodeName]
	if !ok {
		var node v1.Node
		if err := c.kubeClient.Get(ctx, client.ObjectKey{Name: pod.Spec.NodeName}, &node); err != nil {
			return client.IgnoreNotFound(fmt.Errorf("getting node, %w", err))
		}

		var err error
		// node didn't exist, but creating it will pick up this newly bound pod as well
		n, err = c.newNode(ctx, &node)
		if err != nil {
			// no need to delete c.nodes[node.Name] as it wasn't stored previously
			return err
		}
		c.nodes[node.Name] = n
		return nil
	}

	// sum the newly bound pod's requests and limits into the existing node and record the binding
	n.podRequests[podKey] = resources.RequestsForPods(pod)
	n.podLimits[podKey] = resources.LimitsForPods(pod)
	c.bindings[podKey] = pod.Spec.NodeName // TODO @joinnis: potentially point this to the actual state node
	// if it's a daemonset, we track what it has requested separately
	if podutils.IsOwnedByDaemonSet(pod) {
		n.daemonSetRequests[podKey] = resources.RequestsForPods(pod)
		n.daemonSetLimits[podKey] = resources.LimitsForPods(pod)
	}
	n.hostPortUsage.Add(ctx, pod)
	n.volumeUsage.Add(ctx, pod)

	return nil
}

func (c *Cluster) recordConsolidationChange() {
	atomic.StoreInt64(&c.consolidationState, c.clock.Now().UnixMilli())
}

// Reset the cluster state for unit testing
func (c *Cluster) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nodes = map[string]*Node{}
	c.bindings = map[types.NamespacedName]string{}
	c.antiAffinityPods = sync.Map{}
}
