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

package static

import (
	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
)

func IsStaticNodePool(np *v1.NodePool) bool {
	return np.Spec.Replicas != nil
}

func GetFilteredNodes(c *state.Cluster, filter func(*state.StateNode) bool) []*state.StateNode {
	nodes := make([]*state.StateNode, 0)
	c.ForEachNode(func(node *state.StateNode) bool {
		if filter(node) {
			nodes = append(nodes, node)
		}
		return true
	})
	return nodes
}
