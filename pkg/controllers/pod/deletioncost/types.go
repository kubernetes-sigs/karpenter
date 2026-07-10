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
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/labels"

	"sigs.k8s.io/karpenter/pkg/controllers/state"
)

// NodeRank pairs a state node with the rank value the controller plans to
// write to its pods. HasDoNotDisrupt indicates Group D, where the controller
// clears the annotation rather than writing rank. Pods carries the pod list
// captured during RankNodes so UpdatePodDeletionCosts does not have to re-list
// pods from the apiserver.
//
// Rank is only meaningful when HasDoNotDisrupt=false; Group D entries leave
// Rank at its zero value and UpdatePodDeletionCosts ignores it. Group D nodes
// are excluded from the sequential-rank walk so the range of annotated ranks
// is contiguous.
type NodeRank struct {
	Node            *state.StateNode
	Rank            int
	HasDoNotDisrupt bool
	Pods            []*corev1.Pod
}

// parsedPDB pairs a PDB with its pre-parsed selector. We pre-parse once per
// reconcile so the per-pod inner loop in hasPDBBlockedPods is allocation-free.
type parsedPDB struct {
	pdb      *policyv1.PodDisruptionBudget
	selector labels.Selector
}
