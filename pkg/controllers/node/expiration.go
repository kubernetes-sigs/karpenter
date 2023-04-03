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
	"time"

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
	// Ignore node if not applicable
	if provisioner.Spec.TTLSecondsUntilExpired == nil {
		return reconcile.Result{}, nil
	}

	// If the node is marked as voluntarily disrupted by another controller, do nothing.
	val, hasAnnotation := node.Annotations[v1alpha5.VoluntaryDisruptionAnnotationKey]
	if hasAnnotation && val != v1alpha5.VoluntaryDisruptionExpiredAnnotationValue {
		return reconcile.Result{}, nil
	}

	expired := utilsnode.IsExpired(node, e.clock, provisioner)
	if !expired && hasAnnotation {
		delete(node.Annotations, v1alpha5.EmptinessTimestampAnnotationKey)
		logging.FromContext(ctx).Infof("removed expiration TTL from node")
	} else if expired && !hasAnnotation {
		node.Annotations = lo.Assign(node.Annotations, map[string]string{
			v1alpha5.VoluntaryDisruptionExpiredAnnotationValue: e.clock.Now().Format(time.RFC3339),
		})
		logging.FromContext(ctx).Infof("added TTL to expired node")
	}

	// If not expired, and doesn't have annotation, requeue at expiration time.
	return reconcile.Result{RequeueAfter: time.Until(utilsnode.GetExpirationTime(node, provisioner))}, nil
}
