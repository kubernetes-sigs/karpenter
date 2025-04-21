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

package termination

import (
	"context"
	"fmt"
	"time"

	"github.com/awslabs/operatorpkg/serrors"
	"github.com/samber/lo"
	"golang.org/x/time/rate"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	"k8s.io/utils/clock"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"sigs.k8s.io/karpenter/pkg/utils/pretty"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/controllers/node/termination/terminator"
	terminatorevents "sigs.k8s.io/karpenter/pkg/controllers/node/termination/terminator/events"
	"sigs.k8s.io/karpenter/pkg/events"
	"sigs.k8s.io/karpenter/pkg/metrics"
	"sigs.k8s.io/karpenter/pkg/operator/injection"
	nodeutils "sigs.k8s.io/karpenter/pkg/utils/node"
	"sigs.k8s.io/karpenter/pkg/utils/pod"
	"sigs.k8s.io/karpenter/pkg/utils/termination"
	volumeutil "sigs.k8s.io/karpenter/pkg/utils/volume"
)

// Controller for the resource
type Controller struct {
	clock         clock.Clock
	kubeClient    client.Client
	cloudProvider cloudprovider.CloudProvider
	terminator    *terminator.Terminator
	recorder      events.Recorder
}

// NewController constructs a controller instance
func NewController(clk clock.Clock, kubeClient client.Client, cloudProvider cloudprovider.CloudProvider, terminator *terminator.Terminator, recorder events.Recorder) *Controller {
	return &Controller{
		clock:         clk,
		kubeClient:    kubeClient,
		cloudProvider: cloudProvider,
		terminator:    terminator,
		recorder:      recorder,
	}
}

func (c *Controller) Reconcile(ctx context.Context, n *corev1.Node) (reconcile.Result, error) {
	ctx = injection.WithControllerName(ctx, "node.termination")
	ctx = log.IntoContext(ctx, log.FromContext(ctx).WithValues("Node", klog.KRef(n.Namespace, n.Name)))

	if !n.GetDeletionTimestamp().IsZero() {
		return c.finalize(ctx, n)
	}
	return reconcile.Result{}, nil
}

//nolint:gocyclo
func (c *Controller) finalize(ctx context.Context, node *corev1.Node) (reconcile.Result, error) {
	if !controllerutil.ContainsFinalizer(node, v1.TerminationFinalizer) {
		return reconcile.Result{}, nil
	}
	if !nodeutils.IsManaged(node, c.cloudProvider) {
		return reconcile.Result{}, nil
	}

	nodeClaim, err := nodeutils.NodeClaimForNode(ctx, c.kubeClient, node)
	if err != nil {
		// This should not occur. The NodeClaim is required to track details about the termination stage and termination grace
		// period and will not be finalized until after the Node has been terminated by Karpenter. If there are duplicates or
		// the nodeclaim does not exist, this indicates a customer induced error (e.g. removing finalizers or manually
		// creating nodeclaims with matching provider IDs).
		if nodeutils.IsDuplicateNodeClaimError(err) || nodeutils.IsNodeClaimNotFoundError(err) {
			return reconcile.Result{}, c.associatedNodeClaimError(err)
		}
		return reconcile.Result{}, err
	}
	ctx = log.IntoContext(ctx, log.FromContext(ctx).WithValues("NodeClaim", klog.KRef(nodeClaim.Namespace, nodeClaim.Name)))
	if nodeClaim.DeletionTimestamp.IsZero() {
		if err := c.kubeClient.Delete(ctx, nodeClaim); err != nil {
			if errors.IsNotFound(err) {
				return reconcile.Result{}, c.associatedNodeClaimError(err)
			}
			return reconcile.Result{}, fmt.Errorf("deleting nodeclaim, %w", err)
		}
	}

	// If the underlying instance no longer exists, we want to delete to avoid trying to gracefully draining the
	// associated node. We do a check on the Ready condition of the node since, even though the CloudProvider says the
	// instance is not around, we know that the kubelet process is still running if the Node Ready condition is true.
	// Similar logic to: https://github.com/kubernetes/kubernetes/blob/3a75a8c8d9e6a1ebd98d8572132e675d4980f184/staging/src/k8s.io/cloud-provider/controllers/nodelifecycle/node_lifecycle_controller.go#L144
	if nodeutils.GetCondition(node, corev1.NodeReady).Status != corev1.ConditionTrue {
		if _, err = c.cloudProvider.Get(ctx, node.Spec.ProviderID); err != nil {
			if cloudprovider.IsNodeClaimNotFoundError(err) {
				return reconcile.Result{}, c.removeFinalizer(ctx, node)
			}
			return reconcile.Result{}, fmt.Errorf("getting nodeclaim, %w", err)
		}
	}

	nodeTerminationTime, err := c.nodeTerminationTime(node, nodeClaim)
	if err != nil {
		return reconcile.Result{}, err
	}
	if err = c.terminator.Taint(ctx, node, v1.DisruptedNoScheduleTaint); err != nil {
		if errors.IsConflict(err) {
			return reconcile.Result{Requeue: true}, nil
		}
		return reconcile.Result{}, serrors.Wrap(fmt.Errorf("tainting node, %w", err), "taint", pretty.Taint(v1.DisruptedNoScheduleTaint))
	}

	for _, f := range []terminationFunc{
		c.awaitDrain,
		c.awaitVolumeDetachment,
		c.awaitInstanceTermination,
	} {
		result, err := f(ctx, nodeClaim, node, nodeTerminationTime)
		if result != nil || err != nil {
			return *result, err
		}
	}
	return reconcile.Result{}, nil
}

