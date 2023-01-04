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

	"github.com/aws/karpenter-core/pkg/apis/config/settings"
	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/scheduling"
	podutils "github.com/aws/karpenter-core/pkg/utils/pod"
	"github.com/aws/karpenter-core/pkg/utils/resources"
)

// Node is a cached version of a node in the cluster that maintains state which is expensive to compute every time it's
// needed.  This currently contains node utilization across all the allocatable resources, but will soon be used to
// compute topology information.
// +k8s:deepcopy-gen=true
type Node struct {
	node *v1.Node

	inflightAllocatable v1.ResourceList
	inflightCapacity    v1.ResourceList
	startupTaints       []v1.Taint

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
	nominatedUntil    metav1.Time
}

// Node returns the node object wrapped by this state Node
// Note: Only use this if you need the object reference, don't access details of this data as there are higher-order
// functions to handle things like node capacity, available, etc.
func (in *Node) Node() *v1.Node {
	return in.node
}

func (in *Node) StartupTaints() []v1.Taint {
	return in.startupTaints
}

func (in *Node) Initialized() bool {
	if in.node == nil {
		return false
	}
	return in.node.Labels[v1alpha5.LabelNodeInitialized] == "true"
}

func (in *Node) Capacity() v1.ResourceList {
	if !in.Initialized() && in.Owned() {
		// Override any zero quantity values in the node status
		ret := lo.Assign(in.node.Status.Capacity)
		for resourceName, quantity := range in.inflightCapacity {
			if resources.IsZero(ret[resourceName]) {
				ret[resourceName] = quantity
			}
		}
	}
	return in.node.Status.Capacity
}

func (in *Node) Allocatable() v1.ResourceList {
	if !in.Initialized() && in.Owned() {
		// Override any zero quantity values in the node status
		ret := lo.Assign(in.node.Status.Allocatable)
		for resourceName, quantity := range in.inflightAllocatable {
			if resources.IsZero(ret[resourceName]) {
				ret[resourceName] = quantity
			}
		}
		return ret
	}
	return in.node.Status.Allocatable
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

func (in *Node) VolumeUsage() *scheduling.VolumeLimits {
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
	return in.markedForDeletion || (in.node != nil && !in.node.DeletionTimestamp.IsZero())
}

func (in *Node) Nominate(ctx context.Context) {
	in.nominatedUntil = metav1.Time{Time: time.Now().Add(getNominationWindow(ctx))}
}

func (in *Node) IsNominated() bool {
	return in.nominatedUntil.After(time.Now())
}

func (in *Node) Owned() bool {
	if in.node == nil {
		return false
	}
	return in.node.Labels[v1alpha5.ProvisionerNameLabelKey] != ""
}

func (in *Node) updateForPod(ctx context.Context, pod *v1.Pod) {
	podKey := client.ObjectKeyFromObject(pod)

	// sum the newly bound pod's requests and limits into the existing node and record the binding
	in.podRequests[podKey] = resources.RequestsForPods(pod)
	in.podLimits[podKey] = resources.LimitsForPods(pod)
	// TODO @joinnis: potentially point this to the actual state node
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

func getNominationWindow(ctx context.Context) time.Duration {
	nominationPeriod := 2 * settings.FromContext(ctx).BatchMaxDuration.Duration
	if nominationPeriod < 10*time.Second {
		nominationPeriod = 10 * time.Second
	}
	return nominationPeriod
}
