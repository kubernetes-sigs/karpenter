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

package disruption_test

import (
	"context"
	"testing"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	coreapis "sigs.k8s.io/karpenter/pkg/apis"
	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
	"sigs.k8s.io/karpenter/pkg/controllers/disruption"
	"sigs.k8s.io/karpenter/pkg/controllers/dynamicresources/deviceallocation"
	"sigs.k8s.io/karpenter/pkg/controllers/provisioning"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/controllers/state/informer"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/state/cost"
	"sigs.k8s.io/karpenter/pkg/state/virtualpods"
	"sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
	"sigs.k8s.io/karpenter/pkg/test/v1alpha1"
	"sigs.k8s.io/karpenter/pkg/utils/pdb"
)

// These benchmarks measure SimulateScheduling end-to-end at increasing cluster sizes. Earlier revisions of this
// file compared a "FreshCopyPerCall" variant against a "SharedSnapshot" variant, back when SimulateScheduling
// took an explicit `nodes state.StateNodes` parameter that callers controlled. That distinction no longer
// applies: SimulateScheduling now always calls cluster.Snapshot() internally, which is cheap enough (see
// Cluster.Snapshot's doc comment and BenchmarkSnapshot_*/BenchmarkPointerSliceCopy_* in
// pkg/controllers/state/snapshot_benchmark_test.go) that there's no meaningful difference between "fresh" and
// "shared" anymore -- every call already gets the cheapest possible up-to-date view.
//
// To run:
//
//	KUBEBUILDER_ASSETS=<path> go test -tags=test_performance -run=XXX -bench=. ./pkg/controllers/disruption/... -benchtime=20x
//
// To compare before/after a further change to this path:
//
//	go test -tags=test_performance -run=XXX -bench=. -count=10 ./pkg/controllers/disruption/... | tee /tmp/old
//	# make your changes
//	go test -tags=test_performance -run=XXX -bench=. -count=10 ./pkg/controllers/disruption/... | tee /tmp/new
//	benchstat /tmp/old /tmp/new
func init() {
	// Benchmarks don't run through Ginkgo's RunSpecs, so the fail handler that ExpectApplied/
	// ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated rely on (via Gomega's Expect) is never registered
	// unless we do it ourselves.
	gomega.RegisterFailHandler(ginkgo.Fail)
}

type simulateSchedulingBenchFixture struct {
	ctx       context.Context
	env       *test.Environment
	cluster   *state.Cluster
	prov      *provisioning.Provisioner
	recorder  *test.EventRecorder
	candidate *disruption.Candidate
}

// setupSimulateSchedulingBenchFixture builds a NodePool with numNodes candidate nodes, all initialized and
// registered in cluster state, and constructs a single disruption Candidate for node[0] -- mirroring the setup
// used by the "Simulate Scheduling" Ginkgo specs in suite_test.go, but standalone (no BeforeSuite/BeforeEach).
func setupSimulateSchedulingBenchFixture(b *testing.B, numNodes int) *simulateSchedulingBenchFixture {
	b.Helper()
	ctx := context.Background()
	env := test.NewEnvironment(test.WithCRDs(coreapis.CRDs...), test.WithCRDs(v1alpha1.CRDs...))
	ctx = options.ToContext(ctx, test.Options())

	cloudProvider := fake.NewCloudProvider()
	cloudProvider.InstanceTypes = fake.InstanceTypesAssorted()
	clusterCost := cost.NewClusterCost(ctx, cloudProvider, env.Client)
	cluster := state.NewCluster(env.Clock, env.Client, cloudProvider)
	nodeStateController := informer.NewNodeController(env.Client, cluster)
	nodeClaimStateController := informer.NewNodeClaimController(env.Client, cloudProvider, cluster, clusterCost)
	recorder := test.NewEventRecorder()
	draController := deviceallocation.NewController(env.Client)
	prov := provisioning.NewProvisioner(env.Client, recorder, cloudProvider, cluster, env.Clock, draController, virtualpods.NewVirtualPodCache(env.Client))
	queue := disruption.NewQueue(env.Client, recorder, cluster, env.Clock, prov)

	nodePool := test.NodePool(v1.NodePool{
		Spec: v1.NodePoolSpec{
			Disruption: v1.Disruption{
				ConsolidateAfter:    v1.MustParseNillableDuration("0s"),
				ConsolidationPolicy: v1.ConsolidationPolicyWhenEmptyOrUnderutilized,
			},
		},
	})
	ExpectApplied(ctx, env.Client, nodePool)

	instanceType := cloudProvider.InstanceTypes[0]
	offering := instanceType.Offerings[0]
	nodeClaims, nodes := test.NodeClaimsAndNodes(numNodes, v1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{
				v1.NodePoolLabelKey:            nodePool.Name,
				corev1.LabelInstanceTypeStable: instanceType.Name,
				v1.CapacityTypeLabelKey:        offering.Requirements.Get(v1.CapacityTypeLabelKey).Any(),
				corev1.LabelTopologyZone:       offering.Requirements.Get(corev1.LabelTopologyZone).Any(),
			},
		},
		Status: v1.NodeClaimStatus{
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:  resource.MustParse("32"),
				corev1.ResourcePods: resource.MustParse("100"),
			},
		},
	})
	for i := range numNodes {
		ExpectApplied(ctx, env.Client, nodeClaims[i], nodes[i])
	}
	ExpectMakeNodesAndNodeClaimsInitializedAndStateUpdated(ctx, env.Client, env.Clock, nodeStateController, nodeClaimStateController, nodes, nodeClaims)

	pdbs, err := pdb.NewLimits(ctx, env.Client)
	if err != nil {
		b.Fatalf("building pdb limits, %s", err)
	}
	nodePoolMap, nodePoolToInstanceTypesMap, err := disruption.BuildNodePoolMap(ctx, env.Client, cloudProvider)
	if err != nil {
		b.Fatalf("building nodepool map, %s", err)
	}
	stateNode := ExpectStateNodeExists(cluster, nodes[0])
	candidate, err := disruption.NewCandidate(ctx, env.Client, recorder, env.Clock, stateNode, pdbs, nodePoolMap, nodePoolToInstanceTypesMap, queue, disruption.GracefulDisruptionClass)
	if err != nil {
		b.Fatalf("constructing candidate, %s", err)
	}

	return &simulateSchedulingBenchFixture{
		ctx:       ctx,
		env:       env,
		cluster:   cluster,
		prov:      prov,
		recorder:  recorder,
		candidate: candidate,
	}
}

