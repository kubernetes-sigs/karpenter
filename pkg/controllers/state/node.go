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
	"time"

	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/aws/karpenter-core/pkg/apis/settings"
	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/scheduling"
	nodeutils "github.com/aws/karpenter-core/pkg/utils/node"
	podutils "github.com/aws/karpenter-core/pkg/utils/pod"
	"github.com/aws/karpenter-core/pkg/utils/resources"
)

// Nodes is a typed version of a list of *Node
type Nodes []*Node

// Active filters nodes that are not in a MarkedForDeletion state
func (n Nodes) Active() Nodes {
	return lo.Filter(n, func(node *Node, _ int) bool {
		return !node.MarkedForDeletion()
	})
}

// Deleting filters nodes that are in a MarkedForDeletion state
func (n Nodes) Deleting() Nodes {
	return lo.Filter(n, func(node *Node, _ int) bool {
		return node.MarkedForDeletion()
	})
}

// Pods gets the pods assigned to all Nodes based on the kubernetes api-server bindings
func (n Nodes) Pods(ctx context.Context, c client.Client) ([]*v1.Pod, error) {
	var pods []*v1.Pod
	for _, node := range n {
		p, err := node.Pods(ctx, c)
		if err != nil {
			return nil, err
		}
		pods = append(pods, p...)
	}
	return pods, nil
}

// Node is a cached version of a node in the cluster that maintains state which is expensive to compute every time it's
// needed.  This currently contains node utilization across all the allocatable resources, but will soon be used to
// compute topology information.
// +k8s:deepcopy-gen=true
type Node struct {
	Node    *v1.Node
	Machine *v1alpha5.Machine

	inflightAllocatable v1.ResourceList // TODO @joinnis: This can be removed when machine is added
	inflightCapacity    v1.ResourceList // TODO @joinnis: This can be removed when machine is added
	startupTaints       []v1.Taint      // TODO: @joinnis: This can be removed when machine is added

	// daemonSetRequests is the total amount of resources that have been requested by daemon sets. This allows users
	// of the Node to identify the remaining resources that we expect future daemonsets to consume.
	daemonSetRequests map[types.NamespacedName]v1.ResourceList
	daemonSetLimits   map[types.NamespacedName]v1.ResourceList

	podRequests map[types.NamespacedName]v1.ResourceList
	podLimits   map[types.NamespacedName]v1.ResourceList

	hostPortUsage *scheduling.HostPortUsage
	volumeUsage   *scheduling.VolumeUsage
	volumeLimits  scheduling.VolumeCount

	markedForDeletion bool
	nominatedUntil    metav1.Time
}

func NewNode() *Node {
	return &Node{
		inflightAllocatable: v1.ResourceList{},
		inflightCapacity:    v1.ResourceList{},
		startupTaints:       []v1.Taint{},
		daemonSetRequests:   map[types.NamespacedName]v1.ResourceList{},
		daemonSetLimits:     map[types.NamespacedName]v1.ResourceList{},
		podRequests:         map[types.NamespacedName]v1.ResourceList{},
		podLimits:           map[types.NamespacedName]v1.ResourceList{},
		hostPortUsage:       &scheduling.HostPortUsage{},
		volumeUsage:         &scheduling.VolumeUsage{},
		volumeLimits:        scheduling.VolumeCount{},
	}
}

func (in *Node) Name() string {
	if !in.Initialized() && in.Machine != nil {
		return in.Machine.Name
	}
	return in.Node.Name
}

// Pods gets the pods assigned to the Node based on the kubernetes api-server bindings
func (in *Node) Pods(ctx context.Context, c client.Client) ([]*v1.Pod, error) {
	if in.Node == nil {
		return nil, nil
	}
	return nodeutils.GetNodePods(ctx, c, in.Node)
}

func (in *Node) HostName() string {
	if in.Labels()[v1.LabelHostname] == "" {
		return in.Name()
	}
	return in.Labels()[v1.LabelHostname]
}

func (in *Node) Annotations() map[string]string {
	// If the machine exists and the state node isn't initialized
	// use the machine representation of the annotations
	if !in.Initialized() && in.Machine != nil {
		return in.Machine.Annotations
	}
	return in.Node.Annotations
}

func (in *Node) Labels() map[string]string {
	// If the machine exists and the state node isn't initialized
	// use the machine representation of the labels
	if !in.Initialized() && in.Machine != nil {
		return in.Machine.Labels
	}
	return in.Node.Labels
}

func (in *Node) Taints() []v1.Taint {
	ephemeralTaints := []v1.Taint{
		{Key: v1.TaintNodeNotReady, Effect: v1.TaintEffectNoSchedule},
		{Key: v1.TaintNodeUnreachable, Effect: v1.TaintEffectNoSchedule},
	}
	// Only consider startup taints until the node is initialized. Without this, if the startup taint is generic and
	// re-appears on the node for a different reason (e.g. the node is cordoned) we will assume that pods can
	// schedule against the node in the future incorrectly.
	if !in.Initialized() && in.Owned() {
		if in.Machine != nil {
			ephemeralTaints = append(ephemeralTaints, in.Machine.Spec.StartupTaints...)
		} else {
			ephemeralTaints = append(ephemeralTaints, in.startupTaints...)
		}
	}

	var taints []v1.Taint
	if !in.Initialized() && in.Machine != nil {
		taints = in.Machine.Spec.Taints
	} else {
		taints = in.Node.Spec.Taints
	}
	return lo.Reject(taints, func(taint v1.Taint, _ int) bool {
		_, rejected := lo.Find(ephemeralTaints, func(t v1.Taint) bool {
			return t.Key == taint.Key && t.Value == taint.Value && t.Effect == taint.Effect
		})
		return rejected
	})
}

