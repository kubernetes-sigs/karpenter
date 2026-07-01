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

package deletioncost

import (
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/labels"

	"sigs.k8s.io/karpenter/pkg/controllers/state"
)

// NodeRank pairs a state node with the rank value the controller plans to
// write to its pods. HasDoNotDisrupt indicates Group D, where the controller
// clears the annotation rather than writing rank.
type NodeRank struct {
	Node            *state.StateNode
	Rank            int
	HasDoNotDisrupt bool
}

// parsedPDB pairs a PDB with its pre-parsed selector. We pre-parse once per
// reconcile so the per-pod inner loop in hasPDBBlockedPods is allocation-free.
type parsedPDB struct {
	pdb      *policyv1.PodDisruptionBudget
	selector labels.Selector
}
