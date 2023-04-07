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

package consistency

import (
	"context"
	"fmt"

	v1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/aws/karpenter-core/pkg/controllers/deprovisioning"
	nodeutils "github.com/aws/karpenter-core/pkg/utils/node"
)

// Termination detects nodes that are stuck terminating and reports why.
type Termination struct {
	kubeClient client.Client
}

func NewTermination(kubeClient client.Client) Check {
	return &Termination{
		kubeClient: kubeClient,
	}
}

func (t *Termination) Check(ctx context.Context, node *v1.Node) ([]Issue, error) {
	// we are only looking at nodes that are hung deleting
	if node.DeletionTimestamp.IsZero() {
		return nil, nil
	}
	pdbs, err := deprovisioning.NewPDBLimits(ctx, t.kubeClient)
	if err != nil {
		return nil, err
	}
	pods, err := nodeutils.GetNodePods(ctx, t.kubeClient, node)
	if err != nil {
		return nil, err
	}
	var issues []Issue
	if pdb, ok := pdbs.CanEvictPods(pods); !ok {
		issues = append(issues, Issue(fmt.Sprintf("can't drain node, PDB %s is blocking evictions", pdb)))
	}
	return issues, nil
}
