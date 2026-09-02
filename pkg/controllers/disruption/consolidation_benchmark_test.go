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

package disruption

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/client-go/tools/record"
	clock "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakecr "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
	"sigs.k8s.io/karpenter/pkg/controllers/dynamicresources/deviceallocation"
	"sigs.k8s.io/karpenter/pkg/controllers/provisioning"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/events"
	"sigs.k8s.io/karpenter/pkg/operator/logging"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/state/virtualpods"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/utils/testing"
)

func init() {
	log.SetLogger(logging.NopLogger)
}

//nolint:gosec
var benchRand = rand.New(rand.NewSource(42))

// To run the consolidation benchmarks:
//
//	go test -tags=test_performance -run=XXX -bench=. ./pkg/controllers/disruption/
//
// To compare before/after with benchstat:
//
//	go test -tags=test_performance -run=XXX -bench=. -count=10 ./pkg/controllers/disruption/ | tee /tmp/old
//	# make changes
//	go test -tags=test_performance -run=XXX -bench=. -count=10 ./pkg/controllers/disruption/ | tee /tmp/new
//	benchstat /tmp/old /tmp/new
//
// These benchmarks exercise the SimulateScheduling path, which is the hot path
// for consolidation. This is where PR#2671's regression occurred: adding per-pod
// NodePool compatibility checks inside the topology domain evaluation loop.
//
// Sub-benchmarks are named vector=<name>/<param>=<n>/... where "vector" is the
// primary axis being swept and remaining path segments pin the other axes as
// filters. This shape is compatible with slope-based benchstat gates (see PR
// #3299's perf_fixtures_test.go) which parse the vector to group points and
// gate on per-vector growth slope instead of a single-point cost. The current
// 20% p<0.05 gate is level-based per sub-benchmark; the vector naming is
// forward-compatible with an eventual slope gate.

type benchConfig struct {
	nodeCount              int
	podsPerNode            int
	nodePoolCount          int
	topologySpreadFraction float64
}

// BenchmarkConsolidation is the top-level benchmark for consolidation
// SimulateScheduling. Sub-benchmarks are organized by vector:
//
//   - vector=nodes: sweep node count at fixed (np=1, topo=none). Detects
//     per-node cost scaling and node-index build regressions.
//   - vector=nodepools: sweep NodePool count at fixed (topo=hostname). This
//     is the R1 replay axis — the np=9/n=100 case is the #2671 replay
//     target that trips the 20% wall-time gate at p=0.002.
//   - vector=topology: sweep topology spread fraction. Currently a single
//     data point at frac=50 (n=500, np=1) for measurement-only coverage.
//
// 500-node sub-benchmarks are gated by testing.Short() to avoid CI timeouts
// on shared runners; they run in local profiling.
func BenchmarkConsolidation(b *testing.B) {
	// vector=nodes: sweep node count at (topo=none, np=1).
	for _, n := range []int{10, 50, 100, 500} {
		cfg := benchConfig{nodeCount: n, podsPerNode: 10, nodePoolCount: 1, topologySpreadFraction: 0.0}
		b.Run(fmt.Sprintf("vector=nodes/n=%d/topo=none/np=1", n), func(b *testing.B) {
			if n >= 500 && testing.Short() {
				b.Skip("skipping 500-node benchmark in short mode")
			}
			benchmarkConsolidationSim(b, cfg)
		})
	}

	// vector=nodepools at n=100 (topo=hostname). The np=9 case is the #2671
	// (R1) regression pattern: O(pods * domains * NodePools).
	for _, np := range []int{1, 3, 9} {
		cfg := benchConfig{nodeCount: 100, podsPerNode: 10, nodePoolCount: np, topologySpreadFraction: 1.0}
		b.Run(fmt.Sprintf("vector=nodepools/np=%d/topo=hostname/n=100", np), func(b *testing.B) {
			benchmarkConsolidationSim(b, cfg)
		})
	}

	// vector=nodepools at n=500 (topo=hostname), short-gated. Extends the
	// nodepools sweep to larger cluster scale for local profiling.
	for _, np := range []int{3, 9} {
		cfg := benchConfig{nodeCount: 500, podsPerNode: 10, nodePoolCount: np, topologySpreadFraction: 1.0}
		b.Run(fmt.Sprintf("vector=nodepools/np=%d/topo=hostname/n=500", np), func(b *testing.B) {
			if testing.Short() {
				b.Skip("skipping 500-node benchmark in short mode")
			}
			benchmarkConsolidationSim(b, cfg)
		})
	}

	// vector=topology at (n=500, np=1). Half-fraction topology spread.
	b.Run("vector=topology/frac=50/n=500/np=1", func(b *testing.B) {
		if testing.Short() {
			b.Skip("skipping 500-node benchmark in short mode")
		}
		benchmarkConsolidationSim(b, benchConfig{nodeCount: 500, podsPerNode: 10, nodePoolCount: 1, topologySpreadFraction: 0.5})
	})
}

