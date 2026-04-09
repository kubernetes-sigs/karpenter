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

// Package disruption – drift_interval.go
//
// This file extends the Drift disruptor (drift.go) to support per-NodePool
// configurable drift detection cadence as proposed in Issue #2914.
//
// How it plugs in
// ───────────────
// The core reconcile loop in controller.go calls Drift.ComputeCommands() each
// cycle.  After computing commands for a batch of nodes, it requeues the work
// item with a delay equal to the global default.
//
// With per-NodePool intervals the reconciler needs to:
//  1. After processing a NodePool, read nodePool.Spec.Disruption.DriftInterval.
//  2. If set, requeue that NodePool after that interval instead of the global
//     default.
//  3. If a NodePool's interval is shorter than the global default, ensure its
//     item fires sooner.
//
// Because Karpenter's controller requeues via ctrl.Result{RequeueAfter: d},
// the change is localised to the place where the disruption loop returns its
// result.
//
// The helper below is provided for the reconciler to call.

package disruption

import (
	"time"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

// DriftIntervalFor returns the drift detection re-queue interval for the
// supplied NodePool.  When the NodePool specifies a DriftInterval in its
// disruption spec that value is used; otherwise DefaultDriftInterval is
// returned so that existing behaviour is preserved.
func DriftIntervalFor(nodePool *v1.NodePool) time.Duration {
	if nodePool == nil {
		return v1.DefaultDriftInterval
	}
	disruption := nodePool.Spec.Disruption
	if disruption.DriftInterval != nil && disruption.DriftInterval.Duration > 0 {
		return disruption.DriftInterval.Duration
	}
	return v1.DefaultDriftInterval
}

// ─────────────────────────────────────────────────────────────────────────────
// Integration guidance (to be applied to drift.go / controller.go)
// ─────────────────────────────────────────────────────────────────────────────
//
// In the outer reconciler loop (controller.go) where the requeue delay is
// computed, replace the global constant with a call to DriftIntervalFor:
//
//   // Before:
//   return ctrl.Result{RequeueAfter: time.Minute * 5}, nil
//
//   // After:
//   interval := disruption.DriftIntervalFor(nodePool)
//   return ctrl.Result{RequeueAfter: interval}, nil
//
// This is the minimal, backwards-compatible change.  For NodePools that omit
// DriftInterval the behaviour is identical to today.
//
// ─────────────────────────────────────────────────────────────────────────────
// Validation webhook addition (nodepool_validation.go)
// ─────────────────────────────────────────────────────────────────────────────
//
// Add to ValidateDisruption():
//
//   if spec.Disruption.DriftInterval != nil {
//       if spec.Disruption.DriftInterval.Duration <= 0 {
//           allErrs = append(allErrs, field.Invalid(
//               fldPath.Child("driftInterval"), spec.Disruption.DriftInterval,
//               "driftInterval must be a positive duration"))
//       }
//       if spec.Disruption.DriftInterval.Duration < 30*time.Second {
//           allErrs = append(allErrs, field.Invalid(
//               fldPath.Child("driftInterval"), spec.Disruption.DriftInterval,
//               "driftInterval must be at least 30s to avoid excessive API load"))
//       }
//   }
