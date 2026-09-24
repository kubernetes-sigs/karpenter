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

type eligibleRepairPolicy struct {
	Priority   int
	EligibleAt time.Time
}

// RepairPolicyEvaluation is the complete policy evaluation for one current NodeCondition.
type RepairPolicyEvaluation struct {
	ConditionType          corev1.NodeConditionType
	ConditionStatus        corev1.ConditionStatus
	Reason                 string
	Action                 cloudprovider.RepairAction
	EligibleAt             time.Time
	TerminationGracePeriod *time.Duration
	Fallback               bool
	MatchingPolicies       int
	EligiblePolicies       int
	eligiblePolicies       []eligibleRepairPolicy
}

// RepairPolicyResult is the complete policy evaluation for one Node.
type RepairPolicyResult struct {
	Score       float64
	Decision    *RepairPolicyEvaluation
	Evaluations []RepairPolicyEvaluation
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

// Evaluate returns the score, governing repair decision, and per-condition diagnostics for one Node. Provider policies
// are immutable after construction, so returned duration pointers must be treated as read-only.
func (p *RepairPolicyMatcher) Evaluate(node *corev1.Node, now time.Time) *RepairPolicyResult {
	result := &RepairPolicyResult{
		Evaluations: make([]RepairPolicyEvaluation, 0, len(node.Status.Conditions)),
	}
	governingPriority := 0
	governingDeadline := time.Time{}
	governingIndex := -1
	for i := range node.Status.Conditions {
		evaluation, ok := p.evaluateCondition(node.Status.Conditions[i], now)
		if !ok {
			continue
		}
		result.Evaluations = append(result.Evaluations, evaluation)
		evaluationIndex := len(result.Evaluations) - 1
		for _, policy := range evaluation.eligiblePolicies {
			age := now.Sub(policy.EligibleAt)
			result.Score = max(result.Score, float64(p.ranks[policy.Priority])+age.Minutes()/agingConstant.Minutes())
			if governingIndex == -1 || policy.Priority > governingPriority ||
				(policy.Priority == governingPriority && policy.EligibleAt.Before(governingDeadline)) {
				governingPriority = policy.Priority
				governingDeadline = policy.EligibleAt
				governingIndex = evaluationIndex
			}
		}
	}
	if governingIndex != -1 {
		result.Decision = &result.Evaluations[governingIndex]
	}
	return result
}

func (p *RepairPolicyMatcher) evaluateCondition(condition corev1.NodeCondition, now time.Time) (RepairPolicyEvaluation, bool) {
	group, ok := p.groups[policyKey{conditionType: condition.Type, conditionStatus: condition.Status}]
	if !ok {
		return RepairPolicyEvaluation{}, false
	}

	result := RepairPolicyEvaluation{
		ConditionType:    condition.Type,
		ConditionStatus:  condition.Status,
		Reason:           condition.Reason,
		eligiblePolicies: make([]eligibleRepairPolicy, 0, len(group.specificPolicies)),
	}
	for i := range group.specificPolicies {
		policy := group.specificPolicies[i]
		if !policy.reasonRegex.MatchString(condition.Reason) {
			continue
		}
		result.considerPolicy(policy, condition.LastTransitionTime.Time, now)
	}
	if result.MatchingPolicies == 0 {
		result.Fallback = true
		result.considerPolicy(p.fallbackPolicy, condition.LastTransitionTime.Time, now)
	}
	result.EligiblePolicies = len(result.eligiblePolicies)
	slices.SortFunc(result.eligiblePolicies, func(a, b eligibleRepairPolicy) int {
		if a.Priority != b.Priority {
			return b.Priority - a.Priority
		}
		return a.EligibleAt.Compare(b.EligibleAt)
	})
	return result, true
}

// Matches returns true when the condition is covered by the provider policy set, regardless of toleration.
func (p *RepairPolicyMatcher) Matches(condition corev1.NodeCondition) bool {
	_, ok := p.groups[policyKey{conditionType: condition.Type, conditionStatus: condition.Status}]
	return ok
}

func (r *RepairPolicyEvaluation) considerPolicy(policy compiledPolicy, transitionTime, now time.Time) {
	r.MatchingPolicies++
	eligibleAt := transitionTime.Add(policy.TolerationDuration)
	if now.Before(eligibleAt) {
		if len(r.eligiblePolicies) == 0 && (r.EligibleAt.IsZero() ||
			eligibleAt.Before(r.EligibleAt) ||
			(eligibleAt.Equal(r.EligibleAt) && repairActionRank(policy.Action) > repairActionRank(r.Action))) {
			r.Action = policy.Action
			r.EligibleAt = eligibleAt
		}
		return
	}

	r.eligiblePolicies = append(r.eligiblePolicies, eligibleRepairPolicy{
		Priority:   policy.Priority,
		EligibleAt: eligibleAt,
	})
	r.considerTerminationGracePeriod(policy.TerminationGracePeriod)
	if len(r.eligiblePolicies) == 1 || repairActionRank(policy.Action) > repairActionRank(r.Action) {
		r.Action = policy.Action
		r.EligibleAt = eligibleAt
		return
	}
	if policy.Action == r.Action && eligibleAt.Before(r.EligibleAt) {
		r.EligibleAt = eligibleAt
	}
}

func (r *RepairPolicyEvaluation) considerTerminationGracePeriod(terminationGracePeriod *time.Duration) {
	if terminationGracePeriod == nil {
		return
	}
	if r.TerminationGracePeriod == nil || *terminationGracePeriod < *r.TerminationGracePeriod {
		r.TerminationGracePeriod = terminationGracePeriod
	}
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