// --- Implementation ---

// cachedBench holds the heavy setup fixture built by setupConsolidationBench
// so that -count=N re-invocations of the same benchConfig can reuse it.
// The ctx is deliberately NOT cached: TestContextWithLogger binds a zaptest
// logger to a specific *testing.B via t.Cleanup(), so using a stale ctx from
// a finished testing.B in a later invocation would emit logs on a completed
// test frame. We re-derive ctx per invocation (cheap) and share only the
// expensive-to-build objects.
type cachedBench struct {
	kubeClient   client.Client
	clk          *clock.FakeClock
	clusterState *state.Cluster
	prov         *provisioning.Provisioner
	candidates   []*Candidate
}

// benchCache memoizes setupConsolidationBench output keyed on benchConfig so
// that -count=N re-invocations of the same sub-benchmark share the fixture
// built by the first invocation.
//
// Safety: SimulateScheduling is read-only over its inputs on the timed path
// (verified in pkg/controllers/disruption/helpers.go:53-154 as of PR 2998's
// tip):
//
//   - cluster.DeepCopyNodes() (helpers.go:57) makes a fresh copy of node
//     state before use; clusterState itself is not mutated.
//   - provisioner.GetPendingPods reads pending pods (spec.nodeName="") from
//     kubeClient — but the bench pods are all pre-scheduled onto nodes, so
//     the pending-pod list is empty and the p.Validate/MarkPodScheduling
//     Decisions branch never fires. See provisioner.go:196.
//   - pdb.NewLimits and deletingNodes.CurrentlyReschedulablePods only read.
//   - provisioner.NewScheduler is constructed fresh per call, and Solve
//     mutates only its own local scheduling state; the input pods,
//     stateNodes, and cluster/kubeClient pointers are not written back to.
//
// Because the current b.N inner loop already reuses these fixtures across
// 100+ SimulateScheduling calls per BenchmarkFn invocation without affecting
// correctness or ns/op stability, extending that reuse across -count=N
// invocations is semantically equivalent to what b.N already does — the
// harness just gets more samples per computed fixture.
//
// If a mutating consolidation-related function is added to the timed path
// in the future (e.g. Scheduler.Solve becomes stateful over clusterState, a
// new SimulateScheduling variant writes back to kubeClient, or p.Validate
// starts firing in the bench pod shape), this cache MUST be revisited or
// removed. The load-bearing invariant is: the timed path is read-only over
// kubeClient / clusterState / candidates / prov.
var benchCache sync.Map // benchConfig → *cachedBench

// setupOrLoadBench returns a fresh ctx (always) and either the cached
// fixture for cfg or a freshly built one that is then stored in the cache.
// sync.Map.LoadOrStore handles the race case where two goroutines
// concurrently miss (defensive — bench iterations are sequential by
// default). See benchCache doc for the safety invariant that makes sharing
// setup across -count=N safe.
func setupOrLoadBench(b *testing.B, cfg benchConfig) (context.Context, *cachedBench) {
	// Always derive ctx fresh: it holds a zaptest logger bound to THIS b via
	// t.Cleanup(), so it must not outlive the current invocation.
	ctx := TestContextWithLogger(b)
	ctx = options.ToContext(ctx, test.Options())

	if v, ok := benchCache.Load(cfg); ok {
		return ctx, v.(*cachedBench)
	}
	// Cache miss: do the heavy setup. We discard setupConsolidationBench's
	// internal ctx (bound to the FIRST b that populated the cache) since
	// subsequent cache hits use our fresh ctx anyway.
	_, kubeClient, clk, clusterState, prov, candidates := setupConsolidationBench(b, cfg)
	fresh := &cachedBench{
		kubeClient:   kubeClient,
		clk:          clk,
		clusterState: clusterState,
		prov:         prov,
		candidates:   candidates,
	}
	actual, _ := benchCache.LoadOrStore(cfg, fresh)
	return ctx, actual.(*cachedBench)
}

