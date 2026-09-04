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

package disruption

import (
	"context"
	"strings"
	"sync"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/utils/clock"

	"sigs.k8s.io/karpenter/pkg/metrics"
)

type evaluationObservationContextKey struct{}
type simulationStageContextKey struct{}

const (
	MultipleNodePools = "<multiple>"
	NoNodePool        = "<none>"
)

// EvaluationObservation holds ephemeral, per-pass state used only for metrics.
// Candidate identities are never exported as labels.
type EvaluationObservation struct {
	mu        sync.Mutex
	method    Method
	eligible  sets.Set[string]
	evaluated sets.Set[string]
	timedOut  bool
}

func NewEvaluationObservation(method Method, eligible []*Candidate) *EvaluationObservation {
	return &EvaluationObservation{
		method:    method,
		eligible:  candidateKeys(eligible...),
		evaluated: sets.New[string](),
	}
}

func WithEvaluationObservation(ctx context.Context, observation *EvaluationObservation) context.Context {
	return context.WithValue(ctx, evaluationObservationContextKey{}, observation)
}

func evaluationObservationFromContext(ctx context.Context) *EvaluationObservation {
	observation, _ := ctx.Value(evaluationObservationContextKey{}).(*EvaluationObservation)
	return observation
}

func WithSimulationStage(ctx context.Context, stage string) context.Context {
	return context.WithValue(ctx, simulationStageContextKey{}, stage)
}

func simulationStageFromContext(ctx context.Context) string {
	stage, _ := ctx.Value(simulationStageContextKey{}).(string)
	return stage
}

func (o *EvaluationObservation) RecordEvaluation(candidates ...*Candidate) {
	if o == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.evaluated.Insert(candidateKeys(candidates...).UnsortedList()...)
}

func (o *EvaluationObservation) ObserveTimeout() {
	if o == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.timedOut {
		return
	}
	o.timedOut = true

	evaluated := o.eligible.Intersection(o.evaluated)
	labels := o.methodLabels()
	TimeoutCandidateCount.Observe(float64(o.eligible.Len()), withLabel(labels, stateLabel, TimeoutStateEligible))
	TimeoutCandidateCount.Observe(float64(evaluated.Len()), withLabel(labels, stateLabel, TimeoutStateEvaluated))
	TimeoutCandidateCount.Observe(float64(o.eligible.Difference(evaluated).Len()), withLabel(labels, stateLabel, TimeoutStateUnevaluated))
}

func (o *EvaluationObservation) Complete(outcome string) {
	if o == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	labels := o.methodLabels()
	PassesTotal.Inc(withLabel(labels, outcomeLabel, outcome))
	CandidatesEvaluatedPerPass.Observe(float64(o.eligible.Intersection(o.evaluated).Len()), withLabel(labels, outcomeLabel, outcome))
}

func observeSimulationPodCounts(observation *EvaluationObservation, stage string, candidates []*Candidate, pending, candidate, deleting []*corev1.Pod) {
	if observation == nil {
		return
	}
	labels := observation.methodLabels()
	labels[metrics.NodePoolLabel] = candidateNodePoolScope(candidates...)
	labels[stageLabel] = stage
	all := append(append(append([]*corev1.Pod{}, pending...), candidate...), deleting...)
	for source, pods := range map[string][]*corev1.Pod{
		SimulationPodSourcePending:   pending,
		SimulationPodSourceCandidate: candidate,
		SimulationPodSourceDeleting:  deleting,
		SimulationPodSourceTotal:     all,
	} {
		SimulationPodCount.Observe(float64(podKeys(pods...).Len()), withLabel(labels, sourceLabel, source))
	}
}

func measureValidationStage(ctx context.Context, clk clock.Clock, stage string) func() {
	observation := evaluationObservationFromContext(ctx)
	if observation == nil {
		return func() {}
	}
	start := clk.Now()
	return func() {
		ValidationDurationSeconds.Observe(clk.Since(start).Seconds(), withLabel(observation.methodLabels(), stageLabel, stage))
	}
}

func (o *EvaluationObservation) methodLabels() map[string]string {
	return map[string]string{
		methodLabel:            methodName(o.method),
		metrics.ReasonLabel:    strings.ToLower(string(o.method.Reason())),
		ConsolidationTypeLabel: o.method.ConsolidationType(),
	}
}

func methodName(method Method) string {
	if named, ok := method.(interface{ MethodName() string }); ok {
		return named.MethodName()
	}
	return "unknown"
}

func candidateKeys(candidates ...*Candidate) sets.Set[string] {
	result := sets.New[string]()
	for _, candidate := range candidates {
		if candidate == nil {
			continue
		}
		if candidate.NodeClaim != nil && candidate.NodeClaim.UID != "" {
			result.Insert(string(candidate.NodeClaim.UID))
			continue
		}
		nodePool := NoNodePool
		if candidate.NodePool != nil {
			nodePool = candidate.NodePool.Name
		}
		result.Insert(nodePool + "/" + candidate.Name())
	}
	return result
}

func podKeys(pods ...*corev1.Pod) sets.Set[string] {
	result := sets.New[string]()
	for _, pod := range pods {
		if pod == nil {
			continue
		}
		if pod.UID != "" {
			result.Insert(string(pod.UID))
			continue
		}
		result.Insert(pod.Namespace + "/" + pod.Name)
	}
	return result
}

func candidateNodePoolScope(candidates ...*Candidate) string {
	nodePools := sets.New[string]()
	for _, candidate := range candidates {
		if candidate == nil || candidate.NodePool == nil || candidate.NodePool.Name == "" {
			nodePools.Insert(NoNodePool)
			continue
		}
		nodePools.Insert(candidate.NodePool.Name)
	}
	if nodePools.Len() == 0 {
		return NoNodePool
	}
	if nodePools.Len() > 1 {
		return MultipleNodePools
	}
	return nodePools.UnsortedList()[0]
}

func withLabel(labels map[string]string, key, value string) map[string]string {
	result := make(map[string]string, len(labels)+1)
	for k, v := range labels {
		result[k] = v
	}
	result[key] = value
	return result
}
