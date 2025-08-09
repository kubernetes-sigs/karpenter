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

package nodeoverlay

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"sigs.k8s.io/karpenter/pkg/apis/v1alpha1"
)

// NodeEventHandler is a watcher on corev1.Node that maps Nodes to NodeClaims based on provider ids
// and enqueues reconcile.Requests for the NodeClaims
func NodePoolEventHandler(c client.Client) handler.EventHandler {
	return handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, o client.Object) []reconcile.Request {
		nodeOverlayList := &v1alpha1.NodeOverlayList{}
		err := c.List(ctx, nodeOverlayList)
		if err != nil {
			return nil
		}
		if len(nodeOverlayList.Items) > 0 {
			return []reconcile.Request{{NamespacedName: client.ObjectKeyFromObject(&nodeOverlayList.Items[0])}}
		}
		return []reconcile.Request{}
	})
}