type terminationFunc func(context.Context, *v1.NodeClaim, *corev1.Node, *time.Time) (*reconcile.Result, error)

// awaitDrain initiates the drain of the node and will continue to requeue until the node has been drained. If the
// nodeClaim has a terminationGracePeriod set, pods will be deleted to ensure this function does not requeue past the
// nodeTerminationTime.
func (c *Controller) awaitDrain(
	ctx context.Context,
	nodeClaim *v1.NodeClaim,
	node *corev1.Node,
	nodeTerminationTime *time.Time,
) (*reconcile.Result, error) {
	if err := c.terminator.Drain(ctx, node, nodeTerminationTime); err != nil {
		if !terminator.IsNodeDrainError(err) {
			return &reconcile.Result{}, fmt.Errorf("draining node, %w", err)
		}
		c.recorder.Publish(terminatorevents.NodeFailedToDrain(node, err))
		stored := nodeClaim.DeepCopy()
		if modified := nodeClaim.StatusConditions().SetUnknownWithReason(v1.ConditionTypeDrained, "Draining", "Draining"); modified {
			if err := c.kubeClient.Status().Patch(ctx, nodeClaim, client.MergeFromWithOptions(stored, client.MergeFromWithOptimisticLock{})); err != nil {
				if errors.IsConflict(err) {
					return &reconcile.Result{Requeue: true}, nil
				}
				if errors.IsNotFound(err) {
					return &reconcile.Result{}, c.associatedNodeClaimError(err)
				}
				return &reconcile.Result{}, err
			}
		}
		return &reconcile.Result{RequeueAfter: 1 * time.Second}, nil
	}
	if !nodeClaim.StatusConditions().Get(v1.ConditionTypeDrained).IsTrue() {
		stored := nodeClaim.DeepCopy()
		// No need to check for modification since we've already verifyied it wasn't set to true
		_ = nodeClaim.StatusConditions().SetTrue(v1.ConditionTypeDrained)
		if err := c.kubeClient.Status().Patch(ctx, nodeClaim, client.MergeFromWithOptions(stored, client.MergeFromWithOptimisticLock{})); err != nil {
			if errors.IsConflict(err) {
				return &reconcile.Result{Requeue: true}, nil
			}
			if errors.IsNotFound(err) {
				return &reconcile.Result{}, c.associatedNodeClaimError(err)
			}
			return &reconcile.Result{}, err
		}
		NodesDrainedTotal.Inc(map[string]string{
			metrics.NodePoolLabel: node.Labels[v1.NodePoolLabelKey],
		})
		// We requeue after a patch operation since we want to ensure we read our own writes before any subsequent
		// operations on the NodeClaim.
		return &reconcile.Result{RequeueAfter: 1 * time.Second}, nil
	}
	return nil, nil
}

