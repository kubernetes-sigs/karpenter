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

package node

import (
	"context"
	"fmt"
	"time"

	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"sigs.k8s.io/karpenter/pkg/utils/pod"
)

// GetPods grabs all pods that are currently bound to the passed nodes
func GetPods(ctx context.Context, kubeClient client.Client, nodes ...*v1.Node) ([]*v1.Pod, error) {
	var pods []*v1.Pod
	for _, node := range nodes {
		var podList v1.PodList
		if err := kubeClient.List(ctx, &podList, client.MatchingFields{"spec.nodeName": node.Name}); err != nil {
			return nil, fmt.Errorf("listing pods, %w", err)
		}
		for i := range podList.Items {
			pods = append(pods, &podList.Items[i])
		}
	}
	return pods, nil
}

// GetReschedulablePods grabs all pods from the passed nodes that satisfy the IsReschedulable criteria
func GetReschedulablePods(ctx context.Context, kubeClient client.Client, nodes ...*v1.Node) ([]*v1.Pod, error) {
	pods, err := GetPods(ctx, kubeClient, nodes...)
	if err != nil {
		return nil, fmt.Errorf("listing pods, %w", err)
	}
	return lo.Filter(pods, func(p *v1.Pod, _ int) bool {
		return pod.IsReschedulable(p)
	}), nil
}

// GetEvictablePods grabs all pods from the passed nodes that satisfy the IsEvictable criteria
func GetEvictablePods(ctx context.Context, kubeClient client.Client, now time.Time, nodes ...*v1.Node) ([]*v1.Pod, error) {
	pods, err := GetPods(ctx, kubeClient, nodes...)
	if err != nil {
		return nil, fmt.Errorf("listing pods, %w", err)
	}
	return lo.Filter(pods, func(p *v1.Pod, _ int) bool {
		return pod.IsEvictable(p, now)
	}), nil
}

// GetProvisionablePods grabs all the pods on the cluster that are not currently bound to nodes and
// satisfy the IsProvisionable criteria
func GetProvisionablePods(ctx context.Context, kubeClient client.Client) ([]*v1.Pod, error) {
	var pods []*v1.Pod
	var podList v1.PodList
	if err := kubeClient.List(ctx, &podList, client.MatchingFields{"spec.nodeName": ""}); err != nil {
		return nil, fmt.Errorf("listing pods, %w", err)
	}
	for i := range podList.Items {
		// these pods don't need to be rescheduled
		if pod.IsProvisionable(&podList.Items[i]) {
			pods = append(pods, &podList.Items[i])
		}
	}
	return pods, nil
}

func GetCondition(n *v1.Node, match v1.NodeConditionType) v1.NodeCondition {
	for _, condition := range n.Status.Conditions {
		if condition.Type == match {
			return condition
		}
	}
	return v1.NodeCondition{}
}
