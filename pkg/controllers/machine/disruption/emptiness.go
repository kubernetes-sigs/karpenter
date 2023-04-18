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
	"time"

	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	"k8s.io/utils/clock"
	"knative.dev/pkg/logging"
	"knative.dev/pkg/ptr"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
)

// Emptiness is a node sub-controller that annotates or de-annotates an expired node based on TTLSecondsUntilExpired
type Emptiness struct {
	clock clock.Clock
}

func (e *Emptiness) Reconcile(ctx context.Context, provisioner *v1alpha5.Provisioner, machine *v1alpha5.Machine, node *v1.Node) (reconcile.Result, error) {
	// If the node is marked as voluntarily disrupted by another controller, do nothing.
	voluntarilyDisrupted := machine.StatusConditions().GetCondition(v1alpha5.MachineVoluntarilyDisrupted)
	if voluntarilyDisrupted.IsTrue() && voluntarilyDisrupted.Reason != "Empty" {
		return reconcile.Result{}, nil
	}

	// From here there are three scenarios to handle:
	// 1. If TTLSecondsAfterEmpty is not configured, but the node is empty,
	//    remove the annotation so another disruption controller can annotate the node.
	if provisioner.Spec.TTLSecondsAfterEmpty == nil {
		if voluntarilyDisrupted.IsTrue() {
			_ = machine.StatusConditions().ClearCondition(v1alpha5.MachineVoluntarilyDisrupted)
			delete(node.Annotations, v1alpha5.VoluntaryDisruptionAnnotationKey)
			logging.FromContext(ctx).Debugf("removing emptiness annotation from node as emptiness has been disabled")
		}
		return reconcile.Result{}, nil
	}

	emptinessTimestamp := node.Annotations[v1alpha5.EmptinessTimestampAnnotationKey]
	if emptinessTimestamp == "" {
		_ = machine.StatusConditions().ClearCondition(v1alpha5.MachineVoluntarilyDisrupted)
		delete(node.Annotations, v1alpha5.VoluntaryDisruptionAnnotationKey)
		return reconcile.Result{}, nil
	}
	emptyAt, err := time.Parse(time.RFC3339, emptinessTimestamp)
	if err != nil {
		_ = machine.StatusConditions().ClearCondition(v1alpha5.MachineVoluntarilyDisrupted)
		delete(node.Annotations, v1alpha5.VoluntaryDisruptionAnnotationKey)
		logging.FromContext(ctx).With("emptiness-timestamp", emptinessTimestamp).Errorf("unable to parse emptiness timestamp")
		return reconcile.Result{}, nil
	}
	emptinessTTLTime := emptyAt.Add(time.Duration(ptr.Int64Value(provisioner.Spec.TTLSecondsAfterEmpty)) * time.Second)

	// 2. Otherwise, if the node is expired, but doesn't have the annotation, add it.
	if e.clock.Now().After(emptinessTTLTime) && !voluntarilyDisrupted.IsTrue() {
		machine.StatusConditions().MarkTrueWithReason(v1alpha5.MachineVoluntarilyDisrupted, "Empty", "")
		node.Annotations = lo.Assign(node.Annotations, map[string]string{
			v1alpha5.VoluntaryDisruptionAnnotationKey: v1alpha5.VoluntaryDisruptionEmptyAnnotationValue,
		})
		logging.FromContext(ctx).Debugf("annotating node as empty")
		return reconcile.Result{}, nil
	}
	// 3. Finally, if the node isn't expired, but has the annotation, remove it.
	if !e.clock.Now().After(emptinessTTLTime) && voluntarilyDisrupted.IsTrue() {
		_ = machine.StatusConditions().ClearCondition(v1alpha5.MachineVoluntarilyDisrupted)
		delete(node.Annotations, v1alpha5.VoluntaryDisruptionAnnotationKey)
		logging.FromContext(ctx).Debugf("removing empty annotation from node")
	}
	return reconcile.Result{RequeueAfter: emptinessTTLTime.Sub(e.clock.Now())}, nil
}
