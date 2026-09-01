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

package health

import (
	"fmt"
	"regexp"
	"time"

	"github.com/awslabs/operatorpkg/serrors"
	"go.uber.org/multierr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"

	"sigs.k8s.io/karpenter/pkg/cloudprovider"
)

type policyKey struct {
	conditionType   corev1.NodeConditionType
	conditionStatus corev1.ConditionStatus
}

type compiledPolicy struct {
	cloudprovider.RepairPolicy
	reasonRegex *regexp.Regexp
}

type policyGroup struct {
	specificPolicies []compiledPolicy
	fallbackPolicy   *compiledPolicy
	fallbackCount    int
}

// RepairPolicyMatcher validates and evaluates provider repair policies.
type RepairPolicyMatcher struct {
	groups map[policyKey]policyGroup
}

// RepairPolicyResult is the eligible repair behavior for one current NodeCondition.
type RepairPolicyResult struct {
	ConditionType   corev1.NodeConditionType
	ConditionStatus corev1.ConditionStatus
	Reason          string
	Action          cloudprovider.RepairAction
	EligibleAt      time.Time
}

type repairDecision struct {
	condition        corev1.NodeCondition
	action           cloudprovider.RepairAction
	eligibleAt       time.Time
	eligible         bool
	fallback         bool
	matchingPolicies int
	eligiblePolicies int
}

// NewRepairPolicyMatcher validates and compiles a complete provider repair policy set.
func NewRepairPolicyMatcher(policies []cloudprovider.RepairPolicy, supportedActions sets.Set[cloudprovider.RepairAction]) (*RepairPolicyMatcher, error) {
	groups := map[policyKey]policyGroup{}
	groupKeys := make([]policyKey, 0)
	var errs error

	for i, policy := range policies {
		key := policyKey{conditionType: policy.ConditionType, conditionStatus: policy.ConditionStatus}
		group, ok := groups[key]
		if !ok {
			groupKeys = append(groupKeys, key)
		}
		compiled, err := compileRepairPolicy(i, policy, supportedActions)
		errs = multierr.Append(errs, err)

		if policy.ReasonRegex == "" {
			group.fallbackCount++
			group.fallbackPolicy = &compiled
		} else {
			group.specificPolicies = append(group.specificPolicies, compiled)
		}
		groups[key] = group
	}

	for _, key := range groupKeys {
		group := groups[key]
		if group.fallbackCount != 1 {
			errs = multierr.Append(errs, repairPolicyError(
				key,
				fmt.Errorf("must have exactly one condition-level fallback, found %d", group.fallbackCount),
			))
		}
	}
	if errs != nil {
		return nil, errs
	}
	return &RepairPolicyMatcher{groups: groups}, nil
}

func compileRepairPolicy(index int, policy cloudprovider.RepairPolicy, supportedActions sets.Set[cloudprovider.RepairAction]) (compiledPolicy, error) {
	compiled := compiledPolicy{RepairPolicy: policy}
	key := policyKey{conditionType: policy.ConditionType, conditionStatus: policy.ConditionStatus}
	var errs error
	appendError := func(err error) {
		errs = multierr.Append(errs, repairPolicyError(key, err))
	}

	if policy.ConditionType == "" {
		appendError(fmt.Errorf("policy[%d] has an empty condition type", index))
	}
	if !validConditionStatus(policy.ConditionStatus) {
		appendError(fmt.Errorf("policy[%d] has invalid condition status %q", index, policy.ConditionStatus))
	}
	if policy.TolerationDuration < 0 {
		appendError(fmt.Errorf("policy[%d] has negative toleration duration %s", index, policy.TolerationDuration))
	}
	if !supportedActions.Has(policy.Action) {
		appendError(fmt.Errorf("policy[%d] has unsupported action %q", index, policy.Action))
	}
	if policy.ReasonRegex == "" && policy.Action != cloudprovider.ReplaceNode {
		appendError(fmt.Errorf("policy[%d] condition-level fallback must use action %q", index, cloudprovider.ReplaceNode))
	}
	if policy.ReasonRegex != "" {
		reasonRegex, err := regexp.Compile(policy.ReasonRegex)
		if err != nil {
			appendError(fmt.Errorf("policy[%d] has invalid reason regex %q, %w", index, policy.ReasonRegex, err))
		} else {
			compiled.reasonRegex = reasonRegex
		}
	}
	return compiled, errs
}

