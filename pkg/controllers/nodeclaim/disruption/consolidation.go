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
	"fmt"

	"github.com/prometheus/client_golang/prometheus"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/utils/clock"
	"knative.dev/pkg/apis"
	"knative.dev/pkg/logging"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"sigs.k8s.io/karpenter/pkg/apis/v1beta1"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/metrics"
	"sigs.k8s.io/karpenter/pkg/utils/node"
	nodeclaimutil "sigs.k8s.io/karpenter/pkg/utils/nodeclaim"
)

// Consolidation is a nodeclaim sub-controller that adds or removes status conditions on nodeclaims when using WhenUnderutilized policy.
type Consolidation struct {
	kubeClient client.Client
	cluster    *state.Cluster
	clock      clock.Clock
}

//nolint:gocyclo
func (e *Consolidation) Reconcile(ctx context.Context, nodePool *v1beta1.NodePool, nodeClaim *v1beta1.NodeClaim) (reconcile.Result, error) {
	hasCondition := nodeClaim.StatusConditions().GetCondition(v1beta1.Underutilized) != nil
	if nodePool.Spec.Disruption.ConsolidationPolicy != v1beta1.ConsolidationPolicyWhenUnderutilized {
		if hasCondition {
			_ = nodeClaim.StatusConditions().ClearCondition(v1beta1.Underutilized)
		}
		return reconcile.Result{}, nil
	}
	if initCond := nodeClaim.StatusConditions().GetCondition(v1beta1.Initialized); initCond == nil || initCond.IsFalse() {
		if hasCondition {
			_ = nodeClaim.StatusConditions().ClearCondition(v1beta1.Underutilized)
			logging.FromContext(ctx).Debugf("removing consolidated status condition, isn't initialized")
		}
		return reconcile.Result{}, nil
	}
	_, err := nodeclaimutil.NodeForNodeClaim(ctx, e.kubeClient, nodeClaim)
	if err != nil {
		if nodeclaimutil.IsDuplicateNodeError(err) || nodeclaimutil.IsNodeNotFoundError(err) {
			_ = nodeClaim.StatusConditions().ClearCondition(v1beta1.Underutilized)
			if hasCondition {
				logging.FromContext(ctx).Debugf("removing underutilized status condition, doesn't have a single node mapping")
			}
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, err
	}

	// Get the node to check utilization
	n, err := nodeclaimutil.NodeForNodeClaim(ctx, e.kubeClient, nodeClaim)
	if err != nil {
		if nodeclaimutil.IsDuplicateNodeError(err) || nodeclaimutil.IsNodeNotFoundError(err) {
			_ = nodeClaim.StatusConditions().ClearCondition(v1beta1.Underutilized)
			if hasCondition {
				logging.FromContext(ctx).Debugf("removing underutilized status condition, doesn't have a single node mapping")
			}
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, err
	}
	pods, err := node.GetPods(ctx, e.kubeClient, n)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("retrieving node pods, %w", err)
	}
	// Check the node utilization if the utilizationThreshold is specified, the node can be disruptted only if the utilization is below the threshold.
	threshold := nodePool.Spec.Disruption.UtilizationThreshold
	if threshold != nil {
		cpu, err := calculateUtilizationOfResource(n, v1.ResourceCPU, pods)
		if err != nil {
			return reconcile.Result{}, fmt.Errorf("failed to calculate CPU, %w", err)
		}
		memory, err := calculateUtilizationOfResource(n, v1.ResourceMemory, pods)
		if err != nil {
			return reconcile.Result{}, fmt.Errorf("failed to calculate memory, %w", err)
		}
		logging.FromContext(ctx).Infof("node %s, cpu: %v, ram: %v, threshold %v", nodeClaim.Name, cpu, memory, float64(*threshold/100))
		if cpu < float64(*threshold/100) && memory < float64(*threshold/100) {
			if !hasCondition {
				nodeClaim.StatusConditions().SetCondition(apis.Condition{
					Type:     v1beta1.Underutilized,
					Status:   v1.ConditionTrue,
					Severity: apis.ConditionSeverityWarning,
				})
				logging.FromContext(ctx).Debugf("marking underutilizate")
				metrics.NodeClaimsDisruptedCounter.With(prometheus.Labels{
					metrics.TypeLabel:     metrics.ConsolidationReason,
					metrics.NodePoolLabel: nodeClaim.Labels[v1beta1.NodePoolLabelKey],
				}).Inc()
			}
		} else {
			if hasCondition {
				_ = nodeClaim.StatusConditions().ClearCondition(v1beta1.Underutilized)
				logging.FromContext(ctx).Debugf("removing underutilized status condition, utilization increased")
			}
		}
	}
	return reconcile.Result{}, nil
}

// CalculateUtilizationOfResource calculates utilization of a given resource for a node.
func calculateUtilizationOfResource(node *v1.Node, resourceName v1.ResourceName, pods []*v1.Pod) (float64, error) {
	allocatable, found := node.Status.Allocatable[resourceName]
	if !found {
		return 0, fmt.Errorf("failed to get %v from %s", resourceName, node.Name)
	}
	if allocatable.MilliValue() == 0 {
		return 0, fmt.Errorf("%v is 0 at %s", resourceName, node.Name)
	}
	podsRequest := resource.MustParse("0")
	for _, pod := range pods {
		for _, container := range pod.Spec.Containers {
			if resourceValue, found := container.Resources.Requests[resourceName]; found {
				podsRequest.Add(resourceValue)
			}
		}
	}
	return float64(podsRequest.MilliValue()) / float64(allocatable.MilliValue()), nil
}
