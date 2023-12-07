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

package disruption

import (
	"context"

	"k8s.io/utils/clock"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/aws/karpenter-core/pkg/controllers/provisioning"
	"github.com/aws/karpenter-core/pkg/controllers/state"
	"github.com/aws/karpenter-core/pkg/events"
	"github.com/aws/karpenter-core/pkg/metrics"
)

// UserAnnotedExpiration is a subreconciler that deletes custom marked candidates, based on node annotations.
// Expiration will respect TTLSecondsAfterEmpty
type UserAnnotedExpiration struct {
	clock       clock.Clock
	kubeClient  client.Client
	cluster     *state.Cluster
	provisioner *provisioning.Provisioner
	recorder    events.Recorder
}

func NewCustomExpiration(clk clock.Clock, kubeClient client.Client, cluster *state.Cluster, provisioner *provisioning.Provisioner, recorder events.Recorder) *Expiration {
	return &Expiration{
		clock:       clk,
		kubeClient:  kubeClient,
		cluster:     cluster,
		provisioner: provisioner,
		recorder:    recorder,
	}
}

// ShouldDisrupt is a predicate used to filter candidates
func (e *UserAnnotedExpiration) ShouldDisrupt(_ context.Context, c *Candidate) bool {
	// TODO: tasdikrahman to implement this.
	return true
}

// SortCandidates orders expired candidates by when they've expired
func (e *UserAnnotedExpiration) filterAndSortCandidates(ctx context.Context, candidates []*Candidate) ([]*Candidate, error) {
	// TODO: tasdikrahman to implement this.
	return candidates, nil
}

// ComputeCommand generates a disrpution command given candidates
func (e *UserAnnotedExpiration) ComputeCommand(ctx context.Context, candidates ...*Candidate) (Command, error) {
	// TODO: tasdikrahman to implement this.
	return Command{}, nil
}

func (e *UserAnnotedExpiration) Type() string {
	return metrics.UserAnnotedExpiration
}

func (e *UserAnnotedExpiration) ConsolidationType() string {
	return ""
}
