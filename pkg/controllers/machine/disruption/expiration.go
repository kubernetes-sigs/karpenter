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

package disruption

import (
	"context"

	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	"k8s.io/utils/clock"
	"knative.dev/pkg/logging"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/utils/functional"
)

// Expiration is a node sub-controller that annotates or de-annotates an expired node based on TTLSecondsUntilExpired
type Expiration struct {
	clock clock.Clock
}

func (e *Expiration) Reconcile(ctx context.Context, provisioner *v1alpha5.Provisioner, machine *v1alpha5.Machine, node *v1.Node) (reconcile.Result, error) {
	// If the node is marked as voluntarily disrupted by another controller, do nothing.
	voluntarilyDisrupted := machine.StatusConditions().GetCondition(v1alpha5.MachineVoluntarilyDisrupted)
	if voluntarilyDisrupted.IsTrue() && voluntarilyDisrupted.Reason != "Expired" {
		return reconcile.Result{}, nil
	}

	// From here there are three scenarios to handle:
	// 1. If TTLSecondsUntilExpired is not configured, but the node is expired,
	//    remove the annotation so another disruption controller can annotate the node.
	if provisioner.Spec.TTLSecondsUntilExpired == nil {
		if voluntarilyDisrupted.IsTrue() {
			_ = machine.StatusConditions().ClearCondition(v1alpha5.MachineVoluntarilyDisrupted)
			delete(node.Annotations, v1alpha5.VoluntaryDisruptionAnnotationKey)
			logging.FromContext(ctx).Debugf("removing expiration annotation from node as expiration has been disabled")
		}
		return reconcile.Result{}, nil
	}

	// 2. Otherwise, if the node is expired, but doesn't have the annotation, add it.
	expired := functional.IsExpired(machine, e.clock, provisioner)
	if expired && !voluntarilyDisrupted.IsTrue() {
		machine.StatusConditions().MarkTrueWithReason(v1alpha5.MachineVoluntarilyDisrupted, "Expired", "")
		node.Annotations = lo.Assign(node.Annotations, map[string]string{
			v1alpha5.VoluntaryDisruptionAnnotationKey: v1alpha5.VoluntaryDisruptionExpiredAnnotationValue,
		})
		logging.FromContext(ctx).Debugf("annotating node as expired")
		return reconcile.Result{}, nil
	}
	// 3. Finally, if the node isn't expired, but has the annotation, remove it.
	if !expired && voluntarilyDisrupted.IsTrue() {
		_ = machine.StatusConditions().ClearCondition(v1alpha5.MachineVoluntarilyDisrupted)
		delete(node.Annotations, v1alpha5.VoluntaryDisruptionAnnotationKey)
		logging.FromContext(ctx).Debugf("removing expiration annotation from node")
	}

	// If the node isn't expired and doesn't have annotation, return.
	// Use t.Sub(time.Now()) instead of time.Until() to ensure we're using the injected clock.
	return reconcile.Result{RequeueAfter: functional.GetExpirationTime(machine, provisioner).Sub(e.clock.Now())}, nil
}
