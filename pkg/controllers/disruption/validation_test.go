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

package disruption_test

import (
	"context"
	"time"

	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/controllers/disruption"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
)

type TestEmptinessValidator struct {
	blocked   bool
	churn     bool
	nominated bool
	cluster   *state.Cluster
	nodePool  *v1.NodePool
	emptiness *disruption.EmptinessValidator
}

type TestEmptinessValidatorOption func(*TestEmptinessValidator)

func WithEmptinessChurn() TestEmptinessValidatorOption {
	return func(v *TestEmptinessValidator) {
		v.churn = true
	}
}

func WithEmptinessBlockingBudget() TestEmptinessValidatorOption {
	return func(v *TestEmptinessValidator) {
		v.blocked = true
	}
}

func WithEmptinessNodeNomination() TestEmptinessValidatorOption {
	return func(v *TestEmptinessValidator) {
		v.nominated = true
	}
}

func NewTestEmptinessValidator(cluster *state.Cluster, nodePool *v1.NodePool, e *disruption.EmptinessValidator, opts ...TestEmptinessValidatorOption) disruption.Validator {
	v := &TestEmptinessValidator{
		cluster:   cluster,
		nodePool:  nodePool,
		emptiness: e,
	}
	for _, opt := range opts {
		opt(v)
	}
	return v
}

func (t *TestEmptinessValidator) Validate(ctx context.Context, cmd disruption.Command, _ time.Duration) (disruption.Command, error) {
	var pods []*corev1.Pod
	stateNodes := t.cluster.Nodes()
	nodes := make([]*corev1.Node, len(stateNodes))
	nodeClaims := make([]*v1.NodeClaim, len(stateNodes))
	for i, stateNode := range stateNodes {
		nodes[i] = stateNode.Node
		nodeClaims[i] = stateNode.NodeClaim
	}
	if t.blocked {
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, nodes, nodeClaims)
		t.nodePool.Spec.Disruption.Budgets = []v1.Budget{{
			Nodes: "0%",
		}}
		ExpectApplied(ctx, env.Client, t.nodePool)
	}
	if t.churn {
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, nodes, nodeClaims)
		rs := test.ReplicaSet()
		ExpectApplied(ctx, env.Client, rs)
		// Simulate churn
		pods = test.Pods(1, test.PodOptions{
			ResourceRequirements: corev1.ResourceRequirements{
				Requests: map[corev1.ResourceName]resource.Quantity{
					// 100m * 10 = 1 vCPU. This should be less than the largest node capacity.
					corev1.ResourceCPU: resource.MustParse("100m"),
				},
			},
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
				"app": "test",
			},
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion:         "apps/v1",
						Kind:               "ReplicaSet",
						Name:               rs.Name,
						UID:                rs.UID,
						Controller:         lo.ToPtr(true),
						BlockOwnerDeletion: lo.ToPtr(true),
					},
				}}})
		ExpectApplied(ctx, env.Client, pods[0])
		ExpectManualBinding(ctx, env.Client, pods[0], nodes[0])
		t.cluster.NominateNodeForPod(ctx, nodes[0].Spec.ProviderID)
		Expect(cluster.UpdateNode(ctx, nodes[0])).To(Succeed())
	}
	if t.nominated {
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, nodes, nodeClaims)
		// Simulate churn
		t.cluster.NominateNodeForPod(ctx, nodes[0].Spec.ProviderID)
		Expect(cluster.UpdateNode(ctx, nodes[0])).To(Succeed())
	}
	return t.emptiness.Validate(ctx, cmd, 0)
}

type TestConsolidationValidator struct {
	blocked       bool
	churn         bool
	nominated     bool
	cluster       *state.Cluster
	nodePool      *v1.NodePool
	consolidation *disruption.ConsolidationValidator
}

type TestConsolidationValidatorOption func(*TestConsolidationValidator)

func WithUnderutilizedChurn() TestConsolidationValidatorOption {
	return func(v *TestConsolidationValidator) {
		v.churn = true
	}
}

func WithUnderutilizedBlockingBudget() TestConsolidationValidatorOption {
	return func(v *TestConsolidationValidator) {
		v.blocked = true
	}
}

func WithUnderutilizedNodeNomination() TestConsolidationValidatorOption {
	return func(v *TestConsolidationValidator) {
		v.nominated = true
	}
}

func NewTestConsolidationValidator(cluster *state.Cluster, nodePool *v1.NodePool, c *disruption.ConsolidationValidator, opts ...TestConsolidationValidatorOption) disruption.Validator {
	v := &TestConsolidationValidator{
		cluster:       cluster,
		nodePool:      nodePool,
		consolidation: c,
	}
	for _, opt := range opts {
		opt(v)
	}
	return v
}

func (t *TestConsolidationValidator) Validate(ctx context.Context, cmd disruption.Command, _ time.Duration) (disruption.Command, error) {
	var pods []*corev1.Pod
	stateNodes := t.cluster.Nodes()
	nodes := make([]*corev1.Node, len(stateNodes))
	nodeClaims := make([]*v1.NodeClaim, len(stateNodes))
	for i, stateNode := range stateNodes {
		nodes[i] = stateNode.Node
		nodeClaims[i] = stateNode.NodeClaim
	}
	if t.blocked {
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, nodes, nodeClaims)
		t.nodePool.Spec.Disruption.Budgets = []v1.Budget{{
			Nodes: "0%",
		}}
		ExpectApplied(ctx, env.Client, t.nodePool)
	}
	if t.churn {
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, nodes, nodeClaims)
		rs := test.ReplicaSet()
		ExpectApplied(ctx, env.Client, rs)
		// Simulate churn
		pods = test.Pods(1, test.PodOptions{
			ResourceRequirements: corev1.ResourceRequirements{
				Requests: map[corev1.ResourceName]resource.Quantity{
					// 100m * 10 = 1 vCPU. This should be less than the largest node capacity.
					corev1.ResourceCPU: resource.MustParse("100m"),
				},
			},
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
				"app": "test",
			},
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion:         "apps/v1",
						Kind:               "ReplicaSet",
						Name:               rs.Name,
						UID:                rs.UID,
						Controller:         lo.ToPtr(true),
						BlockOwnerDeletion: lo.ToPtr(true),
					},
				}}})
		ExpectApplied(ctx, env.Client, pods[0])
		ExpectManualBinding(ctx, env.Client, pods[0], nodes[0])
		t.cluster.NominateNodeForPod(ctx, nodes[0].Spec.ProviderID)
		Expect(cluster.UpdateNode(ctx, nodes[0])).To(Succeed())
	}
	if t.nominated {
		ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, nodeStateController, nodeClaimStateController, nodes, nodeClaims)
		// Simulate churn
		t.cluster.NominateNodeForPod(ctx, nodes[0].Spec.ProviderID)
		Expect(cluster.UpdateNode(ctx, nodes[0])).To(Succeed())
	}
	return t.consolidation.Validate(ctx, cmd, 0)
}