func benchmarkConsolidationSim(b *testing.B, cfg benchConfig) {
	// Setup is cached per benchConfig across -count=N invocations; b.ResetTimer
	// below excludes both the fresh-ctx derivation and any cache-miss setup
	// from the timed section. See benchCache for the safety invariant.
	ctx, setup := setupOrLoadBench(b, cfg)
	rec := events.NewRecorder(&record.FakeRecorder{})

	// Benchmark SimulateScheduling for a single candidate node removal.
	candidate := setup.candidates[0]

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = SimulateScheduling(ctx, setup.kubeClient, setup.clusterState, setup.prov, setup.clk, rec, nil, candidate)
	}
	b.ReportMetric(float64(len(candidate.reschedulablePods)), "pods")
	b.ReportMetric(float64(cfg.nodeCount), "nodes")
	b.ReportMetric(float64(cfg.nodePoolCount), "nodepools")
	b.ReportMetric(cfg.topologySpreadFraction*100, "topo%")
}

// --- Setup ---

// Parameters (node counts, TSC fraction, pod requests) are intentionally
// deterministic and seeded so benchstat can compare runs across commits with
// low variance. Randomizing configuration inside a microbenchmark defeats that
// signal; broader coverage belongs in the kind-cluster benchmarks (PR #2994).
func setupConsolidationBench(b *testing.B, cfg benchConfig) (
	context.Context, client.Client, *clock.FakeClock, *state.Cluster,
	*provisioning.Provisioner, []*Candidate,
) {
	b.Helper()
	ctx := TestContextWithLogger(b)
	ctx = options.ToContext(ctx, test.Options())

	cp := fake.NewCloudProvider()
	clk := clock.NewFakeClock(time.Now())
	instanceTypes := fake.InstanceTypes(100)
	cp.InstanceTypes = instanceTypes

	// The fake client needs the spec.nodeName field index registered so that
	// GetProvisionablePods and StateNode.Pods (both use a field selector) work
	// against the in-memory store; NewFakeClient() alone doesn't provide it.
	kubeClient := fakecr.NewClientBuilder().
		WithIndex(&corev1.Pod{}, "spec.nodeName", func(o client.Object) []string {
			return []string{o.(*corev1.Pod).Spec.NodeName}
		}).
		Build()
	clusterState := state.NewCluster(clk, kubeClient, cp)
	rec := events.NewRecorder(&record.FakeRecorder{})
	prov := provisioning.NewProvisioner(kubeClient, rec, cp, clusterState, clk, deviceallocation.NewController(kubeClient), virtualpods.NewVirtualPodCache(kubeClient))

	nodePools := make([]*v1.NodePool, cfg.nodePoolCount)
	for i := 0; i < cfg.nodePoolCount; i++ {
		np := test.NodePool(v1.NodePool{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("pool-%d", i)},
			Spec: v1.NodePoolSpec{
				Limits: v1.Limits{
					corev1.ResourceCPU:    resource.MustParse("100000"),
					corev1.ResourceMemory: resource.MustParse("100000Gi"),
				},
			},
		})
		nodePools[i] = np
		if err := kubeClient.Create(ctx, np); err != nil {
			b.Fatal(err)
		}
	}

	candidates := make([]*Candidate, 0, cfg.nodeCount)
	for i := 0; i < cfg.nodeCount; i++ {
		candidates = append(candidates, addCandidateNode(b, ctx, kubeClient, clusterState, cfg, i,
			nodePools[i%cfg.nodePoolCount], instanceTypes[i%len(instanceTypes)]))
	}

	return ctx, kubeClient, clk, clusterState, prov, candidates
}

