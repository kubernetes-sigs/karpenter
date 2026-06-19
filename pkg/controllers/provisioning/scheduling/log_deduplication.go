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

package scheduling

import (
	"time"

	gocache "github.com/patrickmn/go-cache"
	corev1 "k8s.io/api/core/v1"
)

const podSchedulingErrorLogTimeout = 5 * time.Minute

// PodErrorCache suppresses repeated "could not schedule pod" log lines for the same pod across
// scheduling cycles. It must be held on a long-lived struct (e.g. Provisioner) so the
// deduplication window survives across reconcile loops.
type PodErrorCache struct {
	cache *gocache.Cache
}

func NewPodErrorCache() *PodErrorCache {
	return &PodErrorCache{
		cache: gocache.New(podSchedulingErrorLogTimeout, 2*podSchedulingErrorLogTimeout),
	}
}

// ShouldLog returns true the first time a given pod is seen, and false until
// podSchedulingErrorLogTimeout elapses.
func (c *PodErrorCache) ShouldLog(p *corev1.Pod) bool {
	if c == nil {
		return true
	}
	key := string(p.UID)
	if _, found := c.cache.Get(key); found {
		return false
	}
	c.cache.Set(key, nil, gocache.DefaultExpiration)
	return true
}
