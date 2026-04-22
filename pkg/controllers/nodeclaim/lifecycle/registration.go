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
	"strings"

	"github.com/awslabs/operatorpkg/object"
	"github.com/samber/lo"
	"go.uber.org/multierr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/events"
	"sigs.k8s.io/karpenter/pkg/metrics"
	"sigs.k8s.io/karpenter/pkg/scheduling"
	"sigs.k8s.io/karpenter/pkg/state/nodepoolhealth"
	nodeclaimutils "sigs.k8s.io/karpenter/pkg/utils/nodeclaim"
)

type Registration struct {
	kubeClient        client.Client
	recorder          events.Recorder
	npState           *nodepoolhealth.State
	registrationHooks []cloudprovider.NodeLifecycleHook
}

//nolint:gocyclo
func (r *Registration) Reconcile(ctx context.Context, nodeClaim *v1.NodeClaim) (reconcile.Result, error) {
	if cond := nodeClaim.StatusConditions().Get(v1.ConditionTypeRegistered); !cond.IsUnknown() {
		// Ensure that we always set the status condition to the latest generation
		nodeClaim.StatusConditions().Set(*cond)
		return reconcile.Result{}, nil
	}
	node, err := nodeclaimutils.NodeForNodeClaim(ctx, r.kubeClient, nodeClaim)
	if err != nil {
		if nodeclaimutils.IsNodeNotFoundError(err) {
			nodeClaim.StatusConditions().SetUnknownWithReason(v1.ConditionTypeRegistered, "NodeNotFound", "Node not registered with cluster")
			return reconcile.Result{}, nil
		}
		if nodeclaimutils.IsDuplicateNodeError(err) {
			nodeClaim.StatusConditions().SetFalse(v1.ConditionTypeRegistered, "MultipleNodesFound", "Invariant violated, matched multiple nodes")
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, fmt.Errorf("getting node for nodeclaim, %w", err)
	}
	_, hasStartupTaint := lo.Find(node.Spec.Taints, func(t corev1.Taint) bool {
		return t.MatchTaint(&v1.UnregisteredNoExecuteTaint)
	})
	// if the sync hasn't happened yet and the race protecting startup taint isn't present then log it as missing and proceed
	// if the sync has happened then the startup taint has been removed if it was present
	if _, ok := node.Labels[v1.NodeRegisteredLabelKey]; !ok && !hasStartupTaint {
		log.FromContext(ctx).Error(
			fmt.Errorf("missing taint prevents registration-related race conditions on Karpenter-managed nodes"),
			"node claim registration error",
			"taint", v1.UnregisteredTaintKey)
		r.recorder.Publish(UnregisteredTaintMissingEvent(nodeClaim))
	}
	ctx = log.IntoContext(ctx, log.FromContext(ctx).WithValues("Node", klog.KObj(node)))
	stored := node.DeepCopy()
	// Sync labels, annotations, taints, finalizer, and owner references onto the node.
	r.syncNode(nodeClaim, node)
	// Check all registration hooks before completing registration.
	// If any hook is not ready, registration is deferred and the unregistered taint remains.
	hooksResult, hookErrors := r.checkRegistrationHooks(ctx, nodeClaim)
	if lo.IsEmpty(hooksResult) && hookErrors == nil {
		// Re-sync labels after hooks complete since hooks may have mutated nodeClaim labels.
		node.Labels = lo.Assign(node.Labels, nodeClaim.Labels)
		node.Spec.Taints = lo.Reject(node.Spec.Taints, func(t corev1.Taint, _ int) bool {
			return t.MatchTaint(&v1.UnregisteredNoExecuteTaint)
		})
		node.Labels[v1.NodeRegisteredLabelKey] = "true"
	}
	if !equality.Semantic.DeepEqual(stored, node) {
		// We use client.MergeFromWithOptimisticLock because patching a list with a JSON merge patch
		// can cause races due to the fact that it fully replaces the list on a change
		if err := r.kubeClient.Patch(ctx, node, client.MergeFromWithOptions(stored, client.MergeFromWithOptimisticLock{})); err != nil {
			if errors.IsConflict(err) {
				return reconcile.Result{Requeue: true}, nil
			}
			return reconcile.Result{}, fmt.Errorf("patching node, %w", err)
		}
	}
	// If hooks were not ready, return.
	if !lo.IsEmpty(hooksResult) || hookErrors != nil {
		return hooksResult, hookErrors
	}
	log.FromContext(ctx).Info("registered nodeclaim")
	nodeClaim.StatusConditions().SetTrue(v1.ConditionTypeRegistered)
	nodeClaim.Status.NodeName = node.Name

	metrics.NodesCreatedTotal.Inc(map[string]string{
		metrics.NodePoolLabel: nodeClaim.Labels[v1.NodePoolLabelKey],
	})
	if err := r.updateNodePoolRegistrationHealth(ctx, nodeClaim); client.IgnoreNotFound(err) != nil {
		if errors.IsConflict(err) {
			return reconcile.Result{Requeue: true}, nil
		}
		return reconcile.Result{}, err
	}
	return reconcile.Result{}, nil
}

// checkRegistrationHooks evaluates all registration hooks in parallel. If any hook returns an error,
// it is returned. If any hook signals it is not ready (non-empty result), the status condition is
// updated to list all pending hooks and the shortest requeue interval is returned.
//
//nolint:gocyclo
func (r *Registration) checkRegistrationHooks(ctx context.Context, nodeClaim *v1.NodeClaim) (reconcile.Result, error) {
	if len(r.registrationHooks) == 0 {
		return reconcile.Result{}, nil
	}
	results := make([]cloudprovider.NodeLifecycleHookResult, len(r.registrationHooks))
	errs := make([]error, len(r.registrationHooks))

	workqueue.ParallelizeUntil(ctx, len(r.registrationHooks), len(r.registrationHooks), func(i int) {
		results[i], errs[i] = r.registrationHooks[i].Registered(ctx, nodeClaim)
		if errs[i] != nil {
			errs[i] = fmt.Errorf("registration hook %q failed, %w", r.registrationHooks[i].Name(), errs[i])
		}
	})

	// Collect pending and errored hook names and compute the shortest requeue interval
	var pendingHooks []string
	mergedResult := reconcile.Result{}
	for i, result := range results {
		if errs[i] != nil || !lo.IsEmpty(result) {
			pendingHooks = append(pendingHooks, r.registrationHooks[i].Name())
			if errs[i] == nil {
				if mergedResult.RequeueAfter == 0 || (result.RequeueAfter > 0 && result.RequeueAfter < mergedResult.RequeueAfter) {
					mergedResult.RequeueAfter = result.RequeueAfter
				}
				mergedResult.Requeue = mergedResult.Requeue || result.Requeue //nolint:staticcheck
			}
		}
	}

	if len(pendingHooks) > 0 {
		log.FromContext(ctx).V(1).Info("awaiting registration hooks", "hooks", pendingHooks)
		nodeClaim.StatusConditions().SetUnknownWithReason(v1.ConditionTypeRegistered, "RegistrationHookPending", strings.Join(pendingHooks, ", "))
		return mergedResult, multierr.Combine(errs...)
	}
	return reconcile.Result{}, nil
}

// updateNodePoolRegistrationHealth adds a positive value to the nodepool buffer that stores node
// registration results and sets NodeRegistrationHealthy=True on the NodePool if IsHealthy() > 0
func (r *Registration) updateNodePoolRegistrationHealth(ctx context.Context, nodeClaim *v1.NodeClaim) error {
	nodePoolName := nodeClaim.Labels[v1.NodePoolLabelKey]
	if nodePoolName != "" {
		nodePool := &v1.NodePool{}
		if err := r.kubeClient.Get(ctx, types.NamespacedName{Name: nodePoolName}, nodePool); err != nil {
			return err
		}
		if _, found := lo.Find(nodeClaim.GetOwnerReferences(), func(o metav1.OwnerReference) bool {
			return o.Kind == object.GVK(nodePool).Kind && o.UID == nodePool.UID
		}); !found {
			return nil
		}
		stored := nodePool.DeepCopy()
		if r.npState.DryRun(nodePool.UID, true).Status() == nodepoolhealth.StatusHealthy && nodePool.StatusConditions().SetTrue(v1.ConditionTypeNodeRegistrationHealthy) {
			// We use client.MergeFromWithOptimisticLock because patching a list with a JSON merge patch
			// can cause races due to the fact that it fully replaces the list on a change
			// Here, we are updating the status condition list
			if err := r.kubeClient.Status().Patch(ctx, nodePool, client.MergeFromWithOptions(stored, client.MergeFromWithOptimisticLock{})); client.IgnoreNotFound(err) != nil {
				return err
			}
		}
		r.npState.Update(nodePool.UID, true)
	}
	return nil
}

// syncNode mutates the node in memory to sync labels, annotations, taints, finalizer, and owner references
// from the NodeClaim.
func (r *Registration) syncNode(nodeClaim *v1.NodeClaim, node *corev1.Node) {
	controllerutil.AddFinalizer(node, v1.TerminationFinalizer)
	nodeclaimutils.UpdateNodeOwnerReferences(nodeClaim, node)

	// We do not sync the taints if this label is present. We instead assume that the karpenter provider
	// is managing taints. We still manage/remove the unregistered taint to signal the end of syncing.
	if value, ok := node.Labels[v1.NodeDoNotSyncTaintsLabelKey]; !ok || value != "true" {
		// Sync all taints inside NodeClaim into the Node taints
		node.Spec.Taints = scheduling.Taints(node.Spec.Taints).Merge(nodeClaim.Spec.Taints)
		node.Spec.Taints = scheduling.Taints(node.Spec.Taints).Merge(nodeClaim.Spec.StartupTaints)
	}

	node.Annotations = lo.Assign(node.Annotations, nodeClaim.Annotations)
	node.Labels = lo.Assign(node.Labels, nodeClaim.Labels)
}
