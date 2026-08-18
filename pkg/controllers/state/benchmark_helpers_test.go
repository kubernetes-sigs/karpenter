//go:build test_performance || test_performance_5000

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

package state_test

import (
	"context"
	"fmt"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/clock"
	fakecr "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	cloudproviderfake "sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/test"
)

// benchContext returns a context carrying options defaults -- required by paths like StateNode.Nominate
// (nominationWindow) and pod eviction-cost computation that read options.FromContext.
func benchContext() context.Context {
	return options.ToContext(context.Background(), test.Options())
}

// newBenchClusterClient builds a controller-runtime fake client with the "spec.nodeName" field index that
// Cluster.populateResourceRequests (invoked via UpdateNode) requires. Using the fake client instead of a real
// envtest apiserver keeps setup fast enough to build 5000-node fixtures without minutes of API round trips.
func newBenchClusterClient() fakecr.Client {
	return fake.NewClientBuilder().
		WithIndex(&corev1.Pod{}, "spec.nodeName", func(o fakecr.Object) []string {
			return []string{o.(*corev1.Pod).Spec.NodeName}
		}).
		Build()
}

// setupBenchCluster builds a *state.Cluster with numNodes registered nodes, each already carrying podsPerNode
// bound pods (added via UpdatePod, mirroring how the informer path populates usage tracking in production). Pods
// have no volumes, so GetVolumes never needs a client round trip -- setup stays in-memory after node registration.
func setupBenchCluster(b *testing.B, numNodes, podsPerNode int) (*state.Cluster, []*corev1.Node) {
	b.Helper()
	ctx := benchContext()
	client := newBenchClusterClient()
	cloudProvider := cloudproviderfake.NewCloudProvider()
	cluster := state.NewCluster(&clock.RealClock{}, client, cloudProvider)

	nodePool := "bench-nodepool"
	nodeClaims, nodes := test.NodeClaimsAndNodes(numNodes, v1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{
				v1.NodePoolLabelKey:            nodePool,
				corev1.LabelInstanceTypeStable: "bench-instance-type",
				v1.NodeInitializedLabelKey:     "true",
				v1.NodeRegisteredLabelKey:      "true",
			},
		},
		Status: v1.NodeClaimStatus{
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("32"),
				corev1.ResourceMemory: resource.MustParse("128Gi"),
				corev1.ResourcePods:   resource.MustParse("110"),
			},
		},
	})

	for i := range numNodes {
		cluster.UpdateNodeClaim(nodeClaims[i])
		if err := cluster.UpdateNode(ctx, nodes[i]); err != nil {
			b.Fatalf("registering node %d, %s", i, err)
		}
	}

	for i := range numNodes {
		for p := 0; p < podsPerNode; p++ {
			pod := test.Pod(test.PodOptions{
				ResourceRequirements: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("100m"),
						corev1.ResourceMemory: resource.MustParse("128Mi"),
					},
				},
			})
			pod.Spec.NodeName = nodes[i].Name
			if err := cluster.UpdatePod(ctx, pod); err != nil {
				b.Fatalf("binding pod %d to node %d, %s", p, i, err)
			}
		}
	}

	return cluster, nodes
}

// randomPod builds a minimal, volume-free pod bound to the given node name, suitable for repeated UpdatePod/
// DeletePod churn without touching the fake client (GetVolumes short-circuits with zero volumes).
func randomPod(nodeName string, seq int) *corev1.Pod {
	pod := test.Pod(test.PodOptions{
		ObjectMeta: test.ObjectMeta(),
		ResourceRequirements: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("100m"),
				corev1.ResourceMemory: resource.MustParse("128Mi"),
			},
		},
	})
	pod.Name = fmt.Sprintf("%s-churn-%d", pod.Name, seq)
	pod.Spec.NodeName = nodeName
	return pod
}
