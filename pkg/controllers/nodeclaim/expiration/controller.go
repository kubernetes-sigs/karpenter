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

package expiration

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/awslabs/operatorpkg/serrors"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/klog/v2"
	"k8s.io/utils/clock"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"sigs.k8s.io/karpenter/pkg/operator/injection"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/metrics"
	nodeutils "sigs.k8s.io/karpenter/pkg/utils/node"
	nodeclaimutils "sigs.k8s.io/karpenter/pkg/utils/nodeclaim"
)

// Expiration is a nodeclaim controller that deletes expired nodeclaims based on expireAfter
type Controller struct {
	clock         clock.Clock
	kubeClient    client.Client
	cloudProvider cloudprovider.CloudProvider
}

// deferredExpirationRequeueAfter bounds how long an expired NodeClaim can remain without a NodePool event after
// this controller defers to budget-paced disruption. In particular, it lets us recover if the owning NodePool is
// deleted after the initial deferral.
const deferredExpirationRequeueAfter = time.Minute

// NewController constructs a nodeclaim disruption controller
func NewController(clk clock.Clock, kubeClient client.Client, cloudProvider cloudprovider.CloudProvider) *Controller {
	return &Controller{
		clock:         clk,
		kubeClient:    kubeClient,
		cloudProvider: cloudProvider,
	}
}

func (c *Controller) Reconcile(ctx context.Context, nodeClaim *v1.NodeClaim) (reconcile.Result, error) {
	ctx = injection.WithControllerName(ctx, c.Name())
	if nodeClaim.Status.NodeName != "" {
		ctx = log.IntoContext(ctx, log.FromContext(ctx).WithValues("Node", klog.KRef("", nodeClaim.Status.NodeName)))
	}

	if !nodeclaimutils.IsManaged(nodeClaim, c.cloudProvider) {
		return reconcile.Result{}, nil
	}
	if !nodeClaim.DeletionTimestamp.IsZero() {
		return reconcile.Result{}, nil
	}
	// From here there are four scenarios to handle:
	// 1. If ExpireAfter is not configured, exit expiration loop
	expirationTime, ok := nodeclaimutils.ExpirationTime(nodeClaim)
	if !ok {
		return reconcile.Result{}, nil
	}
	// 2. If the NodeClaim isn't expired leave the reconcile loop.
	if c.clock.Now().Before(expirationTime) {
		// Use t.Sub(clock.Now()) instead of time.Until() to ensure we're using the injected clock.
		return reconcile.Result{RequeueAfter: expirationTime.Sub(c.clock.Now())}, nil
	}
	// 3. Expiration of a NodeClaim owned by a dynamic NodePool is a voluntary disruption, paced by that NodePool's
	// disruption budgets under the Expired reason (see pkg/controllers/disruption/expiration.go). This controller must
	// stand down for those NodeClaims, or its unconditional delete would race the budget and force the node out at its
	// deadline regardless.
	forceful, err := c.expiresForcefully(ctx, nodeClaim)
	if err != nil {
		return reconcile.Result{}, err
	}
	if !forceful {
		return reconcile.Result{RequeueAfter: deferredExpirationRequeueAfter}, nil
	}
	// 4. Otherwise, if the NodeClaim is expired we can forcefully expire the nodeclaim (by deleting it)
	if err := c.kubeClient.Delete(ctx, nodeClaim); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	// 5. The deletion timestamp has successfully been set for the NodeClaim, update relevant metrics.
	log.FromContext(ctx).V(1).Info("deleting expired nodeclaim")
	labels := map[string]string{
		metrics.ReasonLabel:              strings.ToLower(metrics.ExpiredReason),
		metrics.NodePoolLabel:            nodeClaim.Labels[v1.NodePoolLabelKey],
		metrics.CapacityTypeLabel:        nodeClaim.Labels[v1.CapacityTypeLabelKey],
		metrics.ConsolidationPolicyLabel: "",
		metrics.TerminationModeLabel:     nodeclaimutils.DisruptionTerminationMode(nodeClaim),
	}
	metrics.NodeClaimsDisruptedTotal.Inc(labels)
	// Pods that haven't started draining yet still appear bound to the node, so the
	// list call here captures the pre-disruption state. Errors are logged but do not
	// fail the reconcile — the metric just reports 0 for this disruption event.
	reschedulablePods, err := nodeutils.ReschedulablePods(ctx, c.kubeClient, nodeClaim.Status.NodeName)
	if err != nil {
		log.FromContext(ctx).V(1).Info("listing reschedulable pods for disruption metric", "error", err.Error())
	}
	metrics.PodsDisruptionInitiatedTotal.Add(float64(len(reschedulablePods)), labels)
	// We sleep here after the delete operation since we want to ensure that we are able to read our own writes so that
	// we avoid duplicating metrics and log lines due to quick re-queues.
	// USE CAUTION when determining whether to increase this timeout or remove this line
	c.clock.Sleep(time.Second)
	return reconcile.Result{}, nil
}

// expiresForcefully reports whether this NodeClaim's expiration is still this controller's to perform, rather than the
// budget-paced Expired disruption method's. Two cases keep the forceful path, because neither can be paced by a
// NodePool's disruption budget:
//
//   - A standalone NodeClaim, which no NodePool owns, so there is no budget to evaluate and the disruption controller
//     never builds a candidate for it.
//   - A NodeClaim owned by a static NodePool is handled by the disruption controller's StaticExpiration method,
//     which stages a replacement while respecting replica and node limits.
func (c *Controller) expiresForcefully(ctx context.Context, nodeClaim *v1.NodeClaim) (bool, error) {
	nodePoolName, ok := nodeClaim.Labels[v1.NodePoolLabelKey]
	if !ok {
		return true, nil
	}
	nodePool := &v1.NodePool{}
	if err := c.kubeClient.Get(ctx, client.ObjectKey{Name: nodePoolName}, nodePool); err != nil {
		// A missing NodePool leaves nothing to pace against, so fall back to forceful expiration rather than leaving
		// the NodeClaim to outlive its expireAfter indefinitely.
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, serrors.Wrap(fmt.Errorf("getting nodepool, %w", err), "NodePool", klog.KRef("", nodePoolName))
	}
	return false, nil
}

func (c *Controller) Name() string {
	return "nodeclaim.expiration"
}

func (c *Controller) Register(_ context.Context, m manager.Manager) error {
	return controllerruntime.NewControllerManagedBy(m).
		Named(c.Name()).
		For(&v1.NodeClaim{}, builder.WithPredicates(nodeclaimutils.IsManagedPredicateFuncs(c.cloudProvider))).
		Complete(reconcile.AsReconciler(m.GetClient(), c))
}
