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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/controllers/disruption"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
)

var _ = Describe("Repair Method Registration", func() {
	var enabledCtx context.Context

	BeforeEach(func() {
		enabledCtx = options.ToContext(ctx, test.Options(test.OptionsFields{FeatureGates: test.FeatureGates{NodeRepair: lo.ToPtr(true)}}))
	})

	It("registers repair when the feature gate is enabled and policies are valid", func() {
		cloudProvider.RepairPolicy = []cloudprovider.RepairPolicy{{
			ConditionType:   "BadNode",
			ConditionStatus: corev1.ConditionFalse,
			Action:          cloudprovider.ReplaceNode,
		}}

		methods := disruption.NewMethods(enabledCtx, env.Clock, cluster, env.Client, prov, cloudProvider, recorder, queue)
		Expect(repairMethodCount(methods)).To(Equal(1))
	})

	It("panics when repair is enabled without any policies", func() {
		cloudProvider.RepairPolicy = nil

		Expect(func() {
			disruption.NewMethods(enabledCtx, env.Clock, cluster, env.Client, prov, cloudProvider, recorder, queue)
		}).To(PanicWith("node repair requires the cloud provider to define RepairPolicies, but it defines none"))
	})

	It("panics when the complete policy set is invalid", func() {
		cloudProvider.RepairPolicy = []cloudprovider.RepairPolicy{
			{
				ConditionType:   "BadNode",
				ConditionStatus: corev1.ConditionFalse,
				ReasonRegex:     "[",
				Action:          cloudprovider.ReplaceNode,
			},
			{
				ConditionType:   "BadNode",
				ConditionStatus: corev1.ConditionFalse,
				Action:          cloudprovider.ReplaceNode,
			},
		}

		Expect(func() {
			disruption.NewMethods(enabledCtx, env.Clock, cluster, env.Client, prov, cloudProvider, recorder, queue)
		}).To(PanicWith(ContainSubstring("node repair requires valid RepairPolicies")))
	})

	It("does not register repair when the feature gate is disabled", func() {
		cloudProvider.RepairPolicy = nil
		disabledCtx := options.ToContext(ctx, test.Options(test.OptionsFields{FeatureGates: test.FeatureGates{NodeRepair: lo.ToPtr(false)}}))

		methods := disruption.NewMethods(disabledCtx, env.Clock, cluster, env.Client, prov, cloudProvider, recorder, queue)
		Expect(repairMethodCount(methods)).To(BeZero())
	})
})

func repairMethodCount(methods []disruption.Method) int {
	count := 0
	for _, method := range methods {
		if _, ok := method.(*disruption.Repair); ok {
			count++
		}
	}
	return count
}

func NewMethodsWithRealValidator() []disruption.Method {
	return disruption.NewMethods(ctx, env.Clock, cluster, env.Client, prov, cloudProvider, recorder, queue)
}

type NopValidator struct{}

func (n NopValidator) Validate(_ context.Context, command disruption.Command, _ time.Duration) (disruption.Command, error) {
	return command, nil
}

func NewMethodsWithNopValidator() []disruption.Method {
	c := disruption.MakeConsolidation(env.Clock, cluster, env.Client, prov, cloudProvider, recorder, queue)
	emptiness := disruption.NewEmptiness(c, disruption.WithValidator(NopValidator{}))
	multiNodeConsolidation := disruption.NewMultiNodeConsolidation(c, disruption.WithValidator(NopValidator{}))
	singleNodeConsolidation := disruption.NewSingleNodeConsolidation(c, disruption.WithValidator(NopValidator{}))
	return []disruption.Method{
		disruption.NewStaticDrift(cluster, prov, cloudProvider),
		disruption.NewDrift(env.Client, cluster, prov, recorder, env.Clock),
		emptiness,
		multiNodeConsolidation,
		singleNodeConsolidation,
	}
}

