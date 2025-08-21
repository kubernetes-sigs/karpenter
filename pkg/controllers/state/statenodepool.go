/*
Copyright The Kubernetes Authors.

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
	"sync"
	"sync/atomic"

	"github.com/samber/lo"
	"k8s.io/apimachinery/pkg/util/sets"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

// Tracks vended limits for a NodePool that are in-flight
type NodePoolLimit struct {
	ReservedNodeLimit atomic.Int64
}

type NodeClaimState struct {
	Running  sets.Set[string]
	Deleting sets.Set[string]
}

// StateNodePool is a cached version of a NodePool in the cluster that maintains state which is expensive to compute every time it's needed.
type NodePoolState struct {
	npMutex sync.RWMutex

	nodePoolNameToNodeClaimState map[string]NodeClaimState // node pool name -> node claim state (Running and Deleting node claim names)
	nodeClaimNameToNodePoolName  map[string]string         // node claim name -> node pool name
	nodePoolNameToNodePoolLimit  map[string]*NodePoolLimit // node pool -> nodepool limit

}

func NewNodePoolState() *NodePoolState {
	return &NodePoolState{
		nodePoolNameToNodeClaimState: map[string]NodeClaimState{},
		nodeClaimNameToNodePoolName:  map[string]string{},
		nodePoolNameToNodePoolLimit:  map[string]*NodePoolLimit{},
	}
}

// Helper methods that hold the lock

// Sets up the NodePoolState to track NodeClaim
func (n *NodePoolState) SetNodeClaimMapping(npName, ncName string) {
	if npName == "" || ncName == "" {
		return
	}
	n.npMutex.Lock()
	defer n.npMutex.Unlock()

	n.ensureNodePoolEntry(npName)
	n.nodeClaimNameToNodePoolName[ncName] = npName
}

// ReleaseNodePoolNodeLimit releases the NodePoolsNodeLimit
func (n *NodePoolState) ReleaseNodePoolNodeLimit(npName string) {
	n.npMutex.Lock()
	defer n.npMutex.Unlock()

	currentlyReserved := n.nodePoolNameToNodePoolLimit[npName].ReservedNodeLimit.Load()
	if currentlyReserved > 0 {
		n.nodePoolNameToNodePoolLimit[npName].ReservedNodeLimit.Store(currentlyReserved - 1)
	}
}

// Marks the given NodeClaim as running in NodePoolState
func (n *NodePoolState) MarkNodeClaimRunning(npName, ncName string) {
	n.npMutex.Lock()
	defer n.npMutex.Unlock()
	n.ensureNodePoolEntry(npName)

	n.nodePoolNameToNodeClaimState[npName].Deleting.Delete(ncName)
	n.nodePoolNameToNodeClaimState[npName].Running.Insert(ncName)
}

// Marks the given NodeClaim as Deleting in NodePoolState
func (n *NodePoolState) MarkNodeClaimDeleting(npName, ncName string) {
	n.npMutex.Lock()
	defer n.npMutex.Unlock()
	n.ensureNodePoolEntry(npName)

	n.nodePoolNameToNodeClaimState[npName].Deleting.Insert(ncName)
	n.nodePoolNameToNodeClaimState[npName].Running.Delete(ncName)
}

// Cleans up the NodeClaim in NodePoolState
func (n *NodePoolState) CleanupNodeClaim(ncName string) {
	n.npMutex.Lock()
	defer n.npMutex.Unlock()

	npName := n.nodeClaimNameToNodePoolName[ncName]

	n.nodePoolNameToNodeClaimState[npName].Deleting.Delete(ncName)
	n.nodePoolNameToNodeClaimState[npName].Running.Delete(ncName)
	delete(n.nodeClaimNameToNodePoolName, ncName)
}

func (n *NodePoolState) IsNodeClaimRunning(npName, ncName string) bool {
	n.npMutex.RLock()
	defer n.npMutex.RUnlock()

	return n.nodePoolNameToNodeClaimState[npName].Running.Has(ncName)
}

// Methods that expect the called to hold the lock

// ReserveNodePoolNodeLimit attempts to reserve nodes against a NodePool's limit.
// It ensures that the total of running nodes + deleting nodes + reserved nodes doesn't exceed the limit.
func (n *NodePoolState) ReserveNodePoolNodeLimit(np string, limit int64, wantedLimit int64) int64 {
	n.ensureNodePoolEntry(np)
	running, deleting := n.NodePoolNodeCounts(np)

	for {
		currentlyReserved := n.nodePoolNameToNodePoolLimit[np].ReservedNodeLimit.Load()
		remainingLimit := limit - int64(running+deleting) - currentlyReserved
		if remainingLimit < 0 {
			return 0
		}
		grantedLimit := lo.Ternary(wantedLimit > remainingLimit,
			remainingLimit,
			wantedLimit,
		)

		if n.nodePoolNameToNodePoolLimit[np].ReservedNodeLimit.CompareAndSwap(currentlyReserved, currentlyReserved+grantedLimit) {
			return grantedLimit
		}
	}
}

// Returns the current NodeClaims for a NodePool by its state (running, deleting)
func (n *NodePoolState) NodePoolNodeCounts(npName string) (running, deleting int) {
	if st, ok := n.nodePoolNameToNodeClaimState[npName]; ok {
		return len(st.Running), len(st.Deleting)
	}
	return 0, 0
}

// Updates the NodeClaim state and releases the Limit if NodeClaim transitions from not Running to Running
func (n *NodePoolState) UpdateNodePoolNodeClaim(nodeClaim *v1.NodeClaim, markedForDeletion bool) {
	// If we are launching/deleting a NodeClaim we need to track the state of the NodeClaim and its limits
	npName := nodeClaim.Labels[v1.NodePoolLabelKey]
	n.SetNodeClaimMapping(npName, nodeClaim.Name)
	wasNodeClaimRunning := n.IsNodeClaimRunning(npName, nodeClaim.Name)

	// If our node is marked for deletion, we need to make sure that we delete it
	if markedForDeletion {
		n.MarkNodeClaimDeleting(npName, nodeClaim.Name)
	} else {
		n.MarkNodeClaimRunning(npName, nodeClaim.Name)

	}

	// if we transition from not running to running then we release the limit reservation
	if !wasNodeClaimRunning && n.IsNodeClaimRunning(npName, nodeClaim.Name) {
		n.ReleaseNodePoolNodeLimit(npName)
	}
}

func (n *NodePoolState) ensureNodePoolEntry(np string) {
	if _, ok := n.nodePoolNameToNodeClaimState[np]; !ok {
		n.nodePoolNameToNodeClaimState[np] = NodeClaimState{
			Running:  sets.New[string](),
			Deleting: sets.New[string](),
		}
	}
	if _, ok := n.nodePoolNameToNodePoolLimit[np]; !ok {
		nl := &NodePoolLimit{}
		nl.ReservedNodeLimit.Store(0)
		n.nodePoolNameToNodePoolLimit[np] = nl
	}
}
