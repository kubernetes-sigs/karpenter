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

package lifecycle

import (
	"context"
	"fmt"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"sigs.k8s.io/karpenter/pkg/apis/v1beta1"
	"sigs.k8s.io/karpenter/pkg/metrics"
	"sigs.k8s.io/karpenter/pkg/scheduling"
	nodeclaimutil "sigs.k8s.io/karpenter/pkg/utils/nodeclaim"
)

type Registration struct {
	kubeClient client.Client
}

func (r *Registration) Reconcile(ctx context.Context, nodeClaim *v1beta1.NodeClaim) (reconcile.Result, error) {
	if nodeClaim.StatusConditions().Get(v1beta1.ConditionTypeRegistered).IsTrue() {
		return reconcile.Result{}, nil
	}
	if !nodeClaim.StatusConditions().Get(v1beta1.ConditionTypeLaunched).IsTrue() {
		nodeClaim.StatusConditions().SetFalse(v1beta1.ConditionTypeRegistered, "NotLaunched", "Node not launched")
		return reconcile.Result{}, nil
	}
	ctx = log.IntoContext(ctx, log.FromContext(ctx).WithValues("provider-id", nodeClaim.Status.ProviderID))
	node, err := nodeclaimutil.NodeForNodeClaim(ctx, r.kubeClient, nodeClaim)
	if err != nil {
		if nodeclaimutil.IsNodeNotFoundError(err) {
			nodeClaim.StatusConditions().SetFalse(v1beta1.ConditionTypeRegistered, "NodeNotFound", "Node not registered with cluster")
			return reconcile.Result{}, nil
		}
		if nodeclaimutil.IsDuplicateNodeError(err) {
			nodeClaim.StatusConditions().SetFalse(v1beta1.ConditionTypeRegistered, "MultipleNodesFound", "Invariant violated, matched multiple nodes")
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, fmt.Errorf("getting node for nodeclaim, %w", err)
	}
	ctx = log.IntoContext(ctx, log.FromContext(ctx).WithValues("Node", klog.KRef("", node.Name)))
	if err = r.syncNode(ctx, nodeClaim, node); err != nil {
		return reconcile.Result{Requeue: true}, fmt.Errorf("sync node %w", err)
		// return reconcile.Result{}, fmt.Errorf("syncing node, %w", err)
	}
	log.FromContext(ctx).Info("registered nodeclaim")
	nodeClaim.StatusConditions().SetTrue(v1beta1.ConditionTypeRegistered)
	nodeClaim.Status.NodeName = node.Name

	metrics.NodeClaimsRegisteredCounter.With(prometheus.Labels{
		metrics.NodePoolLabel: nodeClaim.Labels[v1beta1.NodePoolLabelKey],
	}).Inc()
	metrics.NodesCreatedCounter.With(prometheus.Labels{
		metrics.NodePoolLabel: nodeClaim.Labels[v1beta1.NodePoolLabelKey],
	}).Inc()
	return reconcile.Result{}, nil
}

func (r *Registration) syncNode(ctx context.Context, nodeClaim *v1beta1.NodeClaim, node *v1.Node) error {
	stored := node.DeepCopy()
	controllerutil.AddFinalizer(node, v1beta1.TerminationFinalizer)

	node = nodeclaimutil.UpdateNodeOwnerReferences(nodeClaim, node)
	node.Labels = lo.Assign(node.Labels, nodeClaim.Labels)
	node.Annotations = lo.Assign(node.Annotations, nodeClaim.Annotations)
	// Sync all taints inside NodeClaim into the Node taints
	node.Spec.Taints = scheduling.Taints(node.Spec.Taints).Merge(nodeClaim.Spec.Taints)
	node.Spec.Taints = scheduling.Taints(node.Spec.Taints).Merge(nodeClaim.Spec.StartupTaints)
	// Remove karpenter.sh/unregistered taint
	taints, err := isolateKarpenterStartupTaint(node.Spec.Taints)
	if err != nil {
		return fmt.Errorf("missing required startup taint: %w", err)
	}
	node.Labels = lo.Assign(node.Labels, nodeClaim.Labels, map[string]string{
		v1beta1.NodeRegisteredLabelKey: "true",
	})
	node.Spec.Taints = taints
	if !equality.Semantic.DeepEqual(stored, node) {
		if err := r.kubeClient.Patch(ctx, node, client.StrategicMergeFrom(stored)); err != nil {
			return fmt.Errorf("syncing node labels, %w", err)
		}
	}
	return nil
}

func isolateKarpenterStartupTaint(ts []v1.Taint) ([]v1.Taint, error) {
	hasStartupTaint := false
	taints := []v1.Taint{}
	for _, taint := range ts {
		if taint.MatchTaint(&v1beta1.UnregisteredNoExecuteTaint) {
			hasStartupTaint = true
		} else {
			taints = append(taints, taint)
		}
	}
	if !hasStartupTaint {
		return taints, fmt.Errorf("%s not found", &v1beta1.UnregisteredNoExecuteTaint)
	}
	return taints, nil
}
