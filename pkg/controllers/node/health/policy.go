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
	"slices"
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
	// agingConstant is the time a node must wait past toleration to earn one dense priority rank. It bounds starvation:
	// a node overtakes a freshly eligible rival one rank higher after one agingConstant.
	agingConstant = 30 * time.Minute
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
	ranks          map[int]int
}

// RepairResult merges all eligible policies for one Node: Score is the maximum urgency, Action is the most disruptive,
// EligibleAt is the earliest eligibility, and TerminationGracePeriod is the shortest bound. Action is empty when no
// policy is eligible; Condition identifies the deterministic source of the selected action.
type RepairResult struct {
	Score                  float64
	Action                 cloudprovider.RepairAction
	Condition              corev1.NodeConditionType
	EligibleAt             time.Time
	TerminationGracePeriod *time.Duration
}

type repairSelection struct {
	priority   int
	eligibleAt time.Time
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
		ranks:          denseRanks(policies),
	}, nil
}

func compileRepairPolicy(index int, policy cloudprovider.RepairPolicy, supportedActions sets.Set[cloudprovider.RepairAction]) (compiledPolicy, error) {
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

func repairPolicyError(key policyKey, err error) error {
	return serrors.Wrap(
		fmt.Errorf("validating repair policy for condition status %q, %w", key.conditionStatus, err),
		"condition", key.conditionType,
	)
}

func validConditionStatus(status corev1.ConditionStatus) bool {
	return status == corev1.ConditionTrue || status == corev1.ConditionFalse || status == corev1.ConditionUnknown
}

// Evaluate returns the merged repair policy decision for one Node. Provider policies are immutable after construction,
// so returned duration pointers must be treated as read-only.
func (p *RepairPolicyMatcher) Evaluate(node *corev1.Node, now time.Time) RepairResult {
	result := RepairResult{}
	selected := repairSelection{}
	for i := range node.Status.Conditions {
		condition := node.Status.Conditions[i]
		group, ok := p.groups[policyKey{conditionType: condition.Type, conditionStatus: condition.Status}]
		if !ok {
			continue
		}
		matched := false
		for j := range group.specificPolicies {
			policy := group.specificPolicies[j]
			if policy.reasonRegex.MatchString(condition.Reason) {
				matched = true
				selected = result.considerPolicy(condition, policy, p.ranks[policy.Priority], now, selected)
			}
		}
		if !matched {
			selected = result.considerPolicy(
				condition,
				p.fallbackPolicy,
				p.ranks[p.fallbackPolicy.Priority],
				now,
				selected,
			)
		}
	}
	return result
}

// Matches returns true when the condition is covered by the provider policy set, regardless of toleration.
func (p *RepairPolicyMatcher) Matches(condition corev1.NodeCondition) bool {
	_, ok := p.groups[policyKey{conditionType: condition.Type, conditionStatus: condition.Status}]
	return ok
}

func (r *RepairResult) considerPolicy(
	condition corev1.NodeCondition,
	policy compiledPolicy,
	rank int,
	now time.Time,
	selected repairSelection,
) repairSelection {
	eligibleAt := condition.LastTransitionTime.Add(policy.TolerationDuration)
	if eligibleAt.After(now) {
		return selected
	}
	age := now.Sub(eligibleAt)
	r.Score = max(r.Score, float64(rank)+age.Minutes()/agingConstant.Minutes())
	if r.EligibleAt.IsZero() || eligibleAt.Before(r.EligibleAt) {
		r.EligibleAt = eligibleAt
	}
	if policy.TerminationGracePeriod != nil &&
		(r.TerminationGracePeriod == nil || *policy.TerminationGracePeriod < *r.TerminationGracePeriod) {
		r.TerminationGracePeriod = policy.TerminationGracePeriod
	}
	if repairActionRank(policy.Action) > repairActionRank(r.Action) ||
		(policy.Action == r.Action && conditionPrecedes(policy.Priority, eligibleAt, condition.Type, selected, r.Condition)) {
		r.Action = policy.Action
		r.Condition = condition.Type
		return repairSelection{priority: policy.Priority, eligibleAt: eligibleAt}
	}
	return selected
}

func conditionPrecedes(priority int, eligibleAt time.Time, condition corev1.NodeConditionType, selected repairSelection, selectedCondition corev1.NodeConditionType) bool {
	if priority != selected.priority {
		return priority > selected.priority
	}
	if !eligibleAt.Equal(selected.eligibleAt) {
		return eligibleAt.Before(selected.eligibleAt)
	}
	return condition < selectedCondition
}

func denseRanks(policies []cloudprovider.RepairPolicy) map[int]int {
	uniquePriorities := map[int]struct{}{}
	for _, policy := range policies {
		uniquePriorities[policy.Priority] = struct{}{}
	}
	priorities := make([]int, 0, len(uniquePriorities))
	for priority := range uniquePriorities {
		priorities = append(priorities, priority)
	}
	slices.Sort(priorities)
	ranks := make(map[int]int, len(priorities))
	for rank, priority := range priorities {
		ranks[priority] = rank
	}
	return ranks
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
