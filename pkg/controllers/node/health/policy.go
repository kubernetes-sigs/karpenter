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
	"context"
	"fmt"
	"regexp"
	"slices"
	"time"

	"github.com/awslabs/operatorpkg/serrors"
	"go.uber.org/multierr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"

	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/operator/options"
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
	Condition   corev1.NodeCondition
}

// supportedRepairActions are the repair actions node repair can perform.
var supportedRepairActions = sets.New(cloudprovider.ReplaceNode, cloudprovider.RebootNode)

// RepairPolicyMatcher validates and evaluates provider repair policies.
type RepairPolicyMatcher struct {
	groups         map[policyKey][]compiledPolicy
	fallbackPolicy compiledPolicy
	ranks          map[int]int
}

// RepairResult merges all eligible policies for one Node. Condition, ConditionStatus, Reason, ReasonRegex, Fallback,
// and SelectedEligibleAt identify the deterministic source of the selected Action, while TerminationGracePeriod is
// the shortest bound and TerminationGracePeriodCondition identifies the condition that supplied it.
type RepairResult struct {
	Score                            float64
	Action                           cloudprovider.RepairAction
	Condition                        corev1.NodeConditionType
	ConditionStatus                  corev1.ConditionStatus
	Reason                           string
	ReasonRegex                      string
	Fallback                         bool
	SelectedEligibleAt               time.Time
	TerminationGracePeriod           *time.Duration
	TerminationGracePeriodCondition  corev1.NodeConditionType
	selectedPriority                 int
	terminationGracePeriodEligibleAt time.Time
}

// NewRepairPolicyMatcher compiles the cloud provider's repair policies once, for cluster state and node repair to
// share. It returns nil when node repair is disabled, and an error when node repair is enabled but the provider defines
// no policies or an invalid set.
func NewRepairPolicyMatcher(ctx context.Context, cloudProvider cloudprovider.CloudProvider) (*RepairPolicyMatcher, error) {
	if !options.FromContext(ctx).FeatureGates.NodeRepair {
		return nil, nil
	}
	policies := cloudProvider.RepairPolicies()
	if len(policies) == 0 {
		return nil, fmt.Errorf("node repair requires the cloud provider to define RepairPolicies, but it defines none")
	}
	matcher, err := newRepairPolicyMatcher(policies, supportedRepairActions)
	if err != nil {
		return nil, fmt.Errorf("node repair requires valid RepairPolicies, %w", err)
	}
	return matcher, nil
}

