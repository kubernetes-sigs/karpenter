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

package controller

import (
	"fmt"
	"math"
	"time"

	"github.com/awslabs/operatorpkg/serrors"
	"golang.org/x/time/rate"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// RetryConfig configures failed-reconciliation retries, not all reconciliations or
// cloud API calls. Zero-valued fields retain the controller's defaults.
type RetryConfig struct {
	// InitialDelay is the per-resource delay after the first failure.
	InitialDelay time.Duration
	// MaxDelay caps the per-resource exponential backoff, not the combined limiter's delay.
	MaxDelay time.Duration
	// QPS limits sustained retry scheduling across the controller's resources.
	QPS float64
	// Burst is the token bucket's capacity for retry scheduling.
	Burst int
}

// NewRateLimiter applies overrides to defaults and constructs a fresh limiter for
// one controller. Defaults must supply positive values for every field.
func (c RetryConfig) NewRateLimiter(defaults RetryConfig) (workqueue.TypedRateLimiter[reconcile.Request], error) {
	if c.InitialDelay == 0 {
		c.InitialDelay = defaults.InitialDelay
	}
	if c.MaxDelay == 0 {
		c.MaxDelay = defaults.MaxDelay
	}
	if c.QPS == 0 {
		c.QPS = defaults.QPS
	}
	if c.Burst == 0 {
		c.Burst = defaults.Burst
	}
	if c.InitialDelay <= 0 || c.MaxDelay <= 0 || c.InitialDelay > c.MaxDelay {
		return nil, serrors.Wrap(fmt.Errorf("retry delays must be positive and initial delay must not exceed maximum delay"))
	}
	if c.QPS <= 0 || math.IsNaN(c.QPS) || math.IsInf(c.QPS, 0) {
		return nil, serrors.Wrap(fmt.Errorf("retry QPS must be positive and finite"))
	}
	if c.Burst <= 0 {
		return nil, serrors.Wrap(fmt.Errorf("retry burst must be positive"))
	}
	return workqueue.NewTypedMaxOfRateLimiter[reconcile.Request](
		workqueue.NewTypedItemExponentialFailureRateLimiter[reconcile.Request](c.InitialDelay, c.MaxDelay),
		&workqueue.TypedBucketRateLimiter[reconcile.Request]{Limiter: rate.NewLimiter(rate.Limit(c.QPS), c.Burst)},
	), nil
}
