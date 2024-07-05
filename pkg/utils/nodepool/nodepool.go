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

package nodepool

import (
	"context"

	"github.com/awslabs/operatorpkg/object"

	"github.com/samber/lo"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"sigs.k8s.io/karpenter/pkg/apis/v1beta1"
)

// NodeClassEventHandler is a watcher on v1beta1.NodePool that maps NodeClass to NodePools based
// on the nodeClassRef and enqueues reconcile.Requests for the NodePool
func NodeClassEventHandler(c client.Client) handler.EventHandler {
	return handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, o client.Object) (requests []reconcile.Request) {
		nodePoolList := &v1beta1.NodePoolList{}
		if err := c.List(ctx, nodePoolList, client.MatchingFields{
			"spec.template.spec.nodeClassRef.apiVersion": object.GVK(o).GroupVersion().String(),
			"spec.template.spec.nodeClassRef.kind":       object.GVK(o).Kind,
			"spec.template.spec.nodeClassRef.name":       o.GetName(),
		}); err != nil {
			return requests
		}
		return lo.Map(nodePoolList.Items, func(n v1beta1.NodePool, _ int) reconcile.Request {
			return reconcile.Request{
				NamespacedName: client.ObjectKeyFromObject(&n),
			}
		})
	})
}
