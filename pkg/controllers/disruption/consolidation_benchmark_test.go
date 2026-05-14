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
	"testing"
	"time"

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
	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
	"sigs.k8s.io/karpenter/pkg/controllers/provisioning"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/events"
	"sigs.k8s.io/karpenter/pkg/operator/logging"
	"sigs.k8s.io/karpenter/pkg/operator/options"
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

type benchConfig struct {
	nodeCount              int
	podsPerNode            int
	nodePoolCount          int
	topologySpreadFraction float64
}

// --- Consolidation scheduling simulation benchmarks ---
// These benchmark the rescheduling simulation that runs when evaluating whether
// a node's pods can be placed elsewhere. This is the inner loop of consolidation.

func BenchmarkConsolidation_10Nodes_NoTopology(b *testing.B) {
	benchmarkConsolidationSim(b, benchConfig{10, 10, 1, 0.0})
}

func BenchmarkConsolidation_50Nodes_NoTopology(b *testing.B) {
	benchmarkConsolidationSim(b, benchConfig{50, 10, 1, 0.0})
}

func BenchmarkConsolidation_100Nodes_NoTopology(b *testing.B) {
	benchmarkConsolidationSim(b, benchConfig{100, 10, 1, 0.0})
}

func BenchmarkConsolidation_100Nodes_HostnameSpread(b *testing.B) {
	benchmarkConsolidationSim(b, benchConfig{100, 10, 1, 1.0})
}

func BenchmarkConsolidation_100Nodes_HostnameSpread_3NP(b *testing.B) {
	benchmarkConsolidationSim(b, benchConfig{100, 10, 3, 1.0})
}

func BenchmarkConsolidation_100Nodes_HostnameSpread_9NP(b *testing.B) {
	// This is the case that PR#2671 regressed: O(pods * domains * NodePools)
	benchmarkConsolidationSim(b, benchConfig{100, 10, 9, 1.0})
}

func BenchmarkConsolidation_500Nodes_NoTopology(b *testing.B) {
	if testing.Short() {
		b.Skip("skipping 500-node benchmark in short mode")
	}
	benchmarkConsolidationSim(b, benchConfig{500, 10, 1, 0.0})
}

func BenchmarkConsolidation_500Nodes_HalfTopology(b *testing.B) {
	if testing.Short() {
		b.Skip("skipping 500-node benchmark in short mode")
	}
	benchmarkConsolidationSim(b, benchConfig{500, 10, 1, 0.5})
}

func BenchmarkConsolidation_500Nodes_HostnameSpread_3NP(b *testing.B) {
	if testing.Short() {
		b.Skip("skipping 500-node benchmark in short mode")
	}
	benchmarkConsolidationSim(b, benchConfig{500, 10, 3, 1.0})
}

func BenchmarkConsolidation_500Nodes_HostnameSpread_9NP(b *testing.B) {
	if testing.Short() {
		b.Skip("skipping 500-node benchmark in short mode")
	}
	benchmarkConsolidationSim(b, benchConfig{500, 10, 9, 1.0})
}

// --- Implementation ---

func benchmarkConsolidationSim(b *testing.B, cfg benchConfig) {
	ctx, kubeClient, clk, clusterState, _, prov, candidates := setupConsolidationBench(b, cfg)
	rec := events.NewRecorder(&record.FakeRecorder{})

	// Benchmark SimulateScheduling for a single candidate node removal.
	// This exercises the full rescheduling path: collect pods from the candidate,
	// create a scheduler with topology constraints, and solve placement.
	candidate := candidates[0]

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = SimulateScheduling(ctx, kubeClient, clusterState, prov, clk, rec, candidate)
	}
	b.ReportMetric(float64(len(candidate.reschedulablePods)), "pods")
	b.ReportMetric(float64(cfg.nodeCount), "nodes")
	b.ReportMetric(float64(cfg.nodePoolCount), "nodepools")
	b.ReportMetric(cfg.topologySpreadFraction*100, "topo%")
}

// --- Setup ---

func setupConsolidationBench(b *testing.B, cfg benchConfig) (
	context.Context, client.Client, *clock.FakeClock, *state.Cluster,
	*fake.CloudProvider, *provisioning.Provisioner, []*Candidate,
) {
	b.Helper()
	ctx := TestContextWithLogger(b)
	ctx = options.ToContext(ctx, test.Options())

	cp := fake.NewCloudProvider()
	clk := clock.NewFakeClock(time.Now())
	instanceTypes := fake.InstanceTypes(100)
	cp.InstanceTypes = instanceTypes

	kubeClient := fakecr.NewFakeClient()
	clusterState := state.NewCluster(clk, kubeClient, cp)
	rec := events.NewRecorder(&record.FakeRecorder{})
	prov := provisioning.NewProvisioner(kubeClient, rec, cp, clusterState, clk)

	// Create NodePools
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

	// Build candidates with pods
	var candidates []*Candidate
	for i := 0; i < cfg.nodeCount; i++ {
		np := nodePools[i%cfg.nodePoolCount]
		it := instanceTypes[i%len(instanceTypes)]

		nodeClaim, node := test.NodeClaimAndNode(v1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1.NodePoolLabelKey:            np.Name,
					corev1.LabelInstanceTypeStable: it.Name,
					corev1.LabelTopologyZone:       fmt.Sprintf("zone-%d", i%3),
				},
			},
			Status: v1.NodeClaimStatus{
				ProviderID: fmt.Sprintf("fake://node-%d", i),
				Allocatable: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("16"),
					corev1.ResourceMemory: resource.MustParse("64Gi"),
					corev1.ResourcePods:   resource.MustParse("110"),
				},
			},
		})

		pods := makeBenchPods(cfg.podsPerNode, cfg.topologySpreadFraction, node.Name)

		candidates = append(candidates, &Candidate{
			StateNode: &state.StateNode{
				Node:      node,
				NodeClaim: nodeClaim,
			},
			instanceType:      it,
			NodePool:          np,
			zone:              fmt.Sprintf("zone-%d", i%3),
			capacityType:      v1.CapacityTypeOnDemand,
			reschedulablePods: pods,
		})
	}

	return ctx, kubeClient, clk, clusterState, cp, prov, candidates
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
