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

const (
	minRepairPolicyPriority = 0
	maxRepairPolicyPriority = 100
)

type compiledPolicy struct {
	cloudprovider.RepairPolicy
	reasonRegex *regexp.Regexp
}

type policyGroup struct {
	specificPolicies []compiledPolicy
}

// RepairPolicyMatcher validates and evaluates provider repair policies.
type RepairPolicyMatcher struct {
	groups         map[policyKey]policyGroup
	fallbackPolicy compiledPolicy
}

// RepairPolicyResult is the eligible repair behavior for one current NodeCondition.
type RepairPolicyResult struct {
	ConditionType          corev1.NodeConditionType
	ConditionStatus        corev1.ConditionStatus
	Reason                 string
	Action                 cloudprovider.RepairAction
	EligibleAt             time.Time
	TerminationGracePeriod *time.Duration
}

type repairDecision struct {
	condition              corev1.NodeCondition
	action                 cloudprovider.RepairAction
	eligibleAt             time.Time
	terminationGracePeriod *time.Duration
	eligible               bool
	fallback               bool
	matchingPolicies       int
	eligiblePolicies       int
}

// NewRepairPolicyMatcher validates and compiles a complete provider repair policy set.
func NewRepairPolicyMatcher(policies []cloudprovider.RepairPolicy, supportedActions sets.Set[cloudprovider.RepairAction]) (*RepairPolicyMatcher, error) {
	groups := map[policyKey]policyGroup{}
	var fallbackPolicy compiledPolicy
	hasFallbackPolicy := false
	var errs error

	for i, policy := range policies {
		key := policyKey{conditionType: policy.ConditionType, conditionStatus: policy.ConditionStatus}
		group := groups[key]
		compiled, err := compileRepairPolicy(i, policy, supportedActions)
		errs = multierr.Append(errs, err)

		if policy.ReasonRegex == "" {
			if hasFallbackPolicy {
				errs = multierr.Append(errs, fmt.Errorf(
					"repair policy set has multiple default fallbacks: %+v and %+v",
					fallbackPolicy.RepairPolicy,
					compiled.RepairPolicy,
				))
			} else {
				fallbackPolicy = compiled
				hasFallbackPolicy = true
			}
		} else {
			group.specificPolicies = append(group.specificPolicies, compiled)
		}
		groups[key] = group
	}

	if !hasFallbackPolicy {
		errs = multierr.Append(errs, fmt.Errorf("repair policy set must define one default fallback"))
	} else if fallbackPolicy.Action != cloudprovider.ReplaceNode {
		errs = multierr.Append(errs, fmt.Errorf(
			"default fallback policy %+v must use action %q",
			fallbackPolicy.RepairPolicy,
			cloudprovider.ReplaceNode,
		))
	}
	if errs != nil {
		return nil, errs
	}
	return &RepairPolicyMatcher{
		groups:         groups,
		fallbackPolicy: fallbackPolicy,
	}, nil
}

func compileRepairPolicy(index int, policy cloudprovider.RepairPolicy, supportedActions sets.Set[cloudprovider.RepairAction]) (compiledPolicy, error) {
	policy.TerminationGracePeriod = cloneDuration(policy.TerminationGracePeriod)
	compiled := compiledPolicy{RepairPolicy: policy}
	key := policyKey{conditionType: policy.ConditionType, conditionStatus: policy.ConditionStatus}
	errs := validateRepairPolicy(index, policy, supportedActions)

	reasonRegex, err := compileRepairPolicyReasonRegex(index, policy)
	if err != nil {
		errs = multierr.Append(errs, repairPolicyError(key, err))
	}
	compiled.reasonRegex = reasonRegex
	return compiled, errs
}

func validateRepairPolicy(index int, policy cloudprovider.RepairPolicy, supportedActions sets.Set[cloudprovider.RepairAction]) error {
	key := policyKey{conditionType: policy.ConditionType, conditionStatus: policy.ConditionStatus}
	var errs error
	appendError := func(err error) {
		errs = multierr.Append(errs, repairPolicyError(key, err))
	}
	policyValue := fmt.Sprintf("policy[%d]=%+v", index, policy)

	if policy.ConditionType == "" {
		appendError(fmt.Errorf("%s has an empty condition type", policyValue))
	}
	if !validConditionStatus(policy.ConditionStatus) {
		appendError(fmt.Errorf("%s has invalid condition status %q", policyValue, policy.ConditionStatus))
	}
	if policy.TolerationDuration < 0 {
		appendError(fmt.Errorf("%s has negative toleration duration %s", policyValue, policy.TolerationDuration))
	}
	if policy.TerminationGracePeriod != nil && *policy.TerminationGracePeriod < 0 {
		appendError(fmt.Errorf("%s has negative termination grace period %s", policyValue, *policy.TerminationGracePeriod))
	}
	if policy.Priority < minRepairPolicyPriority || policy.Priority > maxRepairPolicyPriority {
		appendError(fmt.Errorf(
			"%s has priority %d outside the supported range [%d, %d]",
			policyValue,
			policy.Priority,
			minRepairPolicyPriority,
			maxRepairPolicyPriority,
		))
	}
	if !supportedActions.Has(policy.Action) {
		appendError(fmt.Errorf("%s has unsupported action %q", policyValue, policy.Action))
	}
	return errs
}

