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

package inflightchecks

import (
	"context"
	"fmt"

	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"

	"github.com/aws/karpenter-core/pkg/apis/provisioning/v1alpha5"
	"github.com/aws/karpenter-core/pkg/cloudprovider"
	"github.com/aws/karpenter-core/pkg/controllers/deprovisioning"
)

// NodeShape detects nodes that have launched with 10% or less of any resource than was expected.
type NodeShape struct {
	provider cloudprovider.CloudProvider
}

func NewNodeShape(provider cloudprovider.CloudProvider) Check {
	return &NodeShape{
		provider: provider,
	}
}

func (n *NodeShape) Check(ctx context.Context, node *v1.Node, provisioner *v1alpha5.Provisioner, pdbs *deprovisioning.PDBLimits) ([]Issue, error) {
	// ignore nodes that are deleting
	if !node.DeletionTimestamp.IsZero() {
		return nil, nil
	}
	// and nodes that haven't initialized yet
	if node.Labels[v1alpha5.LabelNodeInitialized] != "true" {
		return nil, nil
	}

	instanceTypes, err := n.provider.GetInstanceTypes(ctx, provisioner)
	if err != nil {
		return nil, err
	}

	instanceType, ok := lo.Find(instanceTypes, func(it cloudprovider.InstanceType) bool { return it.Name() == node.Labels[v1.LabelInstanceType] })
	if !ok {
		return []Issue{{
			node:    node,
			message: fmt.Sprintf("Instance Type %q not found", node.Labels[v1.LabelInstanceType]),
		}}, nil
	}
	var issues []Issue
	for resourceName, expectedQuantity := range instanceType.Resources() {
		nodeQuantity, ok := node.Status.Capacity[resourceName]
		if !ok && !expectedQuantity.IsZero() {
			issues = append(issues, Issue{
				node:    node,
				message: fmt.Sprintf("Expected resource %s not found", resourceName),
			})
			continue
		}

		pct := nodeQuantity.AsApproximateFloat64() / expectedQuantity.AsApproximateFloat64()
		if pct < 0.90 {
			issues = append(issues, Issue{
				node: node,
				message: fmt.Sprintf("Expected %s of resource %s, but found %s (%0.1f%% of expected)", expectedQuantity.String(),
					resourceName, nodeQuantity.String(), pct*100),
			})
		}

	}
	return issues, nil
}