// Initialized always implies that the node is there. If something is initialized, we are guaranteed that the Node
// exists inside of cluster state. If the node is not initialized, it is possible that it is represented by a Node or
// by a Machine inside of cluster state
func (in *Node) Initialized() bool {
	if in.Machine != nil {
		if in.Node != nil && in.Machine.StatusConditions().GetCondition(v1alpha5.MachineInitialized).IsTrue() {
			return true
		}
		return false
	}
	if in.Node != nil {
		return in.Node.Labels[v1alpha5.LabelNodeInitialized] == "true"
	}
	return false
}

func (in *Node) Capacity() v1.ResourceList {
	if !in.Initialized() && in.Machine != nil {
		// Override any zero quantity values in the node status
		if in.Node != nil {
			ret := lo.Assign(in.Node.Status.Capacity)
			for resourceName, quantity := range in.Machine.Status.Capacity {
				if resources.IsZero(ret[resourceName]) {
					ret[resourceName] = quantity
				}
			}
			return ret
		}
		return in.Machine.Status.Capacity
	}
	// TODO @joinnis: Remove this when machine migration is complete
	if !in.Initialized() && in.Owned() {
		// Override any zero quantity values in the node status
		ret := lo.Assign(in.Node.Status.Capacity)
		for resourceName, quantity := range in.inflightCapacity {
			if resources.IsZero(ret[resourceName]) {
				ret[resourceName] = quantity
			}
		}
		return ret
	}
	return in.Node.Status.Capacity
}

func (in *Node) Allocatable() v1.ResourceList {
	if !in.Initialized() && in.Machine != nil {
		// Override any zero quantity values in the node status
		if in.Node != nil {
			ret := lo.Assign(in.Node.Status.Allocatable)
			for resourceName, quantity := range in.Machine.Status.Allocatable {
				if resources.IsZero(ret[resourceName]) {
					ret[resourceName] = quantity
				}
			}
			return ret
		}
		return in.Machine.Status.Allocatable
	}
	// TODO @joinnis: Remove this when machine migration is complete
	if !in.Initialized() && in.Owned() {
		// Override any zero quantity values in the node status
		ret := lo.Assign(in.Node.Status.Allocatable)
		for resourceName, quantity := range in.inflightAllocatable {
			if resources.IsZero(ret[resourceName]) {
				ret[resourceName] = quantity
			}
		}
		return ret
	}
	return in.Node.Status.Allocatable
}

// Available is allocatable minus anything allocated to pods.
func (in *Node) Available() v1.ResourceList {
	return resources.Subtract(in.Allocatable(), in.PodRequests())
}

func (in *Node) DaemonSetRequests() v1.ResourceList {
	return resources.Merge(lo.Values(in.daemonSetRequests)...)
}

func (in *Node) DaemonSetLimits() v1.ResourceList {
	return resources.Merge(lo.Values(in.daemonSetLimits)...)
}

func (in *Node) HostPortUsage() *scheduling.HostPortUsage {
	return in.hostPortUsage
}

func (in *Node) VolumeUsage() *scheduling.VolumeUsage {
	return in.volumeUsage
}

func (in *Node) VolumeLimits() scheduling.VolumeCount {
	return in.volumeLimits
}

func (in *Node) PodRequests() v1.ResourceList {
	return resources.Merge(lo.Values(in.podRequests)...)
}

func (in *Node) PodLimits() v1.ResourceList {
	return resources.Merge(lo.Values(in.podLimits)...)
}

func (in *Node) MarkedForDeletion() bool {
	// The Node is marked for the Deletion if:
	//  1. The Node has explicitly MarkedForDeletion
	//  2. The Node has a Machine counterpart and is actively deleting
	//  3. The Node has no Machine counterpart and is actively deleting
	return in.markedForDeletion ||
		(in.Machine != nil && !in.Machine.DeletionTimestamp.IsZero()) ||
		(in.Node != nil && in.Machine == nil && !in.Node.DeletionTimestamp.IsZero())
}

func (in *Node) Nominate(ctx context.Context) {
	in.nominatedUntil = metav1.Time{Time: time.Now().Add(nominationWindow(ctx))}
}

func (in *Node) Nominated() bool {
	return in.nominatedUntil.After(time.Now())
}

func (in *Node) Owned() bool {
	return in.Labels()[v1alpha5.ProvisionerNameLabelKey] != ""
}

func (in *Node) updateForPod(ctx context.Context, pod *v1.Pod) {
	podKey := client.ObjectKeyFromObject(pod)

	in.podRequests[podKey] = resources.RequestsForPods(pod)
	in.podLimits[podKey] = resources.LimitsForPods(pod)
	// if it's a daemonset, we track what it has requested separately
	if podutils.IsOwnedByDaemonSet(pod) {
		in.daemonSetRequests[podKey] = resources.RequestsForPods(pod)
		in.daemonSetLimits[podKey] = resources.LimitsForPods(pod)
	}
	in.hostPortUsage.Add(ctx, pod)
	in.volumeUsage.Add(ctx, pod)
}

func (in *Node) cleanupForPod(podKey types.NamespacedName) {
	in.hostPortUsage.DeletePod(podKey)
	in.volumeUsage.DeletePod(podKey)
	delete(in.podRequests, podKey)
	delete(in.podLimits, podKey)
	delete(in.daemonSetRequests, podKey)
	delete(in.daemonSetLimits, podKey)
}

func nominationWindow(ctx context.Context) time.Duration {
	nominationPeriod := 2 * settings.FromContext(ctx).BatchMaxDuration.Duration
	if nominationPeriod < 10*time.Second {
		nominationPeriod = 10 * time.Second
	}
	return nominationPeriod
}
