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

package expiration

import (
	"context"

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/utils/clock"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/metrics"
)

// Expiration is a nodeclaim controller that deletes expired nodeclaims based on expireAfter
type Controller struct {
	clock      clock.Clock
	kubeClient client.Client
}

// NewController constructs a nodeclaim disruption controller
func NewController(clk clock.Clock, kubeClient client.Client) *Controller {
	return &Controller{
		kubeClient: kubeClient,
		clock:      clk,
	}
}

func (c *Controller) Reconcile(ctx context.Context, nodeClaim *v1.NodeClaim) (reconcile.Result, error) {
	// From here there are three scenarios to handle:
	// 1. If ExpireAfter is not configured, exit expiration loop
	if nodeClaim.Spec.ExpireAfter.Duration == nil {
		return reconcile.Result{}, nil
	}
	expirationTime := nodeClaim.CreationTimestamp.Add(*nodeClaim.Spec.ExpireAfter.Duration)
	// 2. If the NodeClaim isn't expired leave the reconcile loop.
	if c.clock.Now().Before(expirationTime) {
		// Use t.Sub(clock.Now()) instead of time.Until() to ensure we're using the injected clock.
		return reconcile.Result{RequeueAfter: expirationTime.Sub(c.clock.Now())}, nil
	}
	// 3. Otherwise, if the NodeClaim is expired we can forcefully expire the nodeclaim (by deleting it)
	if err := c.kubeClient.Delete(ctx, nodeClaim); err != nil {
		return reconcile.Result{}, err
	}
	// 4. The deletion timestamp has successfully been set for the NodeClaim, update relevant metrics.
	log.FromContext(ctx).V(1).Info("deleting expired nodeclaim")
	metrics.NodeClaimsDisruptedCounter.With(prometheus.Labels{
		metrics.TypeLabel:     metrics.ExpirationReason,
		metrics.NodePoolLabel: metrics.GetLabelOrDefault(nodeClaim.Labels, v1.NodePoolLabelKey),
	}).Inc()
	metrics.NodeClaimsTerminatedCounter.With(prometheus.Labels{
		metrics.ReasonLabel:       metrics.ExpirationReason,
		metrics.NodePoolLabel:     metrics.GetLabelOrDefault(nodeClaim.Labels, v1.NodePoolLabelKey),
		metrics.CapacityTypeLabel: metrics.GetLabelOrDefault(nodeClaim.Labels, v1.CapacityTypeLabelKey),
	}).Inc()
	return reconcile.Result{}, nil
}

func (c *Controller) Register(_ context.Context, m manager.Manager) error {
	builder := controllerruntime.NewControllerManagedBy(m)
	return builder.
		Named("nodeclaim.expiration").
		For(&v1.NodeClaim{}).
		Complete(reconcile.AsReconciler(m.GetClient(), c))
}
