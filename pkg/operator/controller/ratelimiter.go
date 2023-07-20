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

package controller

import "time"

// DisabledRateLimiter is a rate limiter that doesn't backoff when receiving an error
// or Requeue: true from the reconcile loop. This is mainly useful with Singleton controllers
// where we want the new loop to execute immediately without backoff
type DisabledRateLimiter struct{}

func NewDisabledRateLimiter() *DisabledRateLimiter {
	return &DisabledRateLimiter{}
}

func (*DisabledRateLimiter) When(interface{}) time.Duration { return 0 }

func (*DisabledRateLimiter) Forget(interface{}) {}

func (*DisabledRateLimiter) NumRequeues(interface{}) int { return -1 }