// awaitVolumeDetachment will continue to requeue until all volume attachments associated with the node have been
// deleted. The deletion is performed by the upstream attach-detach controller, Karpenter just needs to await deletion.
// This will be skipped once the nodeClaim's terminationGracePeriod has elapsed at nodeTerminationTime.
//
//nolint:gocyclo
func (c *Controller) awaitVolumeDetachment(
	ctx context.Context,
	nodeClaim *v1.NodeClaim,
	node *corev1.Node,
	nodeTerminationTime *time.Time,
) (*reconcile.Result, error) {
	// In order for Pods associated with PersistentVolumes to smoothly migrate from the terminating Node, we wait
	// for VolumeAttachments of drain-able Pods to be cleaned up before terminating Node and removing its finalizer.
	// However, if TerminationGracePeriod is configured for Node, and we are past that period, we will skip waiting.
	pendingVolumeAttachments, err := c.pendingVolumeAttachments(ctx, node)
	if err != nil {
		return &reconcile.Result{}, fmt.Errorf("ensuring no volume attachments, %w", err)
	}
	if len(pendingVolumeAttachments) == 0 {
		// There are no remaining volume attachments blocking instance termination. If we've already updated the status
		// condition, fall through. Otherwise, update the status condition and requeue.
		stored := nodeClaim.DeepCopy()
		if modified := nodeClaim.StatusConditions().SetTrue(v1.ConditionTypeVolumesDetached); modified {
			if err := c.kubeClient.Status().Patch(ctx, nodeClaim, client.MergeFromWithOptions(stored, client.MergeFromWithOptimisticLock{})); err != nil {
				if errors.IsConflict(err) {
					return &reconcile.Result{Requeue: true}, nil
				}
				if errors.IsNotFound(err) {
					return &reconcile.Result{}, c.associatedNodeClaimError(err)
				}
				return &reconcile.Result{}, err
			}
			// We requeue after a patch operation since we want to ensure we read our own writes before any subsequent
			// operations on the NodeClaim.
			return &reconcile.Result{RequeueAfter: 1 * time.Second}, nil
		}
	} else if !c.hasTerminationGracePeriodElapsed(nodeTerminationTime) {
		// There are volume attachments blocking instance termination remaining. We should set the status condition to
		// unknown (if not already) and requeue. This case should never fall through, to continue to instance termination
		// one of two conditions must be met: all blocking volume attachment objects must be deleted or the nodeclaim's TGP
		// must have expired.
		c.recorder.Publish(terminatorevents.NodeAwaitingVolumeDetachmentEvent(node, pendingVolumeAttachments...))
		stored := nodeClaim.DeepCopy()
		if modified := nodeClaim.StatusConditions().SetUnknownWithReason(v1.ConditionTypeVolumesDetached, "AwaitingVolumeDetachment", "AwaitingVolumeDetachment"); modified {
			if err := c.kubeClient.Status().Patch(ctx, nodeClaim, client.MergeFromWithOptions(stored, client.MergeFromWithOptimisticLock{})); err != nil {
				if errors.IsConflict(err) {
					return &reconcile.Result{Requeue: true}, nil
				}
				if errors.IsNotFound(err) {
					return &reconcile.Result{}, c.associatedNodeClaimError(err)
				}
				return &reconcile.Result{}, err
			}
		}
		return &reconcile.Result{RequeueAfter: 1 * time.Second}, nil
	} else {
		// There are volume attachments blocking instance termination remaining, but the nodeclaim's TGP has expired. In this
		// case we should set the status condition to false (requeing if it wasn't already) and then fall through to instance
		// termination.
		stored := nodeClaim.DeepCopy()
		if modified := nodeClaim.StatusConditions().SetFalse(v1.ConditionTypeVolumesDetached, "TerminationGracePeriodElapsed", "TerminationGracePeriodElapsed"); modified {
			if err := c.kubeClient.Status().Patch(ctx, nodeClaim, client.MergeFromWithOptions(stored, client.MergeFromWithOptimisticLock{})); err != nil {
				if errors.IsConflict(err) {
					return &reconcile.Result{Requeue: true}, nil
				}
				if errors.IsNotFound(err) {
					return &reconcile.Result{}, c.associatedNodeClaimError(err)
				}
				return &reconcile.Result{}, err
			}
			// We requeue after a patch operation since we want to ensure we read our own writes before any subsequent
			// operations on the NodeClaim.
			return &reconcile.Result{RequeueAfter: 1 * time.Second}, nil
		}
	}
	return nil, nil
}

