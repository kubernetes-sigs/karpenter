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

// TestSnapshotIsolation_PodUnbinding verifies the mirror of TestSnapshotIsolation_PodBinding: a snapshot taken
// while a pod is bound must not observe that pod being unbound afterward. This exercises
// updateNodeUsageFromPodCompletion (DeletePod), the other half of the pod-usage copy-on-write path --
// TestSnapshotIsolation_PodBinding only covers the bind side.
func TestSnapshotIsolation_PodUnbinding(t *testing.T) {
	cluster, nodes, ctx := newRegressionCluster(t, 1)

	pod := regressionPod(nodes[0].Name, 0)
	if err := cluster.UpdatePod(ctx, pod); err != nil {
		t.Fatalf("binding pod, %s", err)
	}
	wantCPU := resource.MustParse("100m")

	before := cluster.Snapshot()
	beforeRequests := before[0].PodRequests()
	if got := beforeRequests.Cpu(); got.Cmp(wantCPU) != 0 {
		t.Fatalf("expected %s requested cpu before unbind, got %s", wantCPU.String(), got.String())
	}

	cluster.DeletePod(types.NamespacedName{Namespace: pod.Namespace, Name: pod.Name})

	after := cluster.Snapshot()
	afterRequests := after[0].PodRequests()
	if got := afterRequests.Cpu(); !got.IsZero() {
		t.Fatalf("expected zero requested cpu after unbind, got %s", got.String())
	}

	// The snapshot taken before the unbind must still show the pod as bound -- it's frozen, not a live view.
	beforeRequestsAgain := before[0].PodRequests()
	if got := beforeRequestsAgain.Cpu(); got.Cmp(wantCPU) != 0 {
		t.Fatalf("snapshot taken before unbind must remain unchanged, got %s", got.String())
	}
}

// TestSnapshotIsolation_UpdateNode verifies that a snapshot taken before an UpdateNode call (e.g. the node's
// labels or allocatable capacity changing) does not observe that update -- newStateFromNode always builds a new
// *StateNode, but this guards against a future change that mutates the existing one in place instead.
func TestSnapshotIsolation_UpdateNode(t *testing.T) {
	cluster, nodes, ctx := newRegressionCluster(t, 1)

	before := cluster.Snapshot()
	beforeNode := before[0].Node
	if _, ok := beforeNode.Labels["updated"]; ok {
		t.Fatalf("expected node to not have the 'updated' label before UpdateNode")
	}

	updated := nodes[0].DeepCopy()
	updated.Labels["updated"] = "true"
	if err := cluster.UpdateNode(ctx, updated); err != nil {
		t.Fatalf("updating node, %s", err)
	}

	after := cluster.Snapshot()
	if _, ok := after[0].Node.Labels["updated"]; !ok {
		t.Fatalf("expected node to have the 'updated' label in a snapshot taken after UpdateNode")
	}
	// The snapshot taken before the update -- including the *StateNode.Node pointer it captured -- must be
	// unaffected by the later UpdateNode call.
	if _, ok := beforeNode.Labels["updated"]; ok {
		t.Fatalf("snapshot taken before UpdateNode must remain unchanged")
	}
}

// TestSnapshotIsolation_UpdateNodeClaim verifies the same isolation property as
// TestSnapshotIsolation_UpdateNode, for the NodeClaim side (newStateFromNodeClaim).
func TestSnapshotIsolation_UpdateNodeClaim(t *testing.T) {
	cluster, _, _ := newRegressionCluster(t, 1)

	before := cluster.Snapshot()
	beforeNodeClaim := before[0].NodeClaim
	if _, ok := beforeNodeClaim.Labels["updated"]; ok {
		t.Fatalf("expected nodeclaim to not have the 'updated' label before UpdateNodeClaim")
	}

	updated := before[0].NodeClaim.DeepCopy()
	updated.Labels["updated"] = "true"
	cluster.UpdateNodeClaim(updated)

	after := cluster.Snapshot()
	if _, ok := after[0].NodeClaim.Labels["updated"]; !ok {
		t.Fatalf("expected nodeclaim to have the 'updated' label in a snapshot taken after UpdateNodeClaim")
	}
	if _, ok := beforeNodeClaim.Labels["updated"]; ok {
		t.Fatalf("snapshot taken before UpdateNodeClaim must remain unchanged")
	}
}

// TestSnapshotIsolation_DeleteNodeClaim_PartialCleanup guards against a real bug found while implementing the
// generation counter: cleanupNodeClaim's "Node still exists" branch used to do `c.nodes[id].NodeClaim = nil`
// directly on the live map entry instead of swapping in a fresh copy. This verifies a snapshot taken before
// DeleteNodeClaim doesn't observe the NodeClaim being cleared out from under it.
func TestSnapshotIsolation_DeleteNodeClaim_PartialCleanup(t *testing.T) {
	cluster, _, _ := newRegressionCluster(t, 1)

	before := cluster.Snapshot()
	if before[0].NodeClaim == nil {
		t.Fatalf("expected nodeclaim to be present before DeleteNodeClaim")
	}
	nodeClaimName := before[0].NodeClaim.Name

	// The Node still exists after this -- only the NodeClaim side is removed, taking the "partial cleanup"
	// branch (ShallowCopy + nil out NodeClaim) rather than the "delete from map entirely" branch.
	cluster.DeleteNodeClaim(nodeClaimName)

	after := cluster.Snapshot()
	if len(after) != 1 {
		t.Fatalf("expected the node to still be tracked (Node side survives), got %d nodes", len(after))
	}
	if after[0].NodeClaim != nil {
		t.Fatalf("expected nodeclaim to be nil in a snapshot taken after DeleteNodeClaim")
	}
	// The snapshot taken before the delete must still see the NodeClaim -- it's frozen, not a live view.
	if before[0].NodeClaim == nil {
		t.Fatalf("snapshot taken before DeleteNodeClaim must remain unchanged")
	}
	if before[0].NodeClaim.Name != nodeClaimName {
		t.Fatalf("snapshot taken before DeleteNodeClaim must still reference the original nodeclaim")
	}
}

// TestSnapshotIsolation_DeleteNode_FullRemoval verifies the other cleanupNodeClaim/cleanupNode branch: when
// there's nothing left to partially clean up (the NodeClaim side is already gone), the entry is deleted from
// c.nodes entirely. A snapshot taken beforehand must still show the now-fully-removed node.
func TestSnapshotIsolation_DeleteNode_FullRemoval(t *testing.T) {
	cluster, nodes, _ := newRegressionCluster(t, 1)

	before := cluster.Snapshot()
	if len(before) != 1 {
		t.Fatalf("expected 1 node before delete, got %d", len(before))
	}

	// Delete the NodeClaim first so DeleteNode's cleanupNode takes the "nothing left, delete from map" branch
	// instead of the partial-cleanup branch (already covered by TestSnapshotIsolation_DeleteNodeClaim_PartialCleanup).
	cluster.DeleteNodeClaim(before[0].NodeClaim.Name)
	cluster.DeleteNode(nodes[0].Name)

	after := cluster.Snapshot()
	if len(after) != 0 {
		t.Fatalf("expected 0 nodes after DeleteNode removes the fully-cleaned-up entry, got %d", len(after))
	}
	// The snapshot taken before either delete must be completely unaffected.
	if len(before) != 1 || before[0].Node == nil || before[0].NodeClaim == nil {
		t.Fatalf("snapshot taken before DeleteNodeClaim/DeleteNode must remain unchanged")
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
