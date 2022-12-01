/*
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

package node

import (
	"context"
	"fmt"
	"time"

	"k8s.io/utils/clock"

	v1 "k8s.io/api/core/v1"
	"knative.dev/pkg/logging"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/samber/lo"

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/controllers/state"
	"github.com/aws/karpenter-core/pkg/utils/pod"
)

// Emptiness is a subreconciler that deletes nodes that are empty after a ttl
type Emptiness struct {
	kubeClient client.Client
	clock      clock.Clock
	cluster    *state.Cluster
}

// Reconcile reconciles the node
func (r *Emptiness) Reconcile(ctx context.Context, provisioner *v1alpha5.Provisioner, n *v1.Node) (reconcile.Result, error) {
	// Ignore node if not applicable
	if provisioner.Spec.TTLSecondsAfterEmpty == nil {
		return reconcile.Result{}, nil
	}

	// node is not ready yet, so we don't consider it to possibly be empty
	if n.Labels[v1alpha5.LabelNodeInitialized] != "true" {
		return reconcile.Result{}, nil
	}

	empty, err := r.isEmpty(ctx, n)
	if err != nil {
		return reconcile.Result{}, err
	}

	// node is empty, but it is in-use per the last scheduling round so we don't consider it empty
	if r.cluster.IsNodeNominated(n.Name) {
		return reconcile.Result{}, nil
	}

	_, hasEmptinessTimestamp := n.Annotations[v1alpha5.EmptinessTimestampAnnotationKey]
	if !empty && hasEmptinessTimestamp {
		delete(n.Annotations, v1alpha5.EmptinessTimestampAnnotationKey)
		logging.FromContext(ctx).Infof("removed emptiness TTL from node")
	} else if empty && !hasEmptinessTimestamp {
		n.Annotations = lo.Assign(n.Annotations, map[string]string{
			v1alpha5.EmptinessTimestampAnnotationKey: r.clock.Now().Format(time.RFC3339),
		})
		logging.FromContext(ctx).Infof("added TTL to empty node")
	}

	return reconcile.Result{}, nil
}

func (r *Emptiness) isEmpty(ctx context.Context, n *v1.Node) (bool, error) {
	pods := &v1.PodList{}
	if err := r.kubeClient.List(ctx, pods, client.MatchingFields{"spec.nodeName": n.Name}); err != nil {
		return false, fmt.Errorf("listing pods for node, %w", err)
	}
	for i := range pods.Items {
		p := pods.Items[i]
		if !pod.IsTerminal(&p) && !pod.IsOwnedByDaemonSet(&p) && !pod.IsOwnedByNode(&p) {
			return false, nil
		}
	}
	return true, nil
}