// addCandidateNode creates one NodeClaim/Node/pods in the fake client, updates
// cluster state, and returns a Candidate pointing at the resulting StateNode.
func addCandidateNode(b *testing.B, ctx context.Context, kubeClient client.Client, clusterState *state.Cluster,
	cfg benchConfig, i int, np *v1.NodePool, it *cloudprovider.InstanceType,
) *Candidate {
	zone := fmt.Sprintf("zone-%d", i%3)
	alloc := corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("16"),
		corev1.ResourceMemory: resource.MustParse("64Gi"),
		corev1.ResourcePods:   resource.MustParse("110"),
	}
	nodeClaim, node := test.NodeClaimAndNode(v1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{
				v1.NodePoolLabelKey:            np.Name,
				corev1.LabelInstanceTypeStable: it.Name,
				corev1.LabelTopologyZone:       zone,
				v1.CapacityTypeLabelKey:        v1.CapacityTypeOnDemand,
			},
		},
		Status: v1.NodeClaimStatus{
			ProviderID:  fmt.Sprintf("fake://node-%d", i),
			Capacity:    alloc,
			Allocatable: alloc,
		},
	})
	// Mirror ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated: mark the
	// NodeClaim and Node as launched/registered/initialized so cluster state
	// treats them as active capacity available for rescheduling. Without this,
	// SimulateScheduling sees an empty cluster and returns fast.
	nodeClaim.StatusConditions().SetTrue(v1.ConditionTypeLaunched)
	nodeClaim.StatusConditions().SetTrue(v1.ConditionTypeRegistered)
	nodeClaim.StatusConditions().SetTrue(v1.ConditionTypeInitialized)
	node.Spec.Taints = lo.Reject(node.Spec.Taints, func(t corev1.Taint, _ int) bool {
		return t.MatchTaint(&v1.UnregisteredNoExecuteTaint)
	})
	node.Labels[v1.NodeRegisteredLabelKey] = "true"
	node.Labels[v1.NodeInitializedLabelKey] = "true"

	if err := kubeClient.Create(ctx, nodeClaim); err != nil {
		b.Fatal(err)
	}
	if err := kubeClient.Create(ctx, node); err != nil {
		b.Fatal(err)
	}
	clusterState.UpdateNodeClaim(nodeClaim)
	if err := clusterState.UpdateNode(ctx, node); err != nil {
		b.Fatal(err)
	}

	pods := makeBenchPods(cfg.podsPerNode, cfg.topologySpreadFraction, node.Name)
	for _, p := range pods {
		if err := kubeClient.Create(ctx, p); err != nil {
			b.Fatal(err)
		}
		if err := clusterState.UpdatePod(ctx, p); err != nil {
			b.Fatal(err)
		}
	}

	// Grab the StateNode that cluster.DeepCopyNodes will return so the
	// candidate name filter in SimulateScheduling matches (see helpers.go).
	var sn *state.StateNode
	for n := range clusterState.Nodes() {
		if n.Node != nil && n.Node.Name == node.Name {
			sn = n
			break
		}
	}
	if sn == nil {
		b.Fatalf("state node for %s not found after UpdateNode", node.Name)
	}

	return &Candidate{
		StateNode:         sn,
		instanceType:      it,
		NodePool:          np,
		zone:              zone,
		capacityType:      v1.CapacityTypeOnDemand,
		reschedulablePods: pods,
	}
}

func makeBenchPods(count int, topologyFraction float64, nodeName string) []*corev1.Pod {
	pods := make([]*corev1.Pod, count)
	topologyCount := int(float64(count) * topologyFraction)

	for i := 0; i < count; i++ {
		opts := test.PodOptions{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{"app": fmt.Sprintf("bench-%d", i%5)},
				UID:    uuid.NewUUID(),
			},
			NodeName: nodeName,
			ResourceRequirements: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse(fmt.Sprintf("%dm", 100+benchRand.Intn(400))),
					corev1.ResourceMemory: resource.MustParse(fmt.Sprintf("%dMi", 128+benchRand.Intn(512))),
				},
			},
		}
		if i < topologyCount {
			opts.TopologySpreadConstraints = []corev1.TopologySpreadConstraint{{
				MaxSkew:           1,
				TopologyKey:       corev1.LabelHostname,
				WhenUnsatisfiable: corev1.DoNotSchedule,
				LabelSelector:     &metav1.LabelSelector{MatchLabels: map[string]string{"app": fmt.Sprintf("bench-%d", i%5)}},
			}}
		}
		pods[i] = test.Pod(opts)
	}
	return pods
}
