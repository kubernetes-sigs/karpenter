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
)

var (
	// conditionToTaintsMap maps condition values which can be present on the node to a set of taints that should be added in response
	conditionsForCandidateTaint = sets.New(
		v1beta1.ConditionTypeDrifted,
		v1beta1.ConditionTypeExpired,
		v1beta1.ConditionTypeEmpty,
	)
)

type Controller struct {
	kubeClient client.Client
}

func NewController(kubeClient client.Client) *Controller {
	return &Controller{kubeClient: kubeClient}
}

func (c *Controller) Reconcile(ctx context.Context, nodeClaim *v1beta1.NodeClaim) (reconcile.Result, error) {
	node := &v1.Node{}
	if err := c.kubeClient.Get(ctx, types.NamespacedName{Name: nodeClaim.Status.NodeName}, node); err != nil {
		if client.IgnoreNotFound(err) == nil {
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, fmt.Errorf("getting node, %w", err)
	}
	ctx = log.IntoContext(ctx, log.FromContext(ctx).WithValues("node", node.Name))

	haveTaint := lo.ContainsBy(node.Spec.Taints, func(t v1.Taint) bool {
		return t.MatchTaint(&v1beta1.DisruptionCandidatePreferNoScheduleTaint)
	})
	needTaint := lo.ContainsBy(nodeClaim.GetConditions(), func(cond status.Condition) bool {
		return conditionsForCandidateTaint.Has(cond.Type) && cond.IsTrue()
	})

	if (haveTaint && needTaint) || (!haveTaint && !needTaint) {
		return reconcile.Result{}, nil
	}

	if needTaint {
		node.Spec.Taints = append(node.Spec.Taints, v1beta1.DisruptionCandidatePreferNoScheduleTaint)
		log.FromContext(ctx).V(1).Info("tainting disruption candidate", "taint", v1beta1.DisruptionCandidatePreferNoScheduleTaint)
	} else {
		node.Spec.Taints = lo.Reject(node.Spec.Taints, func(t v1.Taint, _ int) bool {
			return t.MatchTaint(&v1beta1.DisruptionCandidatePreferNoScheduleTaint)
		})
		log.FromContext(ctx).V(1).Info("removing taint from disruption candidate", "taint", v1beta1.DisruptionCandidatePreferNoScheduleTaint)
	}

	if err := c.kubeClient.Update(ctx, node); err != nil {
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