// awaitInstanceTermination will initiate instance termination and continue to requeue until the cloudprovider indicates
// the instance is no longer found. Once gone, the node's finalizer will be removed, unblocking the NodeClaim lifecycle
// controller.
func (c *Controller) awaitInstanceTermination(
	ctx context.Context,
	nodeClaim *v1.NodeClaim,
	node *corev1.Node,
	_ *time.Time,
) (*reconcile.Result, error) {
	isInstanceTerminated, err := termination.EnsureTerminated(ctx, c.kubeClient, nodeClaim, c.cloudProvider)
	if client.IgnoreNotFound(err) != nil {
		// 409 - The nodeClaim exists, but its status has already been modified
		if errors.IsConflict(err) {
			return &reconcile.Result{Requeue: true}, nil
		}
		return &reconcile.Result{}, fmt.Errorf("ensuring instance termination, %w", err)
	}
	if !isInstanceTerminated {
		return &reconcile.Result{RequeueAfter: 5 * time.Second}, nil
	}
	if err := c.removeFinalizer(ctx, node); err != nil {
		return &reconcile.Result{}, err
	}
	return nil, nil
}

func (*Controller) associatedNodeClaimError(err error) error {
	return reconcile.TerminalError(fmt.Errorf("failed to terminate node, expected a single associated nodeclaim, %w", err))
}

func (c *Controller) hasTerminationGracePeriodElapsed(nodeTerminationTime *time.Time) bool {
	if nodeTerminationTime == nil {
		return false
	}
	return !c.clock.Now().Before(*nodeTerminationTime)
}

func (c *Controller) pendingVolumeAttachments(ctx context.Context, node *corev1.Node) ([]*storagev1.VolumeAttachment, error) {
	volumeAttachments, err := nodeutils.GetVolumeAttachments(ctx, c.kubeClient, node)
	if err != nil {
		return nil, err
	}
	// Filter out VolumeAttachments associated with not drain-able Pods
	filteredVolumeAttachments, err := filterVolumeAttachments(ctx, c.kubeClient, node, volumeAttachments, c.clock)
	if err != nil {
		return nil, err
	}
	return filteredVolumeAttachments, nil
}

// filterVolumeAttachments filters out storagev1.VolumeAttachments that should not block the termination
// of the passed corev1.Node
func filterVolumeAttachments(ctx context.Context, kubeClient client.Client, node *corev1.Node, volumeAttachments []*storagev1.VolumeAttachment, clk clock.Clock) ([]*storagev1.VolumeAttachment, error) {
	// No need to filter empty VolumeAttachments list
	if len(volumeAttachments) == 0 {
		return volumeAttachments, nil
	}
	// Create list of non-drain-able Pods associated with Node
	pods, err := nodeutils.GetPods(ctx, kubeClient, node)
	if err != nil {
		return nil, err
	}
	unDrainablePods := lo.Reject(pods, func(p *corev1.Pod, _ int) bool {
		return pod.IsDrainable(p, clk)
	})
	// Filter out VolumeAttachments associated with non-drain-able Pods
	// Match on Pod -> PersistentVolumeClaim -> PersistentVolume Name <- VolumeAttachment
	shouldFilterOutVolume := sets.New[string]()
	for _, p := range unDrainablePods {
		for _, v := range p.Spec.Volumes {
			pvc, err := volumeutil.GetPersistentVolumeClaim(ctx, kubeClient, p, v)
			if errors.IsNotFound(err) {
				continue
			}
			if err != nil {
				return nil, err
			}
			if pvc != nil {
				shouldFilterOutVolume.Insert(pvc.Spec.VolumeName)
			}
		}
	}
	filteredVolumeAttachments := lo.Reject(volumeAttachments, func(v *storagev1.VolumeAttachment, _ int) bool {
		pvName := v.Spec.Source.PersistentVolumeName
		return pvName == nil || shouldFilterOutVolume.Has(*pvName)
	})
	return filteredVolumeAttachments, nil
}

