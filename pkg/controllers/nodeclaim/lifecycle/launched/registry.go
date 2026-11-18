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

// Package launched tracks NodeClaims that the CloudProvider has launched but whose launch result has not yet been
// observed as persisted on the NodeClaim. Status writes can lag a successful Create by a long time (API server
// throttling, conflicts, informer lag), and during that window the NodeClaim in the informer cache has no providerID.
// The Registry lets every component in the leader process (launch, finalization, provider garbage collection) see the
// launch result in the meantime. It only ever holds providerIDs returned by CloudProvider.Create.
//
// The Registry is in-memory, so it does not survive a restart or a leader change.
package launched

import (
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/clock"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

// TTL bounds how long an entry lives without being refreshed. Entries are normally removed explicitly once the launch
// result is observed as persisted or the NodeClaim is finalized; the TTL only stops a NodeClaim that disappears without
// either from pinning its entry. Launch refreshes the entry on every reconcile, so a NodeClaim whose status write keeps
// failing stays registered.
const TTL = time.Hour

type entry struct {
	created   *v1.NodeClaim
	expiresAt time.Time
}

// Registry is safe for concurrent use.
type Registry struct {
	clock clock.Clock

	mu           sync.RWMutex
	byUID        map[types.UID]*entry
	byProviderID map[string]types.UID
}

func NewRegistry(clk clock.Clock) *Registry {
	return &Registry{
		clock:        clk,
		byUID:        map[types.UID]*entry{},
		byProviderID: map[string]types.UID{},
	}
}

// Add records (or refreshes) the result of CloudProvider.Create for the NodeClaim with the given UID.
func (r *Registry) Add(uid types.UID, created *v1.NodeClaim) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.byUID[uid]; ok && e.created.Status.ProviderID != created.Status.ProviderID {
		delete(r.byProviderID, e.created.Status.ProviderID)
	}
	r.byUID[uid] = &entry{created: created, expiresAt: r.clock.Now().Add(TTL)}
	if created.Status.ProviderID != "" {
		r.byProviderID[created.Status.ProviderID] = uid
	}
	r.sweepLocked()
}

// Get returns the launch result for the NodeClaim with the given UID.
func (r *Registry) Get(uid types.UID) (*v1.NodeClaim, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.byUID[uid]
	if !ok || r.expired(e) {
		return nil, false
	}
	return e.created, true
}

// ProviderID returns the providerID launched for the NodeClaim with the given UID, or "" if there is none.
func (r *Registry) ProviderID(uid types.UID) string {
	if created, ok := r.Get(uid); ok {
		return created.Status.ProviderID
	}
	return ""
}

// HasProviderID reports whether providerID was launched for a NodeClaim whose launch result has not yet been observed
// as persisted. A nil Registry holds nothing.
func (r *Registry) HasProviderID(providerID string) bool {
	if r == nil || providerID == "" {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	uid, ok := r.byProviderID[providerID]
	if !ok {
		return false
	}
	e, ok := r.byUID[uid]
	return ok && !r.expired(e)
}

// Delete removes the entry for the NodeClaim with the given UID.
func (r *Registry) Delete(uid types.UID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deleteLocked(uid)
}

// Len returns the number of entries, including any that have expired but not yet been swept.
func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.byUID)
}

func (r *Registry) expired(e *entry) bool {
	return !r.clock.Now().Before(e.expiresAt)
}

func (r *Registry) deleteLocked(uid types.UID) {
	if e, ok := r.byUID[uid]; ok {
		if r.byProviderID[e.created.Status.ProviderID] == uid {
			delete(r.byProviderID, e.created.Status.ProviderID)
		}
		delete(r.byUID, uid)
	}
}

// sweepLocked drops expired entries. The registry only holds in-flight launches, so it stays small and a full scan on
// insert is cheap.
func (r *Registry) sweepLocked() {
	for uid, e := range r.byUID {
		if r.expired(e) {
			r.deleteLocked(uid)
		}
	}
}
