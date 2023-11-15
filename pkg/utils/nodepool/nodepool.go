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

package nodepool

import (
	"context"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/aws/karpenter-core/pkg/apis/v1beta1"
)

type Key struct {
	Name          string
	IsProvisioner bool
}

func Get(ctx context.Context, c client.Client, key Key) (*v1beta1.NodePool, error) {
	nodePool := &v1beta1.NodePool{}
	if err := c.Get(ctx, types.NamespacedName{Name: key.Name}, nodePool); err != nil {
		return nil, err
	}
	return nodePool, nil
}

func List(ctx context.Context, c client.Client, opts ...client.ListOption) (*v1beta1.NodePoolList, error) {
	nodePoolList := &v1beta1.NodePoolList{}
	if err := c.List(ctx, nodePoolList, opts...); err != nil {
		return nil, err
	}
	return nodePoolList, nil
}

func Patch(ctx context.Context, c client.Client, stored, nodePool *v1beta1.NodePool) error {
	return c.Patch(ctx, nodePool, client.MergeFrom(stored))
}

func PatchStatus(ctx context.Context, c client.Client, stored, nodePool *v1beta1.NodePool) error {
	return c.Status().Patch(ctx, nodePool, client.MergeFrom(stored))
}

func HashAnnotation(nodePool *v1beta1.NodePool) map[string]string {
	return map[string]string{v1beta1.NodePoolHashAnnotationKey: nodePool.Hash()}
}
