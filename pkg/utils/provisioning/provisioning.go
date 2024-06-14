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

package provisioning

import (
	"context"
	"fmt"

	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"sigs.k8s.io/karpenter/pkg/controllers/provisioning"
	"sigs.k8s.io/karpenter/pkg/controllers/provisioning/scheduling"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/events"
	operatorlogging "sigs.k8s.io/karpenter/pkg/operator/logging"
)

//nolint:gocyclo
func SimulateScheduling(ctx context.Context, kubeClient client.Client, cluster *state.Cluster, provisioner *provisioning.Provisioner,
	recorder events.Recorder, candidates ...*state.StateNode,
) (scheduling.Results, error) {
	nodes := cluster.Nodes()
	deletingNodes := nodes.Deleting()

	// We get the pods that are on nodes that are deleting
	// and track them separately for understanding simulation results
	deletingNodesToPods, err := deletingNodes.ReschedulablePods(ctx, kubeClient)
	if err != nil {
		return scheduling.Results{}, fmt.Errorf("failed to get pods from deleting nodes, %w", err)
	}
	// Get all pending pods
	pods, err := provisioner.GetPendingPods(ctx)
	if err != nil {
		return scheduling.Results{}, fmt.Errorf("determining pending pods, %w", err)
	}
	// Get the reschedulable pods on the candidates we're trying to consider deleting
	errs := make([]error, len(candidates))
	for i, candidate := range candidates {
		reschedulable, err := candidate.ReschedulablePods(ctx, kubeClient)
		if err != nil {
			errs[i] = err
			continue
		}
		pods = append(pods, reschedulable...)
	}
	for _, v := range deletingNodesToPods {
		pods = append(pods, v...)
	}
	// Add a nop logger so we don't emit any logs from the scheduling simulation, as we're only simulating.

	scheduler, err := provisioner.NewScheduler(log.IntoContext(ctx, operatorlogging.NopLogger), pods, nodes)
	if err != nil {
		return scheduling.Results{}, fmt.Errorf("creating scheduler, %w", err)
	}
	deletingNodePodKeys := lo.SliceToMap(deletingNodesToPods, func(p *v1.Pod) (client.ObjectKey, interface{}) {
		return client.ObjectKeyFromObject(p), nil
	})

	results := scheduler.Solve(ctx, pods).TruncateInstanceTypes(scheduling.MaxInstanceTypes)
	for _, n := range results.ExistingNodes {
		// We consider existing nodes for scheduling. When these nodes are unmanaged, their taint logic should
		// tell us if we can schedule to them or not; however, if these nodes are managed, we will still schedule to them
		// even if they are still in the middle of their initialization loop. In the case of disruption, we don't want
		// to proceed disrupting if our scheduling decision relies on nodes that haven't entered a terminal state.
		if !n.Initialized() {
			for _, p := range n.Pods {
				// Only add a pod scheduling error if it isn't on an already deleting node.
				// If the pod is on a deleting node, we assume one of two things has already happened:
				// 1. The node was manually terminated, at which the provisioning controller has scheduled or is scheduling a node
				//    for the pod.
				// 2. The node was chosen for a previous disruption command, we assume that the uninitialized node will come up
				//    for this command, and we assume it will be successful. If it is not successful, the node will become
				//    not terminating, and we will no longer need to consider these pods.
				if _, ok := deletingNodePodKeys[client.ObjectKeyFromObject(p)]; !ok {
					results.PodErrors[p] = fmt.Errorf("would schedule against a non-initialized node %s", n.Name())
				}
			}
		}
	}

	return results, nil
}
