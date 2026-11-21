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

// Package launched holds CloudProvider.Create results until they are observed as persisted on the NodeClaim.
package launched

import (
	"time"

	"github.com/patrickmn/go-cache"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

type Registry struct {
	cache *cache.Cache
}

func NewRegistry() *Registry {
	return &Registry{cache: cache.New(time.Hour, time.Minute)}
}

func (r *Registry) Add(uid types.UID, created *v1.NodeClaim) {
	r.cache.SetDefault(string(uid), created)
}

func (r *Registry) Get(uid types.UID) (*v1.NodeClaim, bool) {
	if ret, ok := r.cache.Get(string(uid)); ok {
		return ret.(*v1.NodeClaim), true
	}
	return nil, false
}

func (r *Registry) Delete(uid types.UID) {
	r.cache.Delete(string(uid))
}

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