func (c *Controller) removeFinalizer(ctx context.Context, n *corev1.Node) error {
	stored := n.DeepCopy()
	controllerutil.RemoveFinalizer(n, v1.TerminationFinalizer)
	if !equality.Semantic.DeepEqual(stored, n) {
		// We use client.StrategicMergeFrom here since the node object supports it and
		// a strategic merge patch represents the finalizer list as a keyed "set" so removing
		// an item from the list doesn't replace the full list
		// https://github.com/kubernetes/kubernetes/issues/111643#issuecomment-2016489732
		if err := c.kubeClient.Patch(ctx, n, client.StrategicMergeFrom(stored)); err != nil {
			return client.IgnoreNotFound(fmt.Errorf("removing finalizer, %w", err))
		}

		metrics.NodesTerminatedTotal.Inc(map[string]string{
			metrics.NodePoolLabel: n.Labels[v1.NodePoolLabelKey],
		})

		// We use stored.DeletionTimestamp since the api-server may give back a node after the patch without a deletionTimestamp
		DurationSeconds.Observe(time.Since(stored.DeletionTimestamp.Time).Seconds(), map[string]string{
			metrics.NodePoolLabel: n.Labels[v1.NodePoolLabelKey],
		})

		NodeLifetimeDurationSeconds.Observe(time.Since(n.CreationTimestamp.Time).Seconds(), map[string]string{
			metrics.NodePoolLabel: n.Labels[v1.NodePoolLabelKey],
		})

		log.FromContext(ctx).Info("deleted node")
	}
	return nil
}

func (c *Controller) nodeTerminationTime(node *corev1.Node, nodeClaims ...*v1.NodeClaim) (*time.Time, error) {
	if len(nodeClaims) == 0 {
		return nil, nil
	}
	expirationTimeString, exists := nodeClaims[0].ObjectMeta.Annotations[v1.NodeClaimTerminationTimestampAnnotationKey]
	if !exists {
		return nil, nil
	}
	c.recorder.Publish(terminatorevents.NodeTerminationGracePeriodExpiring(node, expirationTimeString))
	expirationTime, err := time.Parse(time.RFC3339, expirationTimeString)
	if err != nil {
		return nil, serrors.Wrap(fmt.Errorf("parsing annotation, %w", err), "annotation", v1.NodeClaimTerminationTimestampAnnotationKey)
	}
	return &expirationTime, nil
}

func (c *Controller) Register(_ context.Context, m manager.Manager) error {
	return controllerruntime.NewControllerManagedBy(m).
		Named("node.termination").
		For(&corev1.Node{}, builder.WithPredicates(nodeutils.IsManagedPredicateFuncs(c.cloudProvider))).
		WithOptions(
			controller.Options{
				RateLimiter: workqueue.NewTypedMaxOfRateLimiter[reconcile.Request](
					workqueue.NewTypedItemExponentialFailureRateLimiter[reconcile.Request](100*time.Millisecond, 10*time.Second),
					// 10 qps, 100 bucket size
					&workqueue.TypedBucketRateLimiter[reconcile.Request]{Limiter: rate.NewLimiter(rate.Limit(10), 100)},
				),
				MaxConcurrentReconciles: 100,
			},
		).
		Complete(reconcile.AsReconciler(m.GetClient(), c))
}
