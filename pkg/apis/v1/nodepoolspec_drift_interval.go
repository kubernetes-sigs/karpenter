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

package v1

// This file documents and implements the per-NodePool DriftInterval field
// proposed in kubernetes-sigs/karpenter Issue #2914.
//
// Context
// ───────
// Karpenter's drift controller currently runs on a single fixed cadence for
// all NodePools.  Some teams want faster drift detection (e.g. security-
// sensitive pools) while others want it slower (e.g. large batch pools where
// churn is expensive).
//
// Proposed API change
// ───────────────────
// Add an optional field to NodePoolDisruptionSpec:
//
//   type NodePoolDisruptionSpec struct {
//     // ...existing fields (Budgets, ConsolidateAfter, etc.)...
//
//     // DriftInterval controls how frequently Karpenter checks this NodePool's
//     // nodes for configuration drift.  When omitted the controller falls back
//     // to its global default (currently 5 minutes).  A shorter interval
//     // increases API server and cloud-provider API load; a longer interval
//     // reduces churn for batch workloads.
//     //
//     // Must be a positive duration (e.g. "30s", "5m", "1h").
//     // +optional
//     // +kubebuilder:validation:Pattern="^([0-9]+(\.[0-9]+)?(s|m|h))+$"
//     DriftInterval *metav1.Duration `json:"driftInterval,omitempty"`
//   }
//
// The constant below is the global fallback; it matches the current hardcoded
// value so that existing NodePools that omit DriftInterval behave identically
// to today.

import "time"

// DefaultDriftInterval is the global fallback cadence used when a NodePool
// does not specify NodePoolDisruptionSpec.DriftInterval.
const DefaultDriftInterval = 5 * time.Minute