func repairPolicyError(key policyKey, err error) error {
	return serrors.Wrap(
		fmt.Errorf("validating repair policy for condition status %q, %w", key.conditionStatus, err),
		"condition", key.conditionType,
	)
}

func validConditionStatus(status corev1.ConditionStatus) bool {
	return status == corev1.ConditionTrue || status == corev1.ConditionFalse || status == corev1.ConditionUnknown
}

// Evaluate returns the eligible repair result for one current NodeCondition.
// It returns nil when the condition does not match or its toleration has not elapsed.
func (p *RepairPolicyMatcher) Evaluate(condition corev1.NodeCondition, now time.Time) *RepairPolicyResult {
	decision, ok := p.evaluateDecision(condition, now)
	if !ok || !decision.eligible {
		return nil
	}
	return &RepairPolicyResult{
		ConditionType:   decision.condition.Type,
		ConditionStatus: decision.condition.Status,
		Reason:          decision.condition.Reason,
		Action:          decision.action,
		EligibleAt:      decision.eligibleAt,
	}
}

func (p *RepairPolicyMatcher) evaluateDecision(condition corev1.NodeCondition, now time.Time) (repairDecision, bool) {
	group, ok := p.groups[policyKey{conditionType: condition.Type, conditionStatus: condition.Status}]
	if !ok {
		return repairDecision{}, false
	}

	decision := repairDecision{condition: condition}
	for _, policy := range group.specificPolicies {
		if policy.reasonRegex.MatchString(condition.Reason) {
			decision.considerPolicy(policy, condition.LastTransitionTime.Time, now)
		}
	}
	if decision.matchingPolicies == 0 {
		decision.fallback = true
		decision.considerPolicy(*group.fallbackPolicy, condition.LastTransitionTime.Time, now)
	}
	return decision, true
}

func (d *repairDecision) considerPolicy(policy compiledPolicy, transitionTime, now time.Time) {
	d.matchingPolicies++
	eligibleAt := transitionTime.Add(policy.TolerationDuration)
	if now.Before(eligibleAt) {
		if !d.eligible && (d.eligibleAt.IsZero() ||
			eligibleAt.Before(d.eligibleAt) ||
			(eligibleAt.Equal(d.eligibleAt) && repairActionRank(policy.Action) > repairActionRank(d.action))) {
			d.action = policy.Action
			d.eligibleAt = eligibleAt
		}
		return
	}

	d.eligiblePolicies++
	if !d.eligible || repairActionRank(policy.Action) > repairActionRank(d.action) {
		d.action = policy.Action
		d.eligibleAt = eligibleAt
		d.eligible = true
		return
	}
	if policy.Action == d.action && eligibleAt.Before(d.eligibleAt) {
		d.eligibleAt = eligibleAt
	}
}

func (p *RepairPolicyMatcher) hasCondition(condition corev1.NodeCondition) bool {
	_, ok := p.groups[policyKey{conditionType: condition.Type, conditionStatus: condition.Status}]
	return ok
}

func (p *RepairPolicyMatcher) hasReasonPolicies(condition corev1.NodeCondition) bool {
	group, ok := p.groups[policyKey{conditionType: condition.Type, conditionStatus: condition.Status}]
	return ok && len(group.specificPolicies) != 0
}

func (d *repairDecision) logValues() []any {
	return []any{
		"condition", d.condition.Type,
		"status", d.condition.Status,
		"reason", d.condition.Reason,
		"fallback", d.fallback,
		"matching-policies", d.matchingPolicies,
		"eligible-policies", d.eligiblePolicies,
		"action", d.action,
		"eligible", d.eligible,
		"eligible-at", d.eligibleAt,
	}
}

func repairActionRank(action cloudprovider.RepairAction) int {
	switch action {
	case cloudprovider.ReplaceNode:
		return 1
	case cloudprovider.RebootNode:
		return 0
	default:
		return -1
	}
}
