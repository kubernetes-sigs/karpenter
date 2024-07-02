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

package lifecycle

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/metrics"
)

type Liveness struct {
	clock      clock.Clock
	kubeClient client.Client
}

// registrationTTL is a heuristic time that we expect the node to register within
// If we don't see the node within this time, then we should delete the NodeClaim and try again
const registrationTTL = time.Minute * 15

func (l *Liveness) Reconcile(ctx context.Context, nodeClaim *v1.NodeClaim) (reconcile.Result, error) {
	registered := nodeClaim.StatusConditions().Get(v1.ConditionTypeRegistered)
	if registered.IsTrue() {
		return reconcile.Result{}, nil
	}
	if registered == nil {
		return reconcile.Result{Requeue: true}, nil
	}
	// If the Registered statusCondition hasn't gone True during the TTL since we first updated it, we should terminate the NodeClaim
	if l.clock.Since(registered.LastTransitionTime.Time) < registrationTTL {
		return reconcile.Result{RequeueAfter: registrationTTL - l.clock.Since(registered.LastTransitionTime.Time)}, nil
	}
	// Delete the NodeClaim if we believe the NodeClaim won't register since we haven't seen the node
	if err := l.kubeClient.Delete(ctx, nodeClaim); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	log.FromContext(ctx).V(1).WithValues("ttl", registrationTTL).Info("terminating due to registration ttl")
	metrics.NodeClaimsTerminatedCounter.With(prometheus.Labels{
		metrics.ReasonLabel:       "liveness",
		metrics.NodePoolLabel:     nodeClaim.Labels[v1.NodePoolLabelKey],
		metrics.CapacityTypeLabel: metrics.GetLabelOrDefault(nodeClaim.Labels, v1.CapacityTypeLabelKey),
	}).Inc()

	return reconcile.Result{}, nil
}
