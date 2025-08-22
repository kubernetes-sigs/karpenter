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

// Currently tracks in-flight NodeClaims for a NodePool
type NodePooltracker struct {
	ReservedNodeLimit atomic.Int64
}

type NodeClaimState struct {
	Active   sets.Set[string]
	Deleting sets.Set[string]
}

// StateNodePool is a cached version of a NodePool in the cluster that maintains state which is expensive to compute every time it's needed.
type NodePoolState struct {
	mu sync.RWMutex

	nodePoolNameToNodeClaimState map[string]NodeClaimState   // node pool name -> node claim state (Running and Deleting node claim names)
	nodeClaimNameToNodePoolName  map[string]string           // node claim name -> node pool name
	nodePoolNameToNodePoolLimit  map[string]*NodePooltracker // node pool -> nodepool limit

}

func NewNodePoolState() *NodePoolState {
	return &NodePoolState{
		nodePoolNameToNodeClaimState: map[string]NodeClaimState{},
		nodeClaimNameToNodePoolName:  map[string]string{},
		nodePoolNameToNodePoolLimit:  map[string]*NodePooltracker{},
	}
}

// Helper methods that hold the lock

// Sets up the NodePoolState to track NodeClaim
func (n *NodePoolState) SetNodeClaimMapping(npName, ncName string) {
	if npName == "" || ncName == "" {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()

	n.ensureNodePoolEntry(npName)
	n.nodeClaimNameToNodePoolName[ncName] = npName
}

// Marks the given NodeClaim as running in NodePoolState
func (n *NodePoolState) MarkNodeClaimRunning(npName, ncName string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.ensureNodePoolEntry(npName)

	n.nodePoolNameToNodeClaimState[npName].Deleting.Delete(ncName)
	n.nodePoolNameToNodeClaimState[npName].Active.Insert(ncName)
}

// Marks the given NodeClaim as Deleting in NodePoolState
func (n *NodePoolState) MarkNodeClaimDeleting(npName, ncName string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.ensureNodePoolEntry(npName)

	n.nodePoolNameToNodeClaimState[npName].Deleting.Insert(ncName)
	n.nodePoolNameToNodeClaimState[npName].Active.Delete(ncName)
}

// Cleans up the NodeClaim in NodePoolState and NodePool keys if NodePool is deleted or its sized down to 0
func (n *NodePoolState) Cleanup(ncName string) {
	n.mu.Lock()
	defer n.mu.Unlock()

	npName := n.nodeClaimNameToNodePoolName[ncName]

	if npState, exists := n.nodePoolNameToNodeClaimState[npName]; exists {
		npState.Deleting.Delete(ncName)
		npState.Active.Delete(ncName)

		if npState.Active.Len() == 0 && npState.Deleting.Len() == 0 {
			delete(n.nodePoolNameToNodeClaimState, npName)
			delete(n.nodePoolNameToNodePoolLimit, npName)
		}
	}

	delete(n.nodeClaimNameToNodePoolName, ncName)
}

func (n *NodePoolState) IsNodeClaimRunning(npName, ncName string) bool {
	n.mu.RLock()
	defer n.mu.RUnlock()

	return n.nodePoolNameToNodeClaimState[npName].Active.Has(ncName)
}

// Returns the current NodeClaims for a NodePool by its state (running, deleting)
func (n *NodePoolState) GetNodeCount(npName string) (running, deleting int) {
	n.mu.RLock()
	defer n.mu.RUnlock()

	return n.nodeCounts(npName)
}

// ReserveNodeCount attempts to reserve nodes against a NodePool's limit.
// It ensures that the total of running nodes + deleting nodes + reserved nodes doesn't exceed the limit.
func (n *NodePoolState) ReserveNodeCount(np string, limit int64, wantedLimit int64) int64 {
	n.mu.Lock()
	defer n.mu.Unlock()

	n.ensureNodePoolEntry(np)
	running, deleting := n.nodeCounts(np)

	// We retry until CompareAndSwap is successful
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

// ReleaseNodeCount releases the NodePoolTracker ReservedNodeLimit
func (n *NodePoolState) ReleaseNodeCount(npName string, count int64) {
	n.mu.Lock()
	defer n.mu.Unlock()

	// We retry until CompareAndSwap is successful
	for {
		currentlyReserved := n.nodePoolNameToNodePoolLimit[npName].ReservedNodeLimit.Load()
		if n.nodePoolNameToNodePoolLimit[npName].ReservedNodeLimit.CompareAndSwap(
			currentlyReserved,
			lo.Ternary(currentlyReserved-count < 0, 0, currentlyReserved-count)) {
			return
		}
	}
}

// Methods that expect the caller to hold the lock

func (n *NodePoolState) nodeCounts(npName string) (running, deleting int) {
	if st, ok := n.nodePoolNameToNodeClaimState[npName]; ok {
		return len(st.Active), len(st.Deleting)
	}
	return 0, 0
}

// Updates the NodeClaim state and releases the Limit if NodeClaim transitions from not Running to Running
func (n *NodePoolState) UpdateNodeClaim(nodeClaim *v1.NodeClaim, markedForDeletion bool) {
	// If we are launching/deleting a NodeClaim we need to track the state of the NodeClaim and its limits
	npName := nodeClaim.Labels[v1.NodePoolLabelKey]
	n.SetNodeClaimMapping(npName, nodeClaim.Name)

	// If our node/nodeclaim is marked for deletion, we need to make sure that we delete it
	if markedForDeletion {
		n.MarkNodeClaimDeleting(npName, nodeClaim.Name)
	} else {
		n.MarkNodeClaimRunning(npName, nodeClaim.Name)
	}
}

func (n *NodePoolState) ensureNodePoolEntry(np string) {
	if _, ok := n.nodePoolNameToNodeClaimState[np]; !ok {
		n.nodePoolNameToNodeClaimState[np] = NodeClaimState{
			Active:   sets.New[string](),
			Deleting: sets.New[string](),
		}
	}
	if _, ok := n.nodePoolNameToNodePoolLimit[np]; !ok {
		nl := &NodePooltracker{}
		n.nodePoolNameToNodePoolLimit[np] = nl
	}
}
