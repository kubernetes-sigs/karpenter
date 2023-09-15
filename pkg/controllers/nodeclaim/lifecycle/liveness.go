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

package lifecycle

import (
	"context"
	"time"

	"k8s.io/utils/clock"
	"knative.dev/pkg/logging"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/aws/karpenter-core/pkg/apis/v1beta1"
	nodeclaimutil "github.com/aws/karpenter-core/pkg/utils/nodeclaim"
)

type Liveness struct {
	clock      clock.Clock
	kubeClient client.Client
}

// registrationTTL is a heuristic time that we expect the node to register within
// If we don't see the node within this time, then we should delete the NodeClaim and try again
const registrationTTL = time.Minute * 15

func (l *Liveness) Reconcile(ctx context.Context, nodeClaim *v1beta1.NodeClaim) (reconcile.Result, error) {
	registered := nodeClaim.StatusConditions().GetCondition(v1beta1.Registered)
	if registered.IsTrue() {
		return reconcile.Result{}, nil
	}
	if registered == nil {
		return reconcile.Result{Requeue: true}, nil
	}
	// If the Registered statusCondition hasn't gone True during the TTL since we first updated it, we should terminate the NodeClaim
	if l.clock.Since(registered.LastTransitionTime.Inner.Time) < registrationTTL {
		return reconcile.Result{RequeueAfter: registrationTTL - l.clock.Since(registered.LastTransitionTime.Inner.Time)}, nil
	}
	// Delete the NodeClaim if we believe the NodeClaim won't register since we haven't seen the node
	if err := nodeclaimutil.Delete(ctx, l.kubeClient, nodeClaim); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	logging.FromContext(ctx).With("ttl", registrationTTL).Debugf("terminating due to registration ttl")
	nodeclaimutil.TerminatedCounter(nodeClaim, "liveness").Inc()

	return reconcile.Result{}, nil
}