func compileRepairPolicyReasonRegex(index int, policy cloudprovider.RepairPolicy) (*regexp.Regexp, error) {
	if policy.ReasonRegex == "" {
		return nil, nil
	}
	reasonRegex, err := regexp.Compile(policy.ReasonRegex)
	if err != nil {
		return nil, fmt.Errorf("policy[%d]=%+v has invalid reason regex, %w", index, policy, err)
	}
	return reasonRegex, nil
}

func cloneDuration(duration *time.Duration) *time.Duration {
	if duration == nil {
		return nil
	}
	cloned := *duration
	return &cloned
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
		ConditionType:          decision.condition.Type,
		ConditionStatus:        decision.condition.Status,
		Reason:                 decision.condition.Reason,
		Action:                 decision.action,
		EligibleAt:             decision.eligibleAt,
		TerminationGracePeriod: cloneDuration(decision.terminationGracePeriod),
	}
}

// EligiblePolicies returns the matching policies whose toleration has elapsed.
func (p *RepairPolicyMatcher) EligiblePolicies(condition corev1.NodeCondition, now time.Time) []cloudprovider.RepairPolicy {
	policies, ok := p.matchingPolicies(condition)
	if !ok {
		return nil
	}
	eligible := make([]cloudprovider.RepairPolicy, 0, len(policies))
	for _, policy := range policies {
		if now.Before(condition.LastTransitionTime.Add(policy.TolerationDuration)) {
			continue
		}
		match := policy.RepairPolicy
		match.TerminationGracePeriod = cloneDuration(match.TerminationGracePeriod)
		eligible = append(eligible, match)
	}
	return eligible
}

// Matches returns true when the condition is covered by the provider policy set, regardless of toleration.
func (p *RepairPolicyMatcher) Matches(condition corev1.NodeCondition) bool {
	_, ok := p.groups[policyKey{conditionType: condition.Type, conditionStatus: condition.Status}]
	return ok
}

// DecisionLogValues returns structured diagnostic values for a supported condition, including waiting decisions.
func (p *RepairPolicyMatcher) DecisionLogValues(condition corev1.NodeCondition, now time.Time) []any {
	decision, ok := p.evaluateDecision(condition, now)
	if !ok {
		return nil
	}
	return decision.logValues()
}

func (p *RepairPolicyMatcher) evaluateDecision(condition corev1.NodeCondition, now time.Time) (repairDecision, bool) {
	group, ok := p.groups[policyKey{conditionType: condition.Type, conditionStatus: condition.Status}]
	if !ok {
		return repairDecision{}, false
	}

	decision := repairDecision{
		condition: condition,
	}
	for i := range group.specificPolicies {
		policy := group.specificPolicies[i]
		if !policy.reasonRegex.MatchString(condition.Reason) {
			continue
		}
		decision.considerPolicy(policy, condition.LastTransitionTime.Time, now)
	}
	if decision.matchingPolicies == 0 {
		decision.fallback = true
		decision.considerPolicy(p.fallbackFor(condition), condition.LastTransitionTime.Time, now)
	}
	return decision, true
}

func (p *RepairPolicyMatcher) matchingPolicies(condition corev1.NodeCondition) ([]compiledPolicy, bool) {
	group, ok := p.groups[policyKey{conditionType: condition.Type, conditionStatus: condition.Status}]
	if !ok {
		return nil, false
	}

	matches := make([]compiledPolicy, 0, len(group.specificPolicies))
	for _, policy := range group.specificPolicies {
		if policy.reasonRegex.MatchString(condition.Reason) {
			matches = append(matches, policy)
		}
	}
	if len(matches) != 0 {
		return matches, true
	}
	return []compiledPolicy{p.fallbackFor(condition)}, true
}

func (p *RepairPolicyMatcher) fallbackFor(condition corev1.NodeCondition) compiledPolicy {
	fallback := p.fallbackPolicy
	fallback.ConditionType = condition.Type
	fallback.ConditionStatus = condition.Status
	return fallback
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
	d.considerTerminationGracePeriod(policy.TerminationGracePeriod)
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

func (d *repairDecision) considerTerminationGracePeriod(terminationGracePeriod *time.Duration) {
	if terminationGracePeriod == nil {
		return
	}
	if d.terminationGracePeriod == nil || *terminationGracePeriod < *d.terminationGracePeriod {
		d.terminationGracePeriod = cloneDuration(terminationGracePeriod)
	}
}

func (d *repairDecision) logValues() []any {
	values := []any{
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
	if d.terminationGracePeriod != nil {
		values = append(values, "termination-grace-period", *d.terminationGracePeriod)
	}
	return values
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
