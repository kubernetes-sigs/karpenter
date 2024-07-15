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

package sharedcache

import (
	"sync"
	"time"

	"github.com/patrickmn/go-cache"
)

var sharedCache *cache.Cache
var once sync.Once

const DefaultSharedCacheTTL time.Duration = time.Hour * 24

// SharedCache provides a global point of access to the Cache instance
func SharedCache() *cache.Cache {
	once.Do(func() {
		sharedCache = cache.New(DefaultSharedCacheTTL, time.Hour)
	})
	return sharedCache
}
