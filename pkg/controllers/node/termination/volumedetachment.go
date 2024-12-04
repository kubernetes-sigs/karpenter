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
	"time"

	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/events"

	storagev1 "k8s.io/api/storage/v1"

	nodeutils "sigs.k8s.io/karpenter/pkg/utils/node"
	"sigs.k8s.io/karpenter/pkg/utils/nodeclaim"
	podutils "sigs.k8s.io/karpenter/pkg/utils/pod"
	volumeutil "sigs.k8s.io/karpenter/pkg/utils/volume"
)

type VolumeDetachmentReconciler struct {
	clk        clock.Clock
	kubeClient client.Client
	recorder   events.Recorder
}

// nolint:gocyclo
func (v *VolumeDetachmentReconciler) Reconcile(ctx context.Context, n *corev1.Node, nc *v1.NodeClaim) (reconcile.Result, error) {
	if !nc.StatusConditions().Get(v1.ConditionTypeDrained).IsTrue() {
		return reconcile.Result{}, nil
	}
	if nc.StatusConditions().Get(v1.ConditionTypeVolumesDetached).IsTrue() {
		return reconcile.Result{}, nil
	}
	if elapsed, err := nodeclaim.HasTerminationGracePeriodElapsed(v.clk, nc); err != nil {
		log.FromContext(ctx).Error(err, "failed to terminate node")
		return reconcile.Result{}, nil
	} else if elapsed {
		return reconcile.Result{}, nil
	}

	blockingVAs, err := v.blockingVolumeAttachments(ctx, n)
	if err != nil {
		return reconcile.Result{}, err
	}
	if len(blockingVAs) != 0 {
		stored := nc.DeepCopy()
		_ = nc.StatusConditions().SetFalse(v1.ConditionTypeVolumesDetached, "AwaitingVolumeDetachment", "AwaitingVolumeDetachment")
		if err := v.kubeClient.Status().Patch(ctx, nc, client.MergeFromWithOptions(stored, client.MergeFromWithOptimisticLock{})); err != nil {
			if errors.IsNotFound(err) {
				return reconcile.Result{}, nil
			}
			if errors.IsConflict(err) {
				return reconcile.Result{Requeue: true}, nil
			}
			return reconcile.Result{}, err
		}
		v.recorder.Publish(NodeAwaitingVolumeDetachmentEvent(n, blockingVAs...))
		return reconcile.Result{RequeueAfter: 5 * time.Second}, nil
	}

	stored := nc.DeepCopy()
	_ = nc.StatusConditions().SetTrue(v1.ConditionTypeVolumesDetached)
	if err := v.kubeClient.Status().Patch(ctx, nc, client.MergeFromWithOptions(stored, client.MergeFromWithOptimisticLock{})); err != nil {
		if errors.IsNotFound(err) {
			return reconcile.Result{}, nil
		}
		if errors.IsConflict(err) {
			return reconcile.Result{Requeue: true}, nil
		}
		return reconcile.Result{}, err
	}
	return reconcile.Result{}, nil
}

func (v *VolumeDetachmentReconciler) blockingVolumeAttachments(ctx context.Context, n *corev1.Node) ([]*storagev1.VolumeAttachment, error) {
	vas, err := nodeutils.GetVolumeAttachments(ctx, v.kubeClient, n)
	if err != nil {
		return nil, err
	}
	if len(vas) == 0 {
		return nil, nil
	}

	pods, err := nodeutils.GetPods(ctx, v.kubeClient, n)
	if err != nil {
		return nil, err
	}
	pods = lo.Reject(pods, func(p *corev1.Pod, _ int) bool {
		return podutils.IsDrainable(p, v.clk)
	})

	// Determine the VolumeAttachments associated with non-drainable pods. We consider these non-blocking since they
	// will never be detached without intervention (since the pods aren't drained).
	nonBlockingVolumes := sets.New[string]()
	for _, p := range pods {
		for _, vol := range p.Spec.Volumes {
			pvc, err := volumeutil.GetPersistentVolumeClaim(ctx, v.kubeClient, p, vol)
			if errors.IsNotFound(err) {
				continue
			}
			if err != nil {
				return nil, err
			}
			if pvc != nil {
				nonBlockingVolumes.Insert(pvc.Spec.VolumeName)
			}
		}
	}
	blockingVAs := lo.Reject(vas, func(v *storagev1.VolumeAttachment, _ int) bool {
		pvName := v.Spec.Source.PersistentVolumeName
		return pvName == nil || nonBlockingVolumes.Has(*pvName)
	})
	return blockingVAs, nil
}