func BenchmarkSimulateScheduling_25(b *testing.B)  { benchmarkSimulateScheduling(b, 25) }
func BenchmarkSimulateScheduling_100(b *testing.B) { benchmarkSimulateScheduling(b, 100) }
func BenchmarkSimulateScheduling_400(b *testing.B) { benchmarkSimulateScheduling(b, 400) }

func benchmarkSimulateScheduling(b *testing.B, numNodes int) {
	f := setupSimulateSchedulingBenchFixture(b, numNodes)
	defer func() { _ = f.env.Stop() }()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := disruption.SimulateScheduling(f.ctx, f.env.Client, f.cluster, f.prov, f.env.Clock, f.recorder, nil, f.candidate); err != nil {
			b.Fatalf("simulating scheduling, %s", err)
		}
	}
	b.StopTimer() // exclude envtest teardown (in the deferred env.Stop() above) from the measured time
}

// The following two benchmarks compare cluster.DeepCopyNodes() (now an alias for the cheap, generation-cached
// Cluster.Snapshot()) against the read-locked, zero-allocation cluster.Nodes() iterator used by
// BuildDisruptionBudgetMapping. See pkg/controllers/state/snapshot_benchmark_test.go for the underlying
// Cluster.Snapshot() benchmarks this end-to-end version is built on top of.

func BenchmarkClusterDeepCopyNodes_25(b *testing.B)  { benchmarkClusterDeepCopyNodes(b, 25) }
func BenchmarkClusterDeepCopyNodes_100(b *testing.B) { benchmarkClusterDeepCopyNodes(b, 100) }
func BenchmarkClusterDeepCopyNodes_400(b *testing.B) { benchmarkClusterDeepCopyNodes(b, 400) }

func BenchmarkClusterNodesIterate_25(b *testing.B)  { benchmarkClusterNodesIterate(b, 25) }
func BenchmarkClusterNodesIterate_100(b *testing.B) { benchmarkClusterNodesIterate(b, 100) }
func BenchmarkClusterNodesIterate_400(b *testing.B) { benchmarkClusterNodesIterate(b, 400) }

func benchmarkClusterDeepCopyNodes(b *testing.B, numNodes int) {
	f := setupSimulateSchedulingBenchFixture(b, numNodes)
	defer func() { _ = f.env.Stop() }()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = f.cluster.DeepCopyNodes()
	}
	b.StopTimer() // exclude envtest teardown (in the deferred env.Stop() above) from the measured time
}

func benchmarkClusterNodesIterate(b *testing.B, numNodes int) {
	f := setupSimulateSchedulingBenchFixture(b, numNodes)
	defer func() { _ = f.env.Stop() }()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		count := 0
		for range f.cluster.Nodes() {
			count++
		}
		if count != numNodes {
			b.Fatalf("expected %d nodes, got %d", numNodes, count)
		}
	}
	b.StopTimer() // exclude envtest teardown (in the deferred env.Stop() above) from the measured time
}