// newRepairPolicyMatcher validates and compiles a complete provider repair policy set.
func newRepairPolicyMatcher(policies []cloudprovider.RepairPolicy, supportedActions sets.Set[cloudprovider.RepairAction]) (*RepairPolicyMatcher, error) {
	groups := map[policyKey][]compiledPolicy{}
	var fallbackPolicy compiledPolicy
	hasFallbackPolicy := false
	var errs error

	for i, policy := range policies {
		key := policyKey{conditionType: policy.ConditionType, conditionStatus: policy.ConditionStatus}
		specificPolicies := groups[key]
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
			specificPolicies = append(specificPolicies, compiled)
		}
		groups[key] = specificPolicies
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

// RepairPolicyMatch is a policy that matches one of a Node's conditions. It does not depend on the clock, so it can be
// computed when the Node changes and resolved against the clock later.
type RepairPolicyMatch struct {
	policy     compiledPolicy
	rank       int
	eligibleAt time.Time
}

// LogValues returns the decision as structured logging key/value pairs.
func (r RepairResult) LogValues() []any {
	values := []any{
		"condition", r.Condition,
		"status", r.ConditionStatus,
		"reason", r.Reason,
		"reason-regex", r.ReasonRegex,
		"fallback", r.Fallback,
		"action", r.Action,
		"eligible-at", r.SelectedEligibleAt,
	}
	if r.TerminationGracePeriod != nil {
		values = append(values,
			"termination-grace-period", *r.TerminationGracePeriod,
			"termination-grace-period-condition", r.TerminationGracePeriodCondition,
		)
	}
	return values
}

// LogValues returns the match as structured logging key/value pairs.
func (m RepairPolicyMatch) LogValues() []any {
	values := []any{
		"condition", m.policy.Condition.Type,
		"status", m.policy.Condition.Status,
		"reason", m.policy.Condition.Reason,
		"reason-regex", m.policy.ReasonRegex,
		"fallback", m.policy.ReasonRegex == "",
		"action", m.policy.Action,
		"eligible-at", m.eligibleAt,
	}
	if m.policy.TerminationGracePeriod != nil {
		values = append(values, "termination-grace-period", *m.policy.TerminationGracePeriod)
	}
	return values
}

// DeepCopyInto copies the match. Compiled policies are immutable, so the copy shares them.
func (in *RepairPolicyMatch) DeepCopyInto(out *RepairPolicyMatch) {
	*out = *in
}

// matchCondition and matchCreated are the only parts of a Node that matching reads. Match and MatchInputsEqual both
// derive from them, so a cached match is valid exactly while they are equal. Every field is comparable with ==.
type matchCondition struct {
	conditionType  corev1.NodeConditionType
	status         corev1.ConditionStatus
	reason         string
	lastTransition time.Time
}

func newMatchCondition(c corev1.NodeCondition) matchCondition {
	return matchCondition{conditionType: c.Type, status: c.Status, reason: c.Reason, lastTransition: c.LastTransitionTime.UTC()}
}

func matchCreated(node *corev1.Node) time.Time {
	return node.CreationTimestamp.UTC()
}

// MatchInputsEqual returns true when Match returns the same matches for both Nodes. It does not allocate, since it
// runs on every Node update.
func MatchInputsEqual(a, b *corev1.Node) bool {
	if matchCreated(a) != matchCreated(b) || len(a.Status.Conditions) != len(b.Status.Conditions) {
		return false
	}
	for i := range a.Status.Conditions {
		if newMatchCondition(a.Status.Conditions[i]) != newMatchCondition(b.Status.Conditions[i]) {
			return false
		}
	}
	return true
}

// Match returns every policy that matches one of the Node's conditions, regardless of toleration. Provider policies are
// immutable after construction, so duration pointers in the resolved result must be treated as read-only.
func (p *RepairPolicyMatcher) Match(node *corev1.Node) []RepairPolicyMatch {
	created := matchCreated(node)
	var matches []RepairPolicyMatch
	for i := range node.Status.Conditions {
		c := newMatchCondition(node.Status.Conditions[i])
		condition := corev1.NodeCondition{Type: c.conditionType, Status: c.status, Reason: c.reason}
		// A condition cannot predate its Node; clamping also gives an omitted transition time a durable lower bound.
		transitionTime := c.lastTransition
		if transitionTime.Before(created) {
			transitionTime = created
		}
		specificPolicies, ok := p.groups[policyKey{conditionType: c.conditionType, conditionStatus: c.status}]
		if !ok {
			continue
		}
		matched := false
		for j := range specificPolicies {
			if specificPolicies[j].reasonRegex.MatchString(c.reason) {
				matched = true
				matches = append(matches, p.newMatch(specificPolicies[j], condition, transitionTime))
			}
		}
		if !matched {
			matches = append(matches, p.newMatch(p.fallbackPolicy, condition, transitionTime))
		}
	}
	return matches
}

func (p *RepairPolicyMatcher) newMatch(policy compiledPolicy, condition corev1.NodeCondition, transitionTime time.Time) RepairPolicyMatch {
	policy.Condition = condition
	return RepairPolicyMatch{policy: policy, rank: p.ranks[policy.Priority], eligibleAt: transitionTime.Add(policy.TolerationDuration)}
}

// Resolve merges the matches that are eligible at now into one repair decision, measuring each toleration from no
// earlier than notBefore.
func Resolve(matches []RepairPolicyMatch, now, notBefore time.Time) RepairResult {
	result := RepairResult{}
	for _, match := range matches {
		eligibleAt := match.eligibleAt
		if earliest := notBefore.Add(match.policy.TolerationDuration); eligibleAt.Before(earliest) {
			eligibleAt = earliest
		}
		result.mergePolicy(match.policy, match.rank, eligibleAt, now)
	}
	return result
}

// Matches returns true when the condition is covered by the provider policy set, regardless of toleration.
func (p *RepairPolicyMatcher) Matches(condition corev1.NodeCondition) bool {
	_, ok := p.groups[policyKey{conditionType: condition.Type, conditionStatus: condition.Status}]
	return ok
}

func (r *RepairResult) mergePolicy(policy compiledPolicy, rank int, eligibleAt, now time.Time) {
	condition := policy.Condition
	if eligibleAt.After(now) {
		return
	}
	age := now.Sub(eligibleAt)
	r.Score = max(r.Score, float64(rank)+age.Minutes()/agingConstant.Minutes())
	if policy.TerminationGracePeriod != nil && r.shorterTerminationGracePeriod(*policy.TerminationGracePeriod, condition.Type, eligibleAt) {
		r.TerminationGracePeriod = policy.TerminationGracePeriod
		r.TerminationGracePeriodCondition = condition.Type
		r.terminationGracePeriodEligibleAt = eligibleAt
	}

	selected := r.Action == "" || policy.Action.IsMoreDisruptiveThan(r.Action)
	if policy.Action == r.Action {
		switch {
		case policy.Priority != r.selectedPriority:
			selected = policy.Priority > r.selectedPriority
		case !eligibleAt.Equal(r.SelectedEligibleAt):
			selected = eligibleAt.Before(r.SelectedEligibleAt)
		default:
			selected = condition.Type < r.Condition
		}
	}
	if selected {
		r.Action = policy.Action
		r.Condition = condition.Type
		r.ConditionStatus = condition.Status
		r.Reason = condition.Reason
		r.ReasonRegex = policy.ReasonRegex
		r.Fallback = policy.ReasonRegex == ""
		r.SelectedEligibleAt = eligibleAt
		r.selectedPriority = policy.Priority
	}
}

// shorterTerminationGracePeriod reports whether a policy bound should replace the current one. Equal bounds keep the
// earliest-eligible condition, then the lowest condition type, so the source is independent of condition order.
func (r *RepairResult) shorterTerminationGracePeriod(bound time.Duration, condition corev1.NodeConditionType, eligibleAt time.Time) bool {
	switch {
	case r.TerminationGracePeriod == nil:
		return true
	case bound != *r.TerminationGracePeriod:
		return bound < *r.TerminationGracePeriod
	case !eligibleAt.Equal(r.terminationGracePeriodEligibleAt):
		return eligibleAt.Before(r.terminationGracePeriodEligibleAt)
	default:
		return condition < r.TerminationGracePeriodCondition
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
