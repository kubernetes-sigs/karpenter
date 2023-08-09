/*
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

package deprovisioning

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"k8s.io/utils/clock"
	"knative.dev/pkg/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/cloudprovider"
	"github.com/aws/karpenter-core/pkg/controllers/provisioning"
	"github.com/aws/karpenter-core/pkg/controllers/state"
	"github.com/aws/karpenter-core/pkg/events"
)

// Validation is used to perform validation on a consolidation command.  It makes an assumption that when re-used, all
// of the commands passed to IsValid were constructed based off of the same consolidation state.  This allows it to
// skip the validation TTL for all but the first command.
type Validation struct {
	validationPeriod time.Duration
	start            time.Time
	clock            clock.Clock
	cluster          *state.Cluster
	kubeClient       client.Client
	cloudProvider    cloudprovider.CloudProvider
	provisioner      *provisioning.Provisioner
	once             sync.Once
	recorder         events.Recorder
	// validationCandidates are the cached validation candidates.  We capture these when validating the first command and reuse them for
	// validating subsequent commands.
	validationCandidates []*Candidate
}

func NewValidation(validationPeriod time.Duration, clk clock.Clock, cluster *state.Cluster, kubeClient client.Client, provisioner *provisioning.Provisioner, cp cloudprovider.CloudProvider, recorder events.Recorder) *Validation {
	return &Validation{
		validationPeriod: validationPeriod,
		clock:            clk,
		cluster:          cluster,
		kubeClient:       kubeClient,
		provisioner:      provisioner,
		cloudProvider:    cp,
		recorder:         recorder,
	}
}

func (v *Validation) IsValid(ctx context.Context, cmd Command) (bool, error) {
	var err error
	v.once.Do(func() {
		v.start = v.clock.Now()
	})

	waitDuration := v.validationPeriod - v.clock.Since(v.start)
	if waitDuration > 0 {
		select {
		case <-ctx.Done():
			return false, errors.New("context canceled")
		case <-v.clock.After(waitDuration):
		}
	}
	if len(v.validationCandidates) == 0 {
		v.validationCandidates, err = GetCandidates(ctx, v.cluster, v.kubeClient, v.recorder, v.clock, v.cloudProvider, v.ShouldDeprovision)
		if err != nil {
			return false, fmt.Errorf("constructing validation candidates, %w", err)
		}
	}
	nodes, err := filterCandidates(ctx, v.kubeClient, v.recorder, cmd.candidates)
	if err != nil {
		return false, fmt.Errorf("filtering candidates, %w", err)
	}
	// If we filtered out any candidates, return false as some nodes in the consolidation decision have changed.
	if len(nodes) != len(cmd.candidates) {
		return false, nil
	}
	// a candidate we are about to delete is a target of a currently pending pod, wait for that to settle
	// before continuing consolidation
	for _, n := range cmd.candidates {
		if v.cluster.IsNodeNominated(n.Name()) {
			return false, nil
		}
	}

	isValid, err := v.ValidateCommand(ctx, cmd, v.validationCandidates)
	if err != nil {
		return false, fmt.Errorf("validating command, %w", err)
	}

	return isValid, nil
}

// ShouldDeprovision is a predicate used to filter deprovisionable nodes
func (v *Validation) ShouldDeprovision(_ context.Context, c *Candidate) bool {
	if val, ok := c.Annotations()[v1alpha5.DoNotConsolidateNodeAnnotationKey]; ok {
		return val != "true"
	}
	return c.provisioner != nil && c.provisioner.Spec.Consolidation != nil && ptr.BoolValue(c.provisioner.Spec.Consolidation.Enabled)
}

// ValidateCommand validates a command for a deprovisioner
func (v *Validation) ValidateCommand(ctx context.Context, cmd Command, candidates []*Candidate) (bool, error) {
	// map from candidates we are about to remove back into candidates with cluster state
	candidates = mapCandidates(cmd.candidates, candidates)

	// None of the chosen candidate are valid for execution, so retry
	if len(candidates) == 0 {
		return false, nil
	}

	results, err := simulateScheduling(ctx, v.kubeClient, v.cluster, v.provisioner, candidates...)
	if err != nil {
		return false, fmt.Errorf("simluating scheduling, %w", err)
	}
	if !results.AllNonPendingPodsScheduled() {
		return false, nil
	}

	// We want to ensure that the re-simulated scheduling using the current cluster state produces the same result.
	// There are three possible options for the number of new candidates that we need to handle:
	// len(newMachines) == 0, as long as we weren't expecting a new machine, this is valid
	// len(newMachines) > 1, something in the cluster changed so that the candidates we were going to delete can no longer
	//                    be deleted without producing more than one machine
	// len(newMachines) == 1, as long as the machine looks like what we were expecting, this is valid
	if len(results.NewMachines) == 0 {
		if len(cmd.replacements) == 0 {
			// scheduling produced zero new machines and we weren't expecting any, so this is valid.
			return true, nil
		}
		// if it produced no new machines, but we were expecting one we should re-simulate as there is likely a better
		// consolidation option now
		return false, nil
	}

	// we need more than one replacement machine which is never valid currently (all of our node replacement is m->1, never m->n)
	if len(results.NewMachines) > 1 {
		return false, nil
	}

	// we now know that scheduling simulation wants to create one new machine
	if len(cmd.replacements) == 0 {
		// but we weren't expecting any new nodes, so this is invalid
		return false, nil
	}

	// We know that the scheduling simulation wants to create a new machine and that the command we are verifying wants
	// to create a new machine. The scheduling simulation doesn't apply any filtering to instance types, so it may include
	// instance types that we don't want to launch which were filtered out when the lifecycleCommand was created.  To
	// check if our lifecycleCommand is valid, we just want to ensure that the list of instance types we are considering
	// creating are a subset of what scheduling says we should create.  We check for a subset since the scheduling
	// simulation here does no price filtering, so it will include more expensive types.
	//
	// This is necessary since consolidation only wants cheaper machines.  Suppose consolidation determined we should delete
	// a 4xlarge and replace it with a 2xlarge. If things have changed and the scheduling simulation we just performed
	// now says that we need to launch a 4xlarge. It's still launching the correct number of machines, but it's just
	// as expensive or possibly more so we shouldn't validate.
	if !instanceTypesAreSubset(cmd.replacements[0].InstanceTypeOptions, results.NewMachines[0].InstanceTypeOptions) {
		return false, nil
	}

	// Now we know:
	// - current scheduling simulation says to create a new machine with types T = {T_0, T_1, ..., T_n}
	// - our lifecycle command says to create a machine with types {U_0, U_1, ..., U_n} where U is a subset of T
	return true, nil
}
