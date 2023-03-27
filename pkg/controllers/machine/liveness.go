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

package machine

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/utils/clock"
	"knative.dev/pkg/logging"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/metrics"
)

type Liveness struct {
	clock      clock.Clock
	kubeClient client.Client
}

// registrationTTL is a heuristic time that we expect the node to register within
// If we don't see the node within this time, then we should delete the machine and try again
const registrationTTL = time.Minute * 15

func (l *Liveness) Reconcile(ctx context.Context, machine *v1alpha5.Machine) (reconcile.Result, error) {
	if machine.StatusConditions().GetCondition(v1alpha5.MachineRegistered).IsTrue() {
		return reconcile.Result{}, nil
	}
	if machine.CreationTimestamp.IsZero() || l.clock.Since(machine.CreationTimestamp.Time) < registrationTTL {
		return reconcile.Result{RequeueAfter: registrationTTL - l.clock.Since(machine.CreationTimestamp.Time)}, nil
	}
	// Delete the machine if we believe the machine won't register since we haven't seen the node
	if err := l.kubeClient.Delete(ctx, machine); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	logging.FromContext(ctx).With("ttl", registrationTTL).Debugf("terminating machine since node hasn't registered within registration ttl")
	metrics.MachinesTerminatedCounter.With(prometheus.Labels{
		metrics.ReasonLabel:      "registration_timeout",
		metrics.ProvisionerLabel: machine.Labels[v1alpha5.ProvisionerNameLabelKey],
	})
	return reconcile.Result{}, nil
}
