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

package provisioning

import (
	"context"
	"time"

	"sigs.k8s.io/karpenter/pkg/operator/options"
)

// Batcher separates a stream of Trigger() calls into windowed slices. The
// window is dynamic and will be extended if additional items are added up to a
// maximum batch duration.
type Batcher struct {
	trigger chan struct{}
}

// NewBatcher is a constructor for the Batcher
func NewBatcher() *Batcher {
	return &Batcher{
		trigger: make(chan struct{}, 1),
	}
}

// Trigger causes the batcher to start a batching window, or extend the current batching window if it hasn't reached the
// maximum length.
func (b *Batcher) Trigger() {
	// The trigger is idempotently armed. This statement never blocks
	select {
	case b.trigger <- struct{}{}:
	default:
	}
}

// Wait starts a batching window and continues waiting as long as it continues receiving triggers within
// the idleDuration, up to the maxDuration
func (b *Batcher) Wait(ctx context.Context) bool {
	select {
	case <-b.trigger:
		// start the batching window after the first item is received
	case <-time.After(1 * time.Second):
		// If no pods, bail to the outer controller framework to refresh the context
		return false
	}
	timeout := time.NewTimer(options.FromContext(ctx).BatchMaxDuration)
	idle := time.NewTimer(options.FromContext(ctx).BatchIdleDuration)
	for {
		select {
		case <-b.trigger:
			// correct way to reset an active timer per docs
			if !idle.Stop() {
				<-idle.C
			}
			idle.Reset(options.FromContext(ctx).BatchIdleDuration)
		case <-timeout.C:
			return true
		case <-idle.C:
			return true
		}
	}
}
