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

package performance

import (
	"fmt"
	"os"
	"strconv"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"sigs.k8s.io/karpenter/kwok/apis/v1alpha1"
	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/test"
	"sigs.k8s.io/karpenter/test/pkg/environment/common"
)

var nodePool *v1.NodePool
var nodeClass *unstructured.Unstructured
var env *common.Environment

// decisionRatioThreshold controls the minimum decision ratio for consolidation
// when using WhenCostJustifiesDisruption policy. Default is 1.0 (break-even).
// Can be overridden via DECISION_RATIO_THRESHOLD environment variable.
var decisionRatioThreshold float64 = 1.0

// consolidateWhenPolicy controls the ConsolidateWhen policy for performance tests.
// Can be overridden via CONSOLIDATE_WHEN environment variable.
var consolidateWhenPolicy = v1.ConsolidateWhenEmptyOrUnderutilized

func init() {
	// Initialize decision ratio threshold from environment variable if set
	if val := os.Getenv("DECISION_RATIO_THRESHOLD"); val != "" {
		if threshold, err := strconv.ParseFloat(val, 64); err == nil && threshold > 0 {
			decisionRatioThreshold = threshold
			fmt.Printf("=== Decision Ratio Threshold set to %.2f from environment ===\n", decisionRatioThreshold)
		} else {
			fmt.Printf("WARNING: Invalid DECISION_RATIO_THRESHOLD value '%s', using default %.2f\n", val, decisionRatioThreshold)
		}
	}

	// Initialize ConsolidateWhen policy from environment variable if set
	if val := os.Getenv("CONSOLIDATE_WHEN"); val != "" {
		switch v1.ConsolidateWhenPolicy(val) {
		case v1.ConsolidateWhenEmpty, v1.ConsolidateWhenEmptyOrUnderutilized, v1.ConsolidateWhenCostJustifiesDisruption:
			consolidateWhenPolicy = v1.ConsolidateWhenPolicy(val)
			fmt.Printf("=== ConsolidateWhen policy set to %s from environment ===\n", val)
		default:
			fmt.Printf("WARNING: Invalid CONSOLIDATE_WHEN value '%s', using default %s\n", val, consolidateWhenPolicy)
		}
	}
}

func TestIntegration(t *testing.T) {
	RegisterFailHandler(Fail)
	BeforeSuite(func() {
		env = common.NewEnvironment(t)
	})
	AfterSuite(func() {
		env.Stop()
	})
	RunSpecs(t, "Performance")
}

var _ = BeforeEach(func() {
	env.BeforeEach()
	nodeClass = env.DefaultNodeClass.DeepCopy()
	nodeClass.SetName(fmt.Sprintf("%s-%s", nodeClass.GetName(), test.RandomName()))
	nodePool = env.DefaultNodePool(nodeClass)
	if env.IsDefaultNodeClassKWOK() {
		test.ReplaceRequirements(nodePool, v1.NodeSelectorRequirementWithMinValues{
			Key:      v1alpha1.InstanceSizeLabelKey,
			Operator: corev1.NodeSelectorOpLt,
			Values:   []string{"32"},
		})
	}
	nodePool.Spec.Limits = v1.Limits{}
	nodePool.Spec.Disruption.ConsolidationPolicy = v1.ConsolidationPolicyWhenEmptyOrUnderutilized
	nodePool.Spec.Disruption.ConsolidateAfter = v1.MustParseNillableDuration("30s")
	nodePool.Spec.Disruption.Budgets = []v1.Budget{{Nodes: "100%"}}

	// Configure consolidation policy and decision ratio threshold
	nodePool.Spec.Disruption.ConsolidateWhen = consolidateWhenPolicy
	nodePool.Spec.Disruption.DecisionRatioThreshold = &decisionRatioThreshold
})

var _ = AfterEach(func() {
	env.Cleanup()
	env.AfterEach()
})

// getConsolidationSettings returns the current consolidation policy and decision ratio threshold
func getConsolidationSettings() (string, float64) {
	consolidateWhen := string(nodePool.Spec.Disruption.ConsolidateWhen)
	decisionRatio := nodePool.Spec.Disruption.GetDecisionRatioThreshold()
	return consolidateWhen, decisionRatio
}
