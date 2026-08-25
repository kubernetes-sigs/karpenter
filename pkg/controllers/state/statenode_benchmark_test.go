//go:build test_performance

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

package state

import (
	"context"
	"fmt"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// This is an internal test file (package state, not state_test) so it can call the unexported updateForPod
// directly to build a fixture with realistic map sizes, without needing a Cluster, a client, or any API calls --
// isolating exactly the cost of copying a StateNode's data, with zero noise from anything else.
//
// These benchmark ShallowCopy and the generated DeepCopy -- the two copy operations that exist TODAY. They
// establish the baseline that a future CopyForMutation() helper (added when updateForPod/cleanupForPod become
// copy-on-write) must be compared against: CopyForMutation should land closer to ShallowCopy's cost than
// DeepCopy's, since it only needs to clone the small mutable maps/usage-trackers, not the Node/NodeClaim objects.
//
// Run with:
//
//	go test -tags=test_performance -run=XXX -bench=. ./pkg/controllers/state/... -benchtime=20x

func newBenchStateNode(podCount int) *StateNode {
	n := NewNode()
	n.Node = &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "bench-node"},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("32"),
				corev1.ResourceMemory: resource.MustParse("128Gi"),
			},
		},
	}
	for i := 0; i < podCount; i++ {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("pod-%d", i), Namespace: "default"},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("100m"),
							corev1.ResourceMemory: resource.MustParse("128Mi"),
						},
					},
					Ports: []corev1.ContainerPort{{HostPort: int32(8000 + i), Protocol: corev1.ProtocolTCP}},
				}},
			},
		}
		// nil kubeClient is safe here: GetVolumes only touches the client when pod.Spec.Volumes is non-empty.
		if err := n.updateForPod(context.Background(), nil, pod); err != nil {
			panic(fmt.Sprintf("populating bench fixture, %s", err))
		}
	}
	return n
}

func BenchmarkStateNode_ShallowCopy_0Pods(b *testing.B)   { benchmarkShallowCopy(b, 0) }
func BenchmarkStateNode_ShallowCopy_20Pods(b *testing.B)  { benchmarkShallowCopy(b, 20) }
func BenchmarkStateNode_ShallowCopy_100Pods(b *testing.B) { benchmarkShallowCopy(b, 100) }

func benchmarkShallowCopy(b *testing.B, podCount int) {
	n := newBenchStateNode(podCount)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = n.ShallowCopy()
	}
}

func BenchmarkStateNode_DeepCopy_0Pods(b *testing.B)   { benchmarkDeepCopy(b, 0) }
func BenchmarkStateNode_DeepCopy_20Pods(b *testing.B)  { benchmarkDeepCopy(b, 20) }
func BenchmarkStateNode_DeepCopy_100Pods(b *testing.B) { benchmarkDeepCopy(b, 100) }

func benchmarkDeepCopy(b *testing.B, podCount int) {
	n := newBenchStateNode(podCount)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = n.DeepCopy()
	}
}

// BenchmarkStateNode_CopyForMutation_* measures the new copy-on-write helper used by updateForPod/cleanupForPod
// (see cluster.go's updateNodeUsageFromPod et al.). Compare directly against ShallowCopy (the floor -- no clone
// at all) and DeepCopy (the ceiling -- clones everything, including Node/NodeClaim, which CopyForMutation never
// touches) above: CopyForMutation should land much closer to ShallowCopy.
func BenchmarkStateNode_CopyForMutation_0Pods(b *testing.B)   { benchmarkCopyForMutation(b, 0) }
func BenchmarkStateNode_CopyForMutation_20Pods(b *testing.B)  { benchmarkCopyForMutation(b, 20) }
func BenchmarkStateNode_CopyForMutation_100Pods(b *testing.B) { benchmarkCopyForMutation(b, 100) }

func benchmarkCopyForMutation(b *testing.B, podCount int) {
	n := newBenchStateNode(podCount)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = n.CopyForMutation()
	}
}
