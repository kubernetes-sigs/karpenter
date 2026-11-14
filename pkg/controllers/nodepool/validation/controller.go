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

package validation

import (
	"context"
	"fmt"
	"strings"

	"github.com/awslabs/operatorpkg/status"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/utils/clock"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/events"
	"sigs.k8s.io/karpenter/pkg/operator/injection"
	nodepoolutils "sigs.k8s.io/karpenter/pkg/utils/nodepool"
)

// Controller for the resource
type Controller struct {
	kubeClient    client.Client
	cloudProvider cloudprovider.CloudProvider
	clock         clock.Clock
	recorder      events.Recorder
}

// NewController is a constructor
func NewController(clk clock.Clock, kubeClient client.Client, cloudProvider cloudprovider.CloudProvider, recorder events.Recorder) *Controller {
	return &Controller{
		kubeClient:    kubeClient,
		cloudProvider: cloudProvider,
		clock:         clk,
		recorder:      recorder,
	}
}

func (c *Controller) Name() string {
	return "nodepool.validation"
}

func (c *Controller) Reconcile(ctx context.Context, nodePool *v1.NodePool) (reconcile.Result, error) {
	ctx = injection.WithControllerName(ctx, c.Name())
	if !nodepoolutils.IsManaged(nodePool, c.cloudProvider) {
		return reconcile.Result{}, nil
	}
	stored := nodePool.DeepCopy()
	if err := nodePool.RuntimeValidate(ctx); err != nil {
		nodePool.StatusConditions(status.WithClock(c.clock)).SetFalse(v1.ConditionTypeValidationSucceeded, "NodePoolValidationFailed", err.Error())
	} else {
		nodePool.StatusConditions(status.WithClock(c.clock)).SetTrue(v1.ConditionTypeValidationSucceeded)
	}
	c.warnOnShortBudgetWindows(nodePool)
	if !equality.Semantic.DeepEqual(stored, nodePool) {
		// We use client.MergeFromWithOptimisticLock because patching a list with a JSON merge patch
		// can cause races due to the fact that it fully replaces the list on a change
		// Here, we are updating the status condition list
		if e := c.kubeClient.Status().Patch(ctx, nodePool, client.MergeFromWithOptions(stored, client.MergeFromWithOptimisticLock{})); client.IgnoreNotFound(e) != nil {
			if errors.IsConflict(e) {
				return reconcile.Result{Requeue: true}, nil
			}
			return reconcile.Result{}, e
		}
	}
	return reconcile.Result{}, nil
}

func (c *Controller) warnOnShortBudgetWindows(nodePool *v1.NodePool) {
	tgp := nodePool.Spec.Template.Spec.TerminationGracePeriod
	if tgp == nil {
		return
	}
	for _, budget := range nodePool.Spec.Disruption.Budgets {
		if budget.Schedule == nil || budget.Duration == nil || budget.Duration.Duration >= tgp.Duration || strings.TrimSuffix(budget.Nodes, "%") == "0" {
			continue
		}
		c.recorder.Publish(events.Event{
			InvolvedObject: nodePool,
			Type:           corev1.EventTypeWarning,
			Reason:         events.BudgetWindowShorterThanTerminationGracePeriod,
			Message:        fmt.Sprintf("Budget with schedule %q and duration %s is shorter than terminationGracePeriod %s, so nodes disrupted in the window can keep draining after it ends", *budget.Schedule, budget.Duration.Duration, tgp.Duration),
			DedupeValues:   []string{string(nodePool.UID), *budget.Schedule, budget.Duration.Duration.String()},
		})
	}
}

func (c *Controller) Register(_ context.Context, m manager.Manager) error {
	return controllerruntime.NewControllerManagedBy(m).
		Named(c.Name()).
		For(&v1.NodePool{}, builder.WithPredicates(nodepoolutils.IsManagedPredicateFuncs(c.cloudProvider))).
		WithOptions(controller.Options{MaxConcurrentReconciles: 10}).
		Complete(reconcile.AsReconciler(m.GetClient(), c))
}
