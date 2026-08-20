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
	"runtime"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/clock"
	fakecr "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	cloudproviderfake "sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/test"
)

// This file is deliberately not build-tagged test_performance: it's a correctness/regression guard (bounded heap
// growth under sustained churn), not an opt-in benchmark -- runs as part of the normal `go test
// ./pkg/controllers/state/...`.
//
// The copy-on-write + generation-counter design (see cluster.go's Snapshot/CopyForMutation) intentionally
// allocates a new *StateNode (and, for pod-usage paths, new maps/usage-tracker clones) on every mutation instead
// of mutating in place. Per-mutation allocation is expected and bounded -- see
// pkg/controllers/state/statenode_benchmark_test.go for the isolated per-call cost. What this test guards against
// is *unbounded* growth: e.g. a future change that appends to a slice/map instead of replacing it, or that
// accidentally retains a chain of old StateNode generations instead of letting them become garbage once
// superseded. If heap growth over a fixed number of churn rounds keeps increasing round after round instead of
// staying roughly flat, that's the signature of a real leak, not routine copy-on-write allocation.
//
// Detection strategy: run two equal-sized churn windows back-to-back (after a warmup window to reach steady
// state) and compare their heap growth. Routine copy-on-write allocation costs the same per round regardless of
// how many rounds have already run, so the two windows' growth should be comparable; a real leak compounds and
// makes the second window's growth measurably larger than the first's.

const (
	memTestNumNodes         = 200
	memTestPodsPerNode      = 20
	memTestWarmupRounds     = 200
	memTestWindowRounds     = 300
	memTestEventsPerRound   = 10
	memTestGrowthRatioLimit = 1.35 // second window's heap growth must not exceed this multiple of the first's
)

func newMemTestClusterClient() fakecr.Client {
	return fake.NewClientBuilder().
		WithIndex(&corev1.Pod{}, "spec.nodeName", func(o fakecr.Object) []string {
			return []string{o.(*corev1.Pod).Spec.NodeName}
		}).
		Build()
}

func newMemTestCluster(t *testing.T, numNodes, podsPerNode int) (*state.Cluster, []*corev1.Node) {
	t.Helper()
	ctx := options.ToContext(context.Background(), test.Options())
	client := newMemTestClusterClient()
	cluster := state.NewCluster(&clock.RealClock{}, client, cloudproviderfake.NewCloudProvider())

	nodeClaims, nodes := test.NodeClaimsAndNodes(numNodes, v1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{
				v1.NodePoolLabelKey:            "memtest-nodepool",
				corev1.LabelInstanceTypeStable: "memtest-instance-type",
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
			t.Fatalf("registering node %d, %s", i, err)
		}
	}
	for i := range numNodes {
		for p := 0; p < podsPerNode; p++ {
			pod := memTestPod(nodes[i].Name, fmt.Sprintf("seed-%d-%d", i, p))
			if err := cluster.UpdatePod(ctx, pod); err != nil {
				t.Fatalf("seeding pod %d on node %d, %s", p, i, err)
			}
		}
	}
	return cluster, nodes
}

func memTestPod(nodeName, name string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("pod-%s", name), Namespace: "default"},
		Spec: corev1.PodSpec{
			NodeName: nodeName,
			Containers: []corev1.Container{{
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("100m"),
						corev1.ResourceMemory: resource.MustParse("128Mi"),
					},
				},
			}},
		},
	}
}

// churnRound performs one round of realistic cluster-state churn: eventsPerRound pod bind+unbind pairs, plus one
// MarkForDeletion/UnmarkForDeletion and one NominateNodeForPod against a rotating node -- mirroring the mix of
// mutation paths a live disruption/provisioning reconcile loop exercises. Every round also takes a Snapshot(),
// mirroring how often disruption reads cluster state.
func churnRound(t *testing.T, ctx context.Context, cluster *state.Cluster, nodes []*corev1.Node, round int) {
	t.Helper()
	for e := 0; e < memTestEventsPerRound; e++ {
		node := nodes[(round*memTestEventsPerRound+e)%len(nodes)]
		pod := memTestPod(node.Name, fmt.Sprintf("churn-%d-%d", round, e))
		if err := cluster.UpdatePod(ctx, pod); err != nil {
			t.Fatalf("binding churn pod, %s", err)
		}
		cluster.DeletePod(types.NamespacedName{Namespace: pod.Namespace, Name: pod.Name})
	}
	node := nodes[round%len(nodes)]
	cluster.MarkForDeletion(node.Spec.ProviderID)
	cluster.UnmarkForDeletion(node.Spec.ProviderID)
	cluster.NominateNodeForPod(ctx, node.Spec.ProviderID)
	_ = cluster.Snapshot()
}

// heapGrowth runs n churn rounds and returns the increase in live heap bytes (HeapAlloc), forcing a GC before and
// after so the measurement reflects retained memory, not just uncollected garbage sitting in the heap.
func heapGrowth(t *testing.T, ctx context.Context, cluster *state.Cluster, nodes []*corev1.Node, startRound, n int) uint64 {
	t.Helper()
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	for i := 0; i < n; i++ {
		churnRound(t, ctx, cluster, nodes, startRound+i)
	}

	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)

	if after.HeapAlloc <= before.HeapAlloc {
		return 0
	}
	return after.HeapAlloc - before.HeapAlloc
}

// TestClusterMemory_NoUnboundedGrowthUnderChurn guards against the copy-on-write refactor (or any future change to
// Cluster's mutation paths) silently reintroducing unbounded memory growth -- e.g. a superseded *StateNode
// generation, or one of its cloned maps, being retained somewhere instead of becoming garbage once no longer
// referenced. See the file-level comment for the comparison strategy.
func TestClusterMemory_NoUnboundedGrowthUnderChurn(t *testing.T) {
	ctx := options.ToContext(context.Background(), test.Options())
	cluster, nodes := newMemTestCluster(t, memTestNumNodes, memTestPodsPerNode)

	// Warm up: let allocator/GC pacing and any one-time setup costs (e.g. map growth to steady-state size)
	// settle before measuring, so the two comparison windows both reflect steady-state behavior.
	for i := 0; i < memTestWarmupRounds; i++ {
		churnRound(t, ctx, cluster, nodes, i)
	}

	firstWindowGrowth := heapGrowth(t, ctx, cluster, nodes, memTestWarmupRounds, memTestWindowRounds)
	secondWindowGrowth := heapGrowth(t, ctx, cluster, nodes, memTestWarmupRounds+memTestWindowRounds, memTestWindowRounds)

	t.Logf("heap growth: window 1 = %d bytes, window 2 = %d bytes", firstWindowGrowth, secondWindowGrowth)

	// Both windows are equal-sized and run the identical workload -- routine copy-on-write allocation costs the
	// same per round regardless of how many rounds already ran, so growth should be comparable. A real leak
	// compounds: the second window would retain everything the first window allocated on top of its own,
	// making its growth measurably larger.
	if firstWindowGrowth > 0 {
		limit := uint64(float64(firstWindowGrowth) * memTestGrowthRatioLimit)
		if secondWindowGrowth > limit {
			t.Fatalf("heap growth is compounding across equal churn windows (window 1: %d bytes, window 2: %d bytes, "+
				"limit: %d bytes at %.2fx) -- this is the signature of unbounded retention, not routine copy-on-write allocation",
				firstWindowGrowth, secondWindowGrowth, limit, memTestGrowthRatioLimit)
		}
	}
}
