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

	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/cloudprovider"
)

// NodeShape detects nodes that have launched with 10% or less of any resource than was expected.
type NodeShape struct {
	kubeClient client.Client
	provider   cloudprovider.CloudProvider
}

func NewNodeShape(kubeClient client.Client, provider cloudprovider.CloudProvider) Check {
	return &NodeShape{
		kubeClient: kubeClient,
		provider:   provider,
	}
}

func (n *NodeShape) Check(ctx context.Context, node *v1.Node) ([]Issue, error) {
	// ignore nodes that are deleting
	if !node.DeletionTimestamp.IsZero() {
		return nil, nil
	}
	// and nodes that haven't initialized yet
	if node.Labels[v1alpha5.LabelNodeInitialized] != "true" {
		return nil, nil
	}
	provisioner := &v1alpha5.Provisioner{}
	if err := n.kubeClient.Get(ctx, types.NamespacedName{Name: node.Labels[v1alpha5.ProvisionerNameLabelKey]}, provisioner); err != nil {
		// provisioner is missing, node should be removed soon
		return nil, client.IgnoreNotFound(err)
	}
	instanceTypes, err := n.provider.GetInstanceTypes(ctx, provisioner)
	if err != nil {
		return nil, err
	}
	instanceType, ok := lo.Find(instanceTypes, func(it *cloudprovider.InstanceType) bool { return it.Name == node.Labels[v1.LabelInstanceTypeStable] })
	if !ok {
		return []Issue{Issue(fmt.Sprintf("instance type %q not found", node.Labels[v1.LabelInstanceTypeStable]))}, nil
	}
	var issues []Issue
	for resourceName, expectedQuantity := range instanceType.Capacity {
		nodeQuantity, ok := node.Status.Capacity[resourceName]
		if !ok && !expectedQuantity.IsZero() {
			issues = append(issues, Issue(fmt.Sprintf("expected resource %s not found", resourceName)))
			continue
		}

		pct := nodeQuantity.AsApproximateFloat64() / expectedQuantity.AsApproximateFloat64()
		if pct < 0.90 {
			issues = append(issues, Issue(fmt.Sprintf("expected %s of resource %s, but found %s (%0.1f%% of expected)", expectedQuantity.String(),
				resourceName, nodeQuantity.String(), pct*100)))
		}
	}
	return issues, nil
}
