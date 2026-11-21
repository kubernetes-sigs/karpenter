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
	"time"

	"github.com/patrickmn/go-cache"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

// TTL bounds how long an entry lives without being refreshed. Entries are normally removed once the launch result is
// observed as persisted or the NodeClaim is finalized. Launch refreshes the entry on every reconcile, so a NodeClaim
// whose status write keeps failing stays registered.
const TTL = time.Hour

// Registry maps NodeClaim UID to the NodeClaim returned by CloudProvider.Create. It is safe for concurrent use.
type Registry struct {
	cache *cache.Cache
}

func NewRegistry() *Registry {
	return &Registry{cache: cache.New(TTL, time.Minute)}
}

// Add records (or refreshes) the result of CloudProvider.Create for the NodeClaim with the given UID.
func (r *Registry) Add(uid types.UID, created *v1.NodeClaim) {
	r.cache.SetDefault(string(uid), created)
}

// Get returns the launch result for the NodeClaim with the given UID.
func (r *Registry) Get(uid types.UID) (*v1.NodeClaim, bool) {
	if ret, ok := r.cache.Get(string(uid)); ok {
		return ret.(*v1.NodeClaim), true
	}
	return nil, false
}

// Delete removes the entry for the NodeClaim with the given UID.
func (r *Registry) Delete(uid types.UID) {
	r.cache.Delete(string(uid))
}

// ProviderIDs returns the providerIDs of every launch whose result has not yet been observed as persisted. A nil
// Registry holds nothing.
func (r *Registry) ProviderIDs() sets.Set[string] {
	ids := sets.New[string]()
	if r == nil {
		return ids
	}
	for _, item := range r.cache.Items() {
		if providerID := item.Object.(*v1.NodeClaim).Status.ProviderID; providerID != "" {
			ids.Insert(providerID)
		}
	}
	return ids
}
