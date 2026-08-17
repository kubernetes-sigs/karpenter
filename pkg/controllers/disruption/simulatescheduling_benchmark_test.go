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

// These benchmarks quantify the deep-copy reduction made in helpers.go/controller.go: SimulateScheduling and
// GetCandidatesWithTotals now take a pre-taken `nodes state.StateNodes` snapshot instead of each independently
// calling cluster.DeepCopyNodes(). The "FreshCopyPerCall" variants reproduce the OLD behavior (take a fresh deep
// copy on every call) using the current, real SimulateScheduling signature; the "SharedSnapshot" variants
// reproduce the NEW behavior (take one copy, reuse it). The only difference between each pair is where
// cluster.DeepCopyNodes() is called relative to the b.N loop, so the delta directly isolates the win.
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

func BenchmarkSimulateScheduling_FreshCopyPerCall_25(b *testing.B) {
	benchmarkSimulateSchedulingFreshCopy(b, 25)
}
func BenchmarkSimulateScheduling_FreshCopyPerCall_100(b *testing.B) {
	benchmarkSimulateSchedulingFreshCopy(b, 100)
}
func BenchmarkSimulateScheduling_FreshCopyPerCall_400(b *testing.B) {
	benchmarkSimulateSchedulingFreshCopy(b, 400)
}
func BenchmarkSimulateScheduling_FreshCopyPerCall_5000(b *testing.B) {
	benchmarkSimulateSchedulingFreshCopy(b, 5000)
}

func BenchmarkSimulateScheduling_SharedSnapshot_25(b *testing.B) {
	benchmarkSimulateSchedulingShared(b, 25)
}
func BenchmarkSimulateScheduling_SharedSnapshot_100(b *testing.B) {
	benchmarkSimulateSchedulingShared(b, 100)
}
func BenchmarkSimulateScheduling_SharedSnapshot_400(b *testing.B) {
	benchmarkSimulateSchedulingShared(b, 400)
}
func BenchmarkSimulateScheduling_SharedSnapshot_5000(b *testing.B) {
	benchmarkSimulateSchedulingShared(b, 5000)
}

// benchmarkSimulateSchedulingFreshCopy reproduces the pre-change behavior: every call takes its own independent
// deep copy of cluster state before simulating.
func benchmarkSimulateSchedulingFreshCopy(b *testing.B, numNodes int) {
	f := setupSimulateSchedulingBenchFixture(b, numNodes)
	defer func() { _ = f.env.Stop() }()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		nodes := f.cluster.DeepCopyNodes() // fresh copy every iteration -- the old behavior
		if _, err := disruption.SimulateScheduling(f.ctx, f.env.Client, f.cluster, f.prov, f.env.Clock, f.recorder, nodes, nil, f.candidate); err != nil {
			b.Fatalf("simulating scheduling, %s", err)
		}
	}
	b.StopTimer() // exclude envtest teardown (in the deferred env.Stop() above) from the measured time
}

// benchmarkSimulateSchedulingShared reproduces the post-change behavior: one deep copy is taken up front and
// shared across every call, exactly as the reconcile-cycle snapshot is shared across a method's simulations today.
func benchmarkSimulateSchedulingShared(b *testing.B, numNodes int) {
	f := setupSimulateSchedulingBenchFixture(b, numNodes)
	defer func() { _ = f.env.Stop() }()

	nodes := f.cluster.DeepCopyNodes() // taken once, shared across every iteration -- the new behavior
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := disruption.SimulateScheduling(f.ctx, f.env.Client, f.cluster, f.prov, f.env.Clock, f.recorder, nodes, nil, f.candidate); err != nil {
			b.Fatalf("simulating scheduling, %s", err)
		}
	}
	b.StopTimer() // exclude envtest teardown (in the deferred env.Stop() above) from the measured time
}

// The following two benchmarks isolate the primitive swapped in BuildDisruptionBudgetMapping: a full
// cluster.DeepCopyNodes() versus the read-locked, zero-copy cluster.Nodes() iterator.

func BenchmarkClusterDeepCopyNodes_25(b *testing.B)   { benchmarkClusterDeepCopyNodes(b, 25) }
func BenchmarkClusterDeepCopyNodes_100(b *testing.B)  { benchmarkClusterDeepCopyNodes(b, 100) }
func BenchmarkClusterDeepCopyNodes_400(b *testing.B)  { benchmarkClusterDeepCopyNodes(b, 400) }
func BenchmarkClusterDeepCopyNodes_5000(b *testing.B) { benchmarkClusterDeepCopyNodes(b, 5000) }

func BenchmarkClusterNodesIterate_25(b *testing.B)   { benchmarkClusterNodesIterate(b, 25) }
func BenchmarkClusterNodesIterate_100(b *testing.B)  { benchmarkClusterNodesIterate(b, 100) }
func BenchmarkClusterNodesIterate_400(b *testing.B)  { benchmarkClusterNodesIterate(b, 400) }
func BenchmarkClusterNodesIterate_5000(b *testing.B) { benchmarkClusterNodesIterate(b, 5000) }

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
