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

	v1 "k8s.io/api/core/v1"
	"k8s.io/utils/clock"
	"knative.dev/pkg/logging"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/samber/lo"

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	utilsnode "github.com/aws/karpenter-core/pkg/utils/node"
)

// Expiration is a node sub-controller that annotates or de-annotates an expired node based on TTLSecondsUntilExpired
type Expiration struct {
	clock clock.Clock
}

func (e *Expiration) Reconcile(ctx context.Context, provisioner *v1alpha5.Provisioner, node *v1.Node) (reconcile.Result, error) {
	// If the node is marked as voluntarily disrupted by another controller, do nothing.
	val, hasAnnotation := node.Annotations[v1alpha5.VoluntaryDisruptionAnnotationKey]
	if hasAnnotation && val != v1alpha5.VoluntaryDisruptionExpiredAnnotationValue {
		return reconcile.Result{}, nil
	}

	// From here there are three scenarios to handle:
	// 1. If TTLSecondsUntilExpired is not configured, but the node is expired,
	//    remove the annotation so another disruption controller can annotate the node.
	if provisioner.Spec.TTLSecondsUntilExpired == nil {
		if val == v1alpha5.VoluntaryDisruptionExpiredAnnotationValue {
			delete(node.Annotations, v1alpha5.VoluntaryDisruptionAnnotationKey)
			logging.FromContext(ctx).Infof("removing expiration annotation from node as expiration has been disabled")
		}
		return reconcile.Result{}, nil
	}

	// 2. Otherwise, if the node isn't expired, but has the annotation, remove it.
	expired := utilsnode.IsExpired(node, e.clock, provisioner)
	if !expired && hasAnnotation {
		delete(node.Annotations, v1alpha5.VoluntaryDisruptionAnnotationKey)
		logging.FromContext(ctx).Infof("removing expiration annotation from node")
		// 3. Finally, if the node is expired, but doesn't have the annotation, add it.
	} else if expired && !hasAnnotation {
		node.Annotations = lo.Assign(node.Annotations, map[string]string{
			v1alpha5.VoluntaryDisruptionAnnotationKey: v1alpha5.VoluntaryDisruptionExpiredAnnotationValue,
		})
		logging.FromContext(ctx).Infof("annotating node as expired")
	}

	// If the node isn't expired and doesn't have annotation, return.
	// Use t.Sub(t.Now()) instead of time.Now() to ensure we're using the injected clock.
	return reconcile.Result{RequeueAfter: utilsnode.GetExpirationTime(node, provisioner).Sub(e.clock.Now())}, nil
}
