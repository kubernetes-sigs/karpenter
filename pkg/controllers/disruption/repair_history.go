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

package disruption

import (
	"sync"
	"time"

	"github.com/patrickmn/go-cache"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/clock"

	"sigs.k8s.io/karpenter/pkg/cloudprovider"
)

const (
	rebootHistoryWindow          = 24 * time.Hour
	rebootHistoryCleanupInterval = time.Hour
	rebootsBeforeReplacement     = 2
)

type rebootHistoryEntry struct {
	committedAt [rebootsBeforeReplacement]time.Time
	count       int
}

// RebootHistory counts up to rebootsBeforeReplacement committed reboots in a sliding window by NodeClaim UID.
type RebootHistory struct {
	mu     sync.Mutex
	clock  clock.Clock
	recent *cache.Cache
}

func NewRebootHistory() *RebootHistory {
	return newRebootHistory(clock.RealClock{})
}

func newRebootHistory(clk clock.Clock) *RebootHistory {
	return &RebootHistory{
		clock:  clk,
		recent: cache.New(rebootHistoryWindow, rebootHistoryCleanupInterval),
	}
}

// RecordCommittedReboot consumes one reboot attempt after a new lifecycle handoff commits.
// Callers must record each committed handoff exactly once; retries that observe an active lifecycle must not record it
// again.
func (h *RebootHistory) RecordCommittedReboot(nodeClaimUID types.UID) {
	h.mu.Lock()
	defer h.mu.Unlock()

	key := string(nodeClaimUID)
	now := h.clock.Now()
	entry := h.recentReboots(nodeClaimUID, now)
	if entry.count >= rebootsBeforeReplacement {
		return
	}
	entry.committedAt[entry.count] = now
	entry.count++
	h.recent.SetDefault(key, entry)
}

// Resolve applies recent reboot history to the candidate's current policy decision, escalating a reboot to replacement
// once the NodeClaim has exhausted its reboot attempts. It returns whether the candidate still has a repair action.
// Only the action changes: a reboot decision means no replacement policy is eligible, so the governing condition is
// unchanged by the escalation.
func (h *RebootHistory) Resolve(candidate *Candidate) bool {
	candidate.RebootEscalated = false
	if candidate.RepairPolicyResult.Action == "" {
		return false
	}
	if candidate.RepairPolicyResult.Action == cloudprovider.RebootNode &&
		h.recentReboots(candidate.NodeClaim.UID, h.clock.Now()).count >= rebootsBeforeReplacement {
		candidate.RepairPolicyResult.Action = cloudprovider.ReplaceNode
		candidate.RebootEscalated = true
	}
	return true
}

func (h *RebootHistory) recentReboots(nodeClaimUID types.UID, now time.Time) rebootHistoryEntry {
	value, ok := h.recent.Get(string(nodeClaimUID))
	if !ok {
		return rebootHistoryEntry{}
	}
	entry, _ := value.(rebootHistoryEntry)
	cutoff := now.Add(-rebootHistoryWindow)
	recent := rebootHistoryEntry{}
	for i := range entry.count {
		if entry.committedAt[i].After(cutoff) {
			recent.committedAt[recent.count] = entry.committedAt[i]
			recent.count++
		}
	}
	return recent
}
