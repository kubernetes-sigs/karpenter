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
	"sync"
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

// This file is deliberately not build-tagged test_performance: these are ordinary correctness tests, run as part
// of the normal test suite (`go test ./pkg/controllers/state/...`), not opt-in benchmarks.
//
// Every test here documents an invariant that must hold regardless of how Cluster.Snapshot() is implemented
// underneath -- originally proven by a full deep clone (the pre-refactor DeepCopyNodes()), now by the
// copy-on-write + generation-counter design (see cluster.go's Snapshot()). These are the regression guard for
// that refactor, run both before and after.
//
// Setup is intentionally independent of the test_performance-tagged benchmark helpers (different build tag,
// can't share code across the tag boundary) but follows the same fake-client + field-index pattern.

func newRegressionClusterClient() fakecr.Client {
	return fake.NewClientBuilder().
		WithIndex(&corev1.Pod{}, "spec.nodeName", func(o fakecr.Object) []string {
			return []string{o.(*corev1.Pod).Spec.NodeName}
		}).
		Build()
}

func newRegressionCluster(t *testing.T, numNodes int) (*state.Cluster, []*corev1.Node, context.Context) {
	t.Helper()
	ctx := options.ToContext(context.Background(), test.Options())
	client := newRegressionClusterClient()
	cluster := state.NewCluster(&clock.RealClock{}, client, cloudproviderfake.NewCloudProvider())

	nodeClaims, nodes := test.NodeClaimsAndNodes(numNodes, v1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{
				v1.NodePoolLabelKey:            "regression-nodepool",
				corev1.LabelInstanceTypeStable: "regression-instance-type",
				v1.NodeInitializedLabelKey:     "true",
				v1.NodeRegisteredLabelKey:      "true",
			},
		},
		Status: v1.NodeClaimStatus{
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("8"),
				corev1.ResourceMemory: resource.MustParse("32Gi"),
				corev1.ResourcePods:   resource.MustParse("20"),
			},
		},
	})
	for i := range numNodes {
		cluster.UpdateNodeClaim(nodeClaims[i])
		if err := cluster.UpdateNode(ctx, nodes[i]); err != nil {
			t.Fatalf("registering node %d, %s", i, err)
		}
	}
	return cluster, nodes, ctx
}

func regressionPod(nodeName string, seq int) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("pod-%d", seq), Namespace: "default"},
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

// TestSnapshotIsolation_PodBinding verifies that a snapshot taken before a pod binds to a node does not observe
// that binding, even though the live cluster does. This is the core isolation property the whole copy-on-write
// design depends on: once you're holding a snapshot, nothing that happens afterward should be visible through it.
func TestSnapshotIsolation_PodBinding(t *testing.T) {
	cluster, nodes, ctx := newRegressionCluster(t, 1)

	before := cluster.Snapshot()
	beforeRequests := before[0].PodRequests()
	if got := beforeRequests.Cpu(); !got.IsZero() {
		t.Fatalf("expected zero requested cpu before binding, got %s", got)
	}

	pod := regressionPod(nodes[0].Name, 0)
	if err := cluster.UpdatePod(ctx, pod); err != nil {
		t.Fatalf("binding pod, %s", err)
	}
	wantCPU := resource.MustParse("100m")

	// The live cluster reflects the new pod...
	after := cluster.Snapshot()
	afterRequests := after[0].PodRequests()
	if got := afterRequests.Cpu(); got.Cmp(wantCPU) != 0 {
		t.Fatalf("expected %s requested cpu after binding, got %s", wantCPU.String(), got.String())
	}

	// ...but the snapshot taken before the bind must not.
	beforeRequestsAgain := before[0].PodRequests()
	if got := beforeRequestsAgain.Cpu(); !got.IsZero() {
		t.Fatalf("snapshot taken before binding must remain unchanged, got %s", got.String())
	}
}

// TestSnapshotIsolation_Nomination verifies a snapshot doesn't observe a nomination that happens after it was
// taken, and that the node's nomination state at snapshot time is preserved.
func TestSnapshotIsolation_Nomination(t *testing.T) {
	cluster, nodes, ctx := newRegressionCluster(t, 1)
	clk := &clock.RealClock{}

	before := cluster.Snapshot()
	if before[0].Nominated(clk) {
		t.Fatalf("expected node to not be nominated before NominateNodeForPod")
	}

	cluster.NominateNodeForPod(ctx, nodes[0].Spec.ProviderID)

	afterNominate := cluster.Snapshot()
	if !afterNominate[0].Nominated(clk) {
		t.Fatalf("expected node to be nominated in a snapshot taken after NominateNodeForPod")
	}
	if before[0].Nominated(clk) {
		t.Fatalf("snapshot taken before nomination must remain unchanged")
	}
}