type TestEmptinessValidator struct {
	blocked    bool
	churn      bool
	nominated  bool
	nodes      []*corev1.Node
	nodeClaims []*v1.NodeClaim
	nodePool   *v1.NodePool
	emptiness  *disruption.EmptinessValidator
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

func NewTestEmptinessValidator(nodes []*corev1.Node, nodeClaims []*v1.NodeClaim, nodePool *v1.NodePool, opts ...TestEmptinessValidatorOption) disruption.Validator {
	v := &TestEmptinessValidator{
		nodes:      nodes,
		nodeClaims: nodeClaims,
		nodePool:   nodePool,
		emptiness:  disruption.NewEmptinessValidator(disruption.MakeConsolidation(env.Clock, cluster, env.Client, prov, cloudProvider, recorder, queue)),
	}
	for _, opt := range opts {
		opt(v)
	}
	return v
}

func (t *TestEmptinessValidator) Validate(ctx context.Context, cmd disruption.Command, _ time.Duration) (disruption.Command, error) {
	if t.blocked {
		blockingBudget(t.nodes, t.nodeClaims, t.nodePool)
	}
	if t.churn {
		churn(t.nodes, t.nodeClaims)
	}
	if t.nominated {
		nominated(t.nodes, t.nodeClaims)
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

func NewTestSingleConsolidationValidator(nodePool *v1.NodePool, opts ...TestConsolidationValidatorOption) disruption.Validator {
	return newTestConsolidationValidator(nodePool, disruption.NewSingleConsolidationValidator(disruption.MakeConsolidation(env.Clock, cluster, env.Client, prov, cloudProvider, recorder, queue)), opts...)
}

func NewTestMultiConsolidationValidator(nodePool *v1.NodePool, opts ...TestConsolidationValidatorOption) disruption.Validator {
	return newTestConsolidationValidator(nodePool, disruption.NewMultiConsolidationValidator(disruption.MakeConsolidation(env.Clock, cluster, env.Client, prov, cloudProvider, recorder, queue)), opts...)
}

func newTestConsolidationValidator(nodePool *v1.NodePool, c *disruption.ConsolidationValidator, opts ...TestConsolidationValidatorOption) disruption.Validator {
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
	stateNodes := t.cluster.DeepCopyNodes()
	nodes := make([]*corev1.Node, len(stateNodes))
	nodeClaims := make([]*v1.NodeClaim, len(stateNodes))
	for i, stateNode := range stateNodes {
		nodes[i] = stateNode.Node
		nodeClaims[i] = stateNode.NodeClaim
	}
	if t.blocked {
		blockingBudget(nodes, nodeClaims, t.nodePool)
	}
	if t.churn {
		churn(nodes, nodeClaims)
	}
	if t.nominated {
		nominated(nodes, nodeClaims)
	}
	return t.consolidation.Validate(ctx, cmd, 0)
}

func churn(nodes []*corev1.Node, nodeClaims []*v1.NodeClaim) {
	var pods []*corev1.Pod
	ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, nodes, nodeClaims)
	rs := test.ReplicaSet()
	ExpectApplied(ctx, env.Client, rs)
	pods = test.Pods(1, test.PodOptions{
		ResourceRequirements: corev1.ResourceRequirements{
			Requests: map[corev1.ResourceName]resource.Quantity{
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
					Controller:         new(true),
					BlockOwnerDeletion: new(true),
				},
			}}})
	ExpectApplied(ctx, env.Client, pods[0])
	ExpectManualBinding(ctx, env.Client, pods[0], nodes[0])
	cluster.NominateNodeForPod(ctx, nodes[0].Spec.ProviderID)
	Expect(cluster.UpdateNode(ctx, nodes[0])).To(Succeed())
}

func blockingBudget(nodes []*corev1.Node, nodeClaims []*v1.NodeClaim, nodePool *v1.NodePool) {
	ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, nodes, nodeClaims)
	nodePool.Spec.Disruption.Budgets = []v1.Budget{{
		Nodes: "0%",
	}}
	ExpectApplied(ctx, env.Client, nodePool)
}

func nominated(nodes []*corev1.Node, nodeClaims []*v1.NodeClaim) {
	ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, nodes, nodeClaims)
	for i := range nodes {
		cluster.NominateNodeForPod(ctx, nodes[i].Spec.ProviderID)
		cluster.NominateNodeForPod(ctx, nodes[i].Spec.ProviderID)
		Expect(cluster.UpdateNode(ctx, nodes[i])).To(Succeed())
	}
}
