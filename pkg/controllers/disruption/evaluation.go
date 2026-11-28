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
	"github.com/samber/lo"
)

// evaluationReport is what a consolidation method reports about its current evaluation that disrupt() can't observe
// itself.
type evaluationReport struct {
	// skipped is set when nothing changed since the method last found nothing to do
	skipped          bool
	timedOut         bool
	validationFailed bool
	// evaluated is the number of candidates included in a scheduling simulation
	evaluated int
}

// takeReport returns the report for the current evaluation and clears it for the next one.
func (c *consolidation) takeReport() evaluationReport {
	report := c.report
	c.report = evaluationReport{}
	return report
}

// evaluationOutcome resolves the outcome of an evaluation from what disrupt() observed and what the method reported.
func evaluationOutcome(err error, eligible int, started, budgetBlocked bool, report evaluationReport) string {
	switch {
	case err != nil:
		return EvaluationOutcomeError.Name
	case eligible == 0:
		return EvaluationOutcomeNoCandidates.Name
	case started:
		return EvaluationOutcomeCommand.Name
	case report.skipped:
		return EvaluationOutcomeAlreadyConsolidated.Name
	case report.timedOut:
		return EvaluationOutcomeTimeout.Name
	case report.validationFailed:
		return EvaluationOutcomeValidationFailed.Name
	case budgetBlocked:
		return EvaluationOutcomeBudgetBlocked.Name
	default:
		return EvaluationOutcomeNoCommand.Name
	}
}

// recordEvaluation emits the evaluation metrics. The evaluated count is only recorded for methods that report it.
func recordEvaluation(method Method, outcome string, eligible int, cmds []Command, started bool, report evaluationReport, reported bool) {
	name := methodName(method)
	EvaluationsTotal.Inc(map[string]string{methodLabel: name, outcomeLabel: outcome})
	stages := map[string]int{
		EvaluationStageEligible.Name: eligible,
		EvaluationStageSelected.Name: lo.Ternary(started, lo.SumBy(cmds, func(cmd Command) int { return len(cmd.Candidates) }), 0),
	}
	if reported {
		stages[EvaluationStageEvaluated.Name] = report.evaluated
	}
	for stage, n := range stages {
		EvaluationCandidates.Observe(float64(n), map[string]string{methodLabel: name, stageLabel: stage})
	}
}

func methodName(method Method) string {
	switch method.(type) {
	case *Repair:
		return MethodRepair.Name
	case *Emptiness:
		return EmptyConsolidationType.Name
	case *StaticDrift:
		return MethodStaticDrift.Name
	case *Drift:
		return MethodDrift.Name
	case *MultiNodeConsolidation:
		return MultiNodeConsolidationType.Name
	case *SingleNodeConsolidation:
		return SingleNodeConsolidationType.Name
	default:
		return MethodUnknown.Name
	}
}
