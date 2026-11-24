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

package disruption_test

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clock "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakecr "sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
	"sigs.k8s.io/karpenter/pkg/controllers/disruption"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/test"
)

// benchmarkNodePoolStatsCluster returns cluster state with n initialized Nodes spread over 10 NodePools, each with
// podsPerNode bound pods.
func benchmarkNodePoolStatsCluster(b *testing.B, n, podsPerNode int) *state.Cluster {
	b.Helper()
	benchCtx := options.ToContext(context.Background(), test.Options())
	kubeClient := fakecr.NewClientBuilder().WithIndex(&corev1.Pod{}, "spec.nodeName", func(o client.Object) []string {
		return []string{o.(*corev1.Pod).Spec.NodeName}
	}).Build()
	clusterState := state.NewCluster(clock.NewFakeClock(time.Now()), kubeClient, fake.NewCloudProvider())
	for i := range n {
		labels := map[string]string{
			v1.NodePoolLabelKey:            fmt.Sprintf("nodepool-%d", i%10),
			v1.NodeInitializedLabelKey:     "true",
			corev1.LabelInstanceTypeStable: "default-instance-type",
		}
		providerID := fmt.Sprintf("fake:///node-%d", i)
		clusterState.UpdateNodeClaim(test.NodeClaim(v1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("nodeclaim-%d", i), Labels: labels},
			Status:     v1.NodeClaimStatus{ProviderID: providerID},
		}))
		node := test.Node(test.NodeOptions{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("node-%d", i), Labels: labels}, ProviderID: providerID, ReadyStatus: corev1.ConditionTrue})
		if err := clusterState.UpdateNode(benchCtx, node); err != nil {
			b.Fatal(err)
		}
		for j := range podsPerNode {
			pod := test.Pod(test.PodOptions{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("pod-%d-%d", i, j), Namespace: "default"}, NodeName: node.Name})
			if err := clusterState.UpdatePod(benchCtx, pod); err != nil {
				b.Fatal(err)
			}
		}
	}
	return clusterState
}

// BenchmarkNodePoolStats measures one NodePoolStats call. DeepCopy runs the same count over a deep copy of every
// Node, the copy NodePoolStats made before it counted cluster state in place, for comparison.
func BenchmarkNodePoolStats(b *testing.B) {
	for _, n := range []int{1000, 5000} {
		clusterState := benchmarkNodePoolStatsCluster(b, n, 30)
		if numNodes, _ := disruption.NodePoolStats(clusterState, v1.DisruptionReasonUnderutilized); lo.Sum(lo.Values(numNodes)) != n {
			b.Fatalf("expected %d counted nodes, got %d", n, lo.Sum(lo.Values(numNodes)))
		}
		b.Run(fmt.Sprintf("Nodes=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				disruption.NodePoolStats(clusterState, v1.DisruptionReasonUnderutilized)
			}
		})
		b.Run(fmt.Sprintf("Nodes=%d/DeepCopy", n), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				disruption.NodePoolStatsFromNodes(slices.Values(clusterState.DeepCopyNodes()), v1.DisruptionReasonUnderutilized)
			}
		})
	}
}
