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
	"time"

	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/cloudprovider"
	"github.com/aws/karpenter-core/pkg/controllers/node"
)

// initFailureTime is the time after which we start reporting a node as having failed to initialize. This is set
// so that we should have few if any false positives.
const initFailureTime = time.Hour

// FailedInit detects nodes that fail to initialize within an hour and reports the reason for the initialization
// failure
type FailedInit struct {
	clock      clock.Clock
	kubeClient client.Client
	provider   cloudprovider.CloudProvider
}

func NewFailedInit(clk clock.Clock, kubeClient client.Client, provider cloudprovider.CloudProvider) Check {
	return &FailedInit{clock: clk, kubeClient: kubeClient, provider: provider}
}

func (f FailedInit) Check(ctx context.Context, n *v1.Node) ([]Issue, error) {
	// ignore nodes that are deleting
	if !n.DeletionTimestamp.IsZero() {
		return nil, nil
	}

	nodeAge := f.clock.Since(n.CreationTimestamp.Time)
	// n is already initialized or not old enough
	if n.Labels[v1alpha5.LabelNodeInitialized] == "true" || nodeAge < initFailureTime {
		return nil, nil
	}
	provisioner := &v1alpha5.Provisioner{}
	if err := f.kubeClient.Get(ctx, types.NamespacedName{Name: n.Labels[v1alpha5.ProvisionerNameLabelKey]}, provisioner); err != nil {
		// provisioner is missing, node should be removed soon
		return nil, client.IgnoreNotFound(err)
	}
	instanceTypes, err := f.provider.GetInstanceTypes(ctx, provisioner)
	if err != nil {
		return nil, err
	}
	instanceType, ok := lo.Find(instanceTypes, func(it *cloudprovider.InstanceType) bool { return it.Name == n.Labels[v1.LabelInstanceTypeStable] })
	if !ok {
		return []Issue{Issue(fmt.Sprintf("instance type %q not found", n.Labels[v1.LabelInstanceTypeStable]))}, nil
	}

	// detect startup taints which should be removed
	var result []Issue
	if taint, ok := node.IsStartupTaintRemoved(n, provisioner); !ok {
		result = append(result, Issue(fmt.Sprintf("startup taint %q is still on the node", formatTaint(taint))))
	}

	// and extended resources which never registered
	if resource, ok := node.IsExtendedResourceRegistered(n, instanceType); !ok {
		result = append(result, Issue(fmt.Sprintf("expected resource %q didn't register on the node", resource)))
	}

	return result, nil
}

func formatTaint(taint *v1.Taint) string {
	if taint == nil {
		return "<nil>"
	}
	if taint.Value == "" {
		return fmt.Sprintf("%s:%s", taint.Key, taint.Effect)
	}
	return fmt.Sprintf("%s=%s:%s", taint.Key, taint.Value, taint.Effect)
}
