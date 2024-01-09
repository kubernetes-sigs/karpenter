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

package consistency

import (
	"context"
	"fmt"

	v1 "k8s.io/api/core/v1"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"sigs.k8s.io/karpenter/pkg/apis/v1beta1"
	"sigs.k8s.io/karpenter/pkg/controllers/disruption"
	nodeutils "sigs.k8s.io/karpenter/pkg/utils/node"
)

// Termination detects nodes that are stuck terminating and reports why.
type Termination struct {
	clk        clock.Clock
	kubeClient client.Client
}

func NewTermination(clk clock.Clock, kubeClient client.Client) Check {
	return &Termination{
		clk:        clk,
		kubeClient: kubeClient,
	}
}

func (t *Termination) Check(ctx context.Context, node *v1.Node, nodeClaim *v1beta1.NodeClaim) ([]Issue, error) {
	// we are only looking at nodes that are hung deleting
	if nodeClaim.DeletionTimestamp.IsZero() {
		return nil, nil
	}
	pdbs, err := disruption.NewPDBLimits(ctx, t.clk, t.kubeClient)
	if err != nil {
		return nil, err
	}
	pods, err := nodeutils.GetPods(ctx, t.kubeClient, node)
	if err != nil {
		return nil, err
	}
	var issues []Issue
	if pdbKey, ok := pdbs.CanEvictPods(pods); !ok {
		issues = append(issues, Issue(fmt.Sprintf("can't drain node, PDB %q is blocking evictions", pdbKey)))
	}
	return issues, nil
}
