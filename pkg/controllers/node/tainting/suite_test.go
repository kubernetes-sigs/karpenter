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

package tainting_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"sigs.k8s.io/karpenter/pkg/apis"
	"sigs.k8s.io/karpenter/pkg/apis/v1beta1"
	"sigs.k8s.io/karpenter/pkg/controllers/node/tainting"
	"sigs.k8s.io/karpenter/pkg/test"

	. "sigs.k8s.io/karpenter/pkg/test/expectations"
	. "sigs.k8s.io/karpenter/pkg/utils/testing"
)

var ctx context.Context
var taintingController *tainting.Controller
var env *test.Environment

func TestAPIs(t *testing.T) {
	ctx = TestContextWithLogger(t)
	RegisterFailHandler(Fail)
	RunSpecs(t, "Termination")
}

var _ = BeforeSuite(func() {
	env = test.NewEnvironment(test.WithCRDs(apis.CRDs...), test.WithFieldIndexers(test.NodeClaimFieldIndexer(ctx)))
	taintingController = tainting.NewController(env.Client)
})

var _ = AfterSuite(func() {
	Expect(env.Stop()).To(Succeed(), "Failed to stop environment")
})

var _ = Describe("Tainting", func() {
	var node *v1.Node
	var nodeClaim *v1beta1.NodeClaim
	var disruptionConditions = []string{
		v1beta1.ConditionTypeEmpty,
		v1beta1.ConditionTypeDrifted,
	}

	BeforeEach(func() {
		nodeClaim, node = test.NodeClaimAndNode(v1beta1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Finalizers: []string{v1beta1.TerminationFinalizer}}})
		node.Labels[v1beta1.NodePoolLabelKey] = test.NodePool().Name
		nodeClaim.Status.NodeName = node.Name
	})
	AfterEach(func() {
		ExpectCleanedUp(ctx, env.Client)
	})

	It("shouldn't add the karpenter.sh/candidate taint if no disruption status conditions are set", func() {
		ExpectApplied(ctx, env.Client, node, nodeClaim)
		ExpectObjectReconciled(ctx, env.Client, taintingController, nodeClaim)
		node = ExpectNodes(ctx, env.Client)[0]
		Expect(lo.ContainsBy(node.Spec.Taints, func(t v1.Taint) bool {
			return t.Key == v1beta1.DisruptionCandidateTaintKey
		})).To(BeFalse())

		nodeClaim.StatusConditions().SetTrue(v1beta1.ConditionTypeInitialized)
		ExpectApplied(ctx, env.Client, node, nodeClaim)
		ExpectObjectReconciled(ctx, env.Client, taintingController, nodeClaim)
		node = ExpectNodes(ctx, env.Client)[0]
		Expect(lo.ContainsBy(node.Spec.Taints, func(t v1.Taint) bool {
			return t.Key == v1beta1.DisruptionCandidateTaintKey
		})).To(BeFalse())
	})

	DescribeTable(
		"should add the karpenter.sh/candidate taint for disruption status conditions",
		func(conditions []string) {
			ExpectApplied(ctx, env.Client, node, nodeClaim)
			for _, cond := range conditions {
				nodeClaim.StatusConditions().SetTrue(cond)
			}
			ExpectApplied(ctx, env.Client, nodeClaim)
			ExpectObjectReconciled(ctx, env.Client, taintingController, nodeClaim)
			n := ExpectNodes(ctx, env.Client)[0]
			Expect(lo.ContainsBy(n.Spec.Taints, func(t v1.Taint) bool {
				return t.Key == v1beta1.DisruptionCandidateTaintKey
			})).To(BeTrue())
		},
		lo.Map(disruptionConditions, func(cond string, _ int) TableEntry {
			return Entry(cond, []string{cond})
		}),
		// Any combination of two status conditions (should hold for any number, we'll just test two)
		lo.Map(Combination(disruptionConditions...), func(conds []string, _ int) TableEntry {
			return Entry(strings.Join(conds, " and "), conds)
		}),
	)

	It("should remove the karpenter.sh/candidate once all disruption status conditions are removed", func() {
		ExpectApplied(ctx, env.Client, node, nodeClaim)
		for _, cond := range disruptionConditions {
			nodeClaim.StatusConditions().SetTrue(cond)
		}
		ExpectApplied(ctx, env.Client, nodeClaim)
		ExpectObjectReconciled(ctx, env.Client, taintingController, nodeClaim)
		n := ExpectNodes(ctx, env.Client)[0]
		Expect(lo.ContainsBy(n.Spec.Taints, func(t v1.Taint) bool {
			return t.Key == v1beta1.DisruptionCandidateTaintKey
		})).To(BeTrue())
		for i, cond := range disruptionConditions {
			nodeClaim.StatusConditions().SetFalse(cond, fmt.Sprintf("Not%s", cond), fmt.Sprintf("Not%s", cond))
			ExpectApplied(ctx, env.Client, nodeClaim)
			ExpectObjectReconciled(ctx, env.Client, taintingController, nodeClaim)
			n := ExpectNodes(ctx, env.Client)[0]
			Expect(lo.ContainsBy(n.Spec.Taints, func(t v1.Taint) bool {
				return t.Key == v1beta1.DisruptionCandidateTaintKey
			})).To(Equal(i != len(disruptionConditions)-1))
		}
	})
})

func Combination[T any](values ...T) (result [][]T) {
	for i, ival := range values {
		if i == len(values)-1 {
			continue
		}
		for _, jval := range values[i+1:] {
			result = append(result, []T{ival, jval})
		}
	}
	return
}