// TestSnapshotIsolation_MarkForDeletion verifies the same isolation property for markedForDeletion.
func TestSnapshotIsolation_MarkForDeletion(t *testing.T) {
	cluster, nodes, _ := newRegressionCluster(t, 1)

	before := cluster.Snapshot()
	if before[0].MarkedForDeletion() {
		t.Fatalf("expected node to not be marked for deletion initially")
	}

	cluster.MarkForDeletion(nodes[0].Spec.ProviderID)

	afterMark := cluster.Snapshot()
	if !afterMark[0].MarkedForDeletion() {
		t.Fatalf("expected node to be marked for deletion in a snapshot taken after MarkForDeletion")
	}
	if before[0].MarkedForDeletion() {
		t.Fatalf("snapshot taken before MarkForDeletion must remain unchanged")
	}

	cluster.UnmarkForDeletion(nodes[0].Spec.ProviderID)
	afterUnmark := cluster.Snapshot()
	if afterUnmark[0].MarkedForDeletion() {
		t.Fatalf("expected node to be unmarked after UnmarkForDeletion")
	}
	// The snapshot taken right after marking must still show marked=true -- it's frozen, not a live view.
	if !afterMark[0].MarkedForDeletion() {
		t.Fatalf("snapshot taken after MarkForDeletion (but before UnmarkForDeletion) must remain unchanged")
	}
}

// TestSnapshot_InvalidatesOnReset guards against a real regression found while implementing the generation
// counter: Cluster.Reset() reassigns c.nodes to a fresh empty map, but if it doesn't also bump c.generation,
// Snapshot()'s cache doesn't know anything changed and keeps returning the pre-Reset snapshot -- silently handing
// back nodes that no longer exist anywhere (not in the live map, and potentially not in the API server either,
// since Reset() is typically called between test specs after real cleanup). This caused a "Node not found" 404
// flake in an unrelated suite that only reproduced once cluster.Reset() was in the call path after a
// cluster.Snapshot() cache had already been populated.
func TestSnapshot_InvalidatesOnReset(t *testing.T) {
	cluster, _, _ := newRegressionCluster(t, 3)

	before := cluster.Snapshot()
	if len(before) != 3 {
		t.Fatalf("expected 3 nodes before reset, got %d", len(before))
	}

	cluster.Reset()

	after := cluster.Snapshot()
	if len(after) != 0 {
		t.Fatalf("expected 0 nodes immediately after Reset (snapshot must not serve the stale pre-Reset cache), got %d", len(after))
	}
}

// TestConcurrentReadWrite_NoRace exercises concurrent pod bind/unbind churn against concurrent Snapshot
// reads. Run with `go test -race` -- its purpose is to catch any regression that reintroduces a shared-mutation
// hazard between the read and write paths.
func TestConcurrentReadWrite_NoRace(t *testing.T) {
	cluster, nodes, ctx := newRegressionCluster(t, 10)

	const writers = 8
	const readers = 4
	const opsPerWriter = 200

	var writersWG sync.WaitGroup
	stop := make(chan struct{})

	for w := 0; w < writers; w++ {
		writersWG.Add(1)
		go func(id int) {
			defer writersWG.Done()
			for i := 0; i < opsPerWriter; i++ {
				node := nodes[i%len(nodes)]
				pod := regressionPod(node.Name, id*opsPerWriter+i)
				_ = cluster.UpdatePod(ctx, pod)
				cluster.DeletePod(types.NamespacedName{Namespace: pod.Namespace, Name: pod.Name})
				cluster.NominateNodeForPod(ctx, node.Spec.ProviderID)
			}
		}(w)
	}

	var readersWG sync.WaitGroup
	for r := 0; r < readers; r++ {
		readersWG.Add(1)
		go func() {
			defer readersWG.Done()
			for {
				select {
				case <-stop:
					return
				default:
					snap := cluster.Snapshot()
					for _, n := range snap {
						_ = n.PodRequests()
						_ = n.Nominated(&clock.RealClock{})
						_ = n.MarkedForDeletion()
					}
				}
			}
		}()
	}

	writersWG.Wait()
	close(stop)
	readersWG.Wait()
}
