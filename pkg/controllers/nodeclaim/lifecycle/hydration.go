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

	"github.com/samber/lo"
	"k8s.io/apimachinery/pkg/api/equality"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	nodeclaimutils "sigs.k8s.io/karpenter/pkg/utils/nodeclaim"
)

// The Hydration sub-reconciler is used to hydrate Nodes / NodeClaims with metadata added in new versions of Karpenter.
// TODO: Remove after a sufficiently long time from the v1.1 release
type Hydration struct {
	kubeClient client.Client
}

func (h *Hydration) Reconcile(ctx context.Context, nodeClaim *v1.NodeClaim) (reconcile.Result, error) {
	nodeClaim.Labels = lo.Assign(nodeClaim.Labels, map[string]string{
		v1.NodeClassLabelKey(nodeClaim.Spec.NodeClassRef.GroupKind()): nodeClaim.Spec.NodeClassRef.Name,
	})
	if err := h.hydrateNode(ctx, nodeClaim); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(fmt.Errorf("hydrating node, %w", err))
	}
	return reconcile.Result{}, nil
}

func (h *Hydration) hydrateNode(ctx context.Context, nodeClaim *v1.NodeClaim) error {
	node, err := nodeclaimutils.NodeForNodeClaim(ctx, h.kubeClient, nodeClaim)
	if err != nil {
		if nodeclaimutils.IsNodeNotFoundError(err) {
			return nil
		}
		return err
	}
	stored := node.DeepCopy()
	node.Labels = lo.Assign(node.Labels, map[string]string{
		v1.NodeClassLabelKey(nodeClaim.Spec.NodeClassRef.GroupKind()): nodeClaim.Spec.NodeClassRef.Name,
	})
	if !equality.Semantic.DeepEqual(stored, node) {
		// We use client.MergeFromWithOptimisticLock because patching a list with a JSON merge patch
		// can cause races due to the fact that it fully replaces the list on a change
		// Here, we are updating the taint list
		if err := h.kubeClient.Patch(ctx, node, client.MergeFrom(stored)); err != nil {
			return err
		}
	}
	return nil
}
