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

package tainting

import (
	"context"
	"fmt"
	golog "log"

	"github.com/awslabs/operatorpkg/singleton"
	"github.com/awslabs/operatorpkg/status"
	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"sigs.k8s.io/karpenter/pkg/apis/v1beta1"

	nodeutils "sigs.k8s.io/karpenter/pkg/utils/node"
)

var (
	// conditionToTaintsMap maps condition values which can be present on the node to a set of taints that should be added in response
	conditionToTaintsMap = map[string]v1.Taint{
		v1beta1.ConditionTypeDrifted: v1beta1.DisruptionCandidatePreferNoScheduleTaint,
		v1beta1.ConditionTypeExpired: v1beta1.DisruptionCandidatePreferNoScheduleTaint,
		v1beta1.ConditionTypeEmpty:   v1beta1.DisruptionCandidatePreferNoScheduleTaint,
	}

	// managedTaints is the string representation of all taints managed by the tainting controller
	managedTaints = sets.New(lo.Uniq(lo.MapToSlice(conditionToTaintsMap, func(_ string, taint v1.Taint) string {
		return taint.ToString()
	}))...)
)

func init() {
	// Assert that no managed taint has a key-value collision. The tainting controller relies on this invariant, change with caution.
	taintsPerKeyEffect := lo.GroupBy(lo.UniqBy(lo.Values(conditionToTaintsMap), func(taint v1.Taint) string {
		return taint.ToString()
	}), func(taint v1.Taint) string {
		// Group all taints with the same key + effect together
		taint.Value = ""
		return taint.ToString()
	})
	for _, taints := range taintsPerKeyEffect {
		if len(taints) != 1 {
			golog.Fatalf("taints managed by the tainting controller must only have one value per key-effect pair")
		}
	}
}

type Controller struct {
	kubeClient client.Client
}

func NewController(kubeClient client.Client) *Controller {
	return &Controller{
		kubeClient: kubeClient,
	}
}

func (c *Controller) Reconcile(ctx context.Context, nodeClaim *v1beta1.NodeClaim) (reconcile.Result, error) {
	node := &v1.Node{}
	if err := c.kubeClient.Get(ctx, types.NamespacedName{Name: nodeClaim.Status.NodeName}, node); err != nil {
		if client.IgnoreNotFound(err) == nil {
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, fmt.Errorf("getting node, %w", err)
	}
	stored := node.DeepCopy()
	ctx = log.IntoContext(ctx, log.FromContext(ctx).WithValues("node", node.Name))

	taintsForConditions := lo.SliceToMap(lo.FilterMap(nodeClaim.GetConditions(), func(cond status.Condition, _ int) (v1.Taint, bool) {
		if taint, ok := conditionToTaintsMap[cond.Type]; ok {
			return taint, cond.IsTrue()
		}
		return v1.Taint{}, false
	}), func(taint v1.Taint) (string, v1.Taint) {
		return taint.ToString(), taint
	})
	currentTaints := sets.New(lo.Map(node.Spec.Taints, func(taint v1.Taint, _ int) string {
		return taint.ToString()
	})...)
	toRemove := currentTaints.Intersection(managedTaints.Difference(sets.New(lo.Keys(taintsForConditions)...)))
	toAdd := sets.New(lo.Keys(taintsForConditions)...)
	toAdd = toAdd.Difference(currentTaints.Intersection(toAdd))

	if toAdd.Len() == 0 && toRemove.Len() == 0 {
		return reconcile.Result{}, nil
	}

	node.Spec.Taints = lo.Reject(node.Spec.Taints, func(taint v1.Taint, _ int) bool {
		return toRemove.Has(taint.ToString())
	})
	node.Spec.Taints = append(node.Spec.Taints, lo.Values(lo.PickByKeys(taintsForConditions, toAdd.UnsortedList()))...)

	if len(toAdd) != 0 {
		log.FromContext(ctx).V(1).Info("adding taints for status conditions", "taints", toAdd.UnsortedList())
	}
	if len(toRemove) != 0 {
		log.FromContext(ctx).V(1).Info("removing taints for status conditions", "taints", toRemove.UnsortedList())
	}
	if err := nodeutils.PatchTaints(ctx, c.kubeClient, stored, node); err != nil {
		if errors.IsConflict(err) {
			return reconcile.Result{RequeueAfter: singleton.RequeueImmediately}, nil
		}
		return reconcile.Result{}, fmt.Errorf("updating taints for status conditions, %w", err)
	}
	return reconcile.Result{}, nil
}

func (c *Controller) Register(_ context.Context, m manager.Manager) error {
	return controllerruntime.NewControllerManagedBy(m).
		Named("node.tainting").
		For(&v1beta1.NodeClaim{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: 10}).
		Complete(reconcile.AsReconciler(m.GetClient(), c))
}
