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
	"errors"
	"fmt"

	"github.com/awslabs/operatorpkg/option"
	"github.com/samber/lo"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/controllers/provisioning"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/events"
)

type ValidatorOptions struct {
	atomic bool
}

func WithAtomic() option.Function[ValidatorOptions] {
	return func(o *ValidatorOptions) {
		o.atomic = true
	}
}

type ValidationError struct {
	error
}

func NewValidationError(err error) *ValidationError {
	return &ValidationError{error: err}
}

func IsValidationError(err error) bool {
	if err == nil {
		return false
	}
	var validationError *ValidationError
	return errors.As(err, &validationError)
}

// Validation is used to perform validation on a consolidation command.  It makes an assumption that when re-used, all
// of the commands passed to IsValid were constructed based off of the same consolidation state.  This allows it to
// skip the validation TTL for all but the first command.
type validation struct {
	clock         clock.Clock
	cluster       *state.Cluster
	kubeClient    client.Client
	cloudProvider cloudprovider.CloudProvider
	provisioner   *provisioning.Provisioner
	recorder      events.Recorder
	queue         *Queue
	reason        v1.DisruptionReason
}

type Validator struct {
	validation
	filter         CandidateFilter
	validationType string
}

func NewEmptinessValidator(c consolidation) *Validator {
	e := &Emptiness{consolidation: c}
	return &Validator{
		validation: validation{
			clock:         c.clock,
			cluster:       c.cluster,
			kubeClient:    c.kubeClient,
			provisioner:   c.provisioner,
			cloudProvider: c.cloudProvider,
			recorder:      c.recorder,
			queue:         c.queue,
			reason:        v1.DisruptionReasonEmpty,
		},
		filter:         e.ShouldDisrupt,
		validationType: e.ConsolidationType(),
	}
}

func NewSingleConsolidationValidator(c consolidation) *Validator {
	s := &SingleNodeConsolidation{consolidation: c}
	return &Validator{
		validation: validation{
			clock:         c.clock,
			cluster:       c.cluster,
			kubeClient:    c.kubeClient,
			provisioner:   c.provisioner,
			cloudProvider: c.cloudProvider,
			recorder:      c.recorder,
			queue:         c.queue,
			reason:        v1.DisruptionReasonUnderutilized,
		},
		filter:         s.ShouldDisrupt,
		validationType: s.ConsolidationType(),
	}
}

func NewMultiConsolidationValidator(c consolidation) *Validator {
	m := &MultiNodeConsolidation{consolidation: c}
	return &Validator{
		validation: validation{
			clock:         c.clock,
			cluster:       c.cluster,
			kubeClient:    c.kubeClient,
			provisioner:   c.provisioner,
			cloudProvider: c.cloudProvider,
			recorder:      c.recorder,
			queue:         c.queue,
			reason:        v1.DisruptionReasonUnderutilized,
		},
		filter:         m.ShouldDisrupt,
		validationType: m.ConsolidationType(),
	}
}

//nolint:gocyclo
func (v *Validator) ValidateCandidates(ctx context.Context, candidates []*Candidate, opts ...option.Function[ValidatorOptions]) ([]*Candidate, error) {
	o := option.Resolve(opts...)

	// This GetCandidates call filters out nodes that were nominated
	// GracefulDisruptionClass is hardcoded here because ValidateCandidates is only used for consolidation disruption. All consolidation disruption is graceful disruption.
	validatedCandidates, err := GetCandidates(ctx, v.cluster, v.kubeClient, v.recorder, v.clock, v.cloudProvider, v.filter, GracefulDisruptionClass, v.queue)
	if err != nil {
		return nil, fmt.Errorf("constructing validation candidates, %w", err)
	}
	validatedCandidates = mapCandidates(candidates, validatedCandidates)

	// If we are acting atomically and filtered out any candidates, return nil as some NodeClaims in the consolidation decision have changed.
	if len(validatedCandidates) == 0 || (o.atomic && len(validatedCandidates) != len(candidates)) {
		FailedValidationsTotal.Add(float64(len(candidates)), map[string]string{ConsolidationTypeLabel: v.validationType})
		return nil, NewValidationError(fmt.Errorf("%d candidates are no longer valid", len(candidates)-len(validatedCandidates)))
	}
	disruptionBudgetMapping, err := BuildDisruptionBudgetMapping(ctx, v.cluster, v.clock, v.kubeClient, v.cloudProvider, v.recorder, v.reason)
	if err != nil {
		return nil, fmt.Errorf("building disruption budgets, %w", err)
	}

	// We consider any candidate invalid if it meets either of the following conditions:
	//  a. A pod was nominated to the candidate
	//  b. Disrupting the candidate would violate node disruption budgets
	// If we are acting atomically and 1 candidate is deemed invalid, we return nil.
	var validCandidates []*Candidate
	for _, vc := range validatedCandidates {
		if v.cluster.IsNodeNominated(vc.ProviderID()) {
			if o.atomic {
				err = NewValidationError(fmt.Errorf("a candidate was nominated during validation"))
				validCandidates = []*Candidate{}
				break
			}
			continue
		}
		if disruptionBudgetMapping[vc.NodePool.Name] == 0 {
			if o.atomic {
				err = NewValidationError(fmt.Errorf("a candidate can no longer be disrupted without violating budgets"))
				validCandidates = []*Candidate{}
				break
			}
			continue
		}
		disruptionBudgetMapping[vc.NodePool.Name]--
		validCandidates = append(validCandidates, vc)
	}
	if !o.atomic && len(validCandidates) == 0 {
		err = NewValidationError(fmt.Errorf("%d candidates failed validation because it they were nominated for a pod or would violate disruption budgets", len(candidates)))
	}
	FailedValidationsTotal.Add(
		lo.Ternary(o.atomic, float64(len(candidates)), float64(len(candidates)-len(validCandidates))), map[string]string{ConsolidationTypeLabel: v.validationType},
	)
	return lo.Ternary(err == nil, validCandidates, nil), err
}
