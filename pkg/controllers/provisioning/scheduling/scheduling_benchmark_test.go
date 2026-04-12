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

package scheduling_test

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"os"
	"runtime/pprof"
	"sort"
	"testing"
	"text/tabwriter"
	"time"

	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakecr "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
	"sigs.k8s.io/karpenter/pkg/controllers/provisioning/scheduling"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/events"
	"sigs.k8s.io/karpenter/pkg/operator/injection"
	"sigs.k8s.io/karpenter/pkg/operator/logging"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/test"
)

func init() {
	log.SetLogger(logging.NopLogger)
}

const MinPodsPerSec = 100.0
const PrintStats = false

//nolint:gosec
var r = rand.New(rand.NewSource(42))

// To run the benchmarks use:
// `go test -tags=test_performance -run=XXX -bench=.`
//
// For statistically significant before/after comparisons (recommended: sequential runs, multi-CPU):
//
//	go test -tags=test_performance -run='^$' -bench=. -count=10 -benchtime=5s -cpu=1,2,4 -benchmem | tee /tmp/old
//	# apply your fix
//	go test -tags=test_performance -run='^$' -bench=. -count=10 -benchtime=5s -cpu=1,2,4 -benchmem | tee /tmp/new
//	go run golang.org/x/perf/cmd/benchstat@latest /tmp/old /tmp/new
//
// Performance hotspot benchmarks (run a specific one for focused comparison):
//
//	# sort.Slice per pod on inflight NodeClaims (scheduler.go)
//	go test -tags=test_performance -run=XXX -bench=BenchmarkSortSliceNewNodeClaims -count=10 ./pkg/controllers/provisioning/scheduling/
//	go test -tags=test_performance -run=XXX -bench='BenchmarkScheduling(500|2000)$|BenchmarkSchedulingHighInflight' ./pkg/controllers/provisioning/scheduling/
//
//	# kubeClient.Get per node in countDomains (topology.go)
//	go test -tags=test_performance -run=XXX -bench=BenchmarkSchedulingTopologySpreadWith -count=10 ./pkg/controllers/provisioning/scheduling/
//
//	# hashstructure.Hash reflection in TopologyGroup.Hash (topologygroup.go)
//	go test -tags=test_performance -run=XXX -bench=BenchmarkTopologyGroupHash -count=10 ./pkg/controllers/provisioning/scheduling/
//
// For CPU and heap flame graphs (captures all scenarios):
//
//	go test -tags=test_performance -run=TestSchedulingProfile ./pkg/controllers/provisioning/scheduling/
//	go tool pprof -http=:8080 schedule.cpuprofile    # CPU flame graph
//	go tool pprof -http=:8080 schedule.heapprofile   # allocation flame graph
func BenchmarkScheduling1(b *testing.B) {
	benchmarkScheduler(b, makeDiversePods(1))
}
func BenchmarkScheduling50(b *testing.B) {
	benchmarkScheduler(b, makeDiversePods(50))
}
func BenchmarkScheduling100(b *testing.B) {
	benchmarkScheduler(b, makeDiversePods(100))
}
func BenchmarkScheduling500(b *testing.B) {
	benchmarkScheduler(b, makeDiversePods(500))
}
func BenchmarkScheduling1000(b *testing.B) {
	benchmarkScheduler(b, makeDiversePods(1000))
}
func BenchmarkScheduling2000(b *testing.B) {
	benchmarkScheduler(b, makeDiversePods(2000))
}
func BenchmarkScheduling5000(b *testing.B) {
	benchmarkScheduler(b, makeDiversePods(5000))
}
func BenchmarkScheduling10000(b *testing.B) {
	benchmarkScheduler(b, makeDiversePods(10000))
}
func BenchmarkScheduling20000(b *testing.B) {
	benchmarkScheduler(b, makeDiversePods(20000))
}
func BenchmarkRespectPreferences(b *testing.B) {
	benchmarkScheduler(b, makePreferencePods(4000))
}
func BenchmarkIgnorePreferences(b *testing.B) {
	benchmarkScheduler(b, makePreferencePods(4000), scheduling.IgnorePreferences)
}

// TestSchedulingProfile is used to gather profiling metrics, benchmarking is primarily done with standard
// Go benchmark functions
// go test -tags=test_performance -run=SchedulingProfile
func TestSchedulingProfile(t *testing.T) {
	tw := tabwriter.NewWriter(os.Stdout, 8, 8, 2, ' ', 0)

	cpuf, err := os.Create("schedule.cpuprofile")
	if err != nil {
		t.Fatalf("error creating CPU profile: %s", err)
	}
	lo.Must0(pprof.StartCPUProfile(cpuf))
	defer pprof.StopCPUProfile()

	heapf, err := os.Create("schedule.heapprofile")
	if err != nil {
		t.Fatalf("error creating heap profile: %s", err)
	}
	defer func() { lo.Must0(pprof.WriteHeapProfile(heapf)) }()

	totalPods := 0
	totalNodes := 0
	var totalTime time.Duration
	fmt.Fprintf(tw, "============== Generic Pods ==============\n")
	for _, podCount := range []int{1, 50, 100, 500, 1000, 1500, 2000, 5000, 10000, 20000} {
		start := time.Now()
		res := testing.Benchmark(func(b *testing.B) {
			benchmarkScheduler(b, makeDiversePods(podCount))
		})
		totalTime += time.Since(start) / time.Duration(res.N)
		nodeCount := res.Extra["nodes"]
		fmt.Fprintf(tw, "%s\t%d pods\t%d nodes\t%s per scheduling\t%s per pod\n", fmt.Sprintf("%d Pods", podCount), podCount, int(nodeCount), time.Duration(res.NsPerOp()), time.Duration(res.NsPerOp()/int64(podCount)))
		totalPods += podCount
		totalNodes += int(nodeCount)
	}
	fmt.Fprintf(tw, "============== Preference Pods ==============\n")
	for _, opt := range []scheduling.Options{nil, scheduling.IgnorePreferences} {
		start := time.Now()
		podCount := 4000
		res := testing.Benchmark(func(b *testing.B) {
			benchmarkScheduler(b, makePreferencePods(podCount), opt)
		})
		totalTime += time.Since(start) / time.Duration(res.N)
		nodeCount := res.Extra["nodes"]
		fmt.Fprintf(tw, "%s\t%d pods\t%d nodes\t%s per scheduling\t%s per pod\n", lo.Ternary(opt == nil, "PreferencePolicy=Respect", "PreferencePolicy=Ignore"), podCount, int(nodeCount), time.Duration(res.NsPerOp()), time.Duration(res.NsPerOp()/int64(podCount)))
		totalPods += podCount
		totalNodes += int(nodeCount)
	}
	fmt.Fprintf(tw, "\nscheduled %d against %d nodes in total in %s %f pods/sec\n", totalPods, totalNodes, totalTime, float64(totalPods)/totalTime.Seconds())

	// Topology Spread with pre-seeded state: exercises the countDomains kubeClient.Get path
	fmt.Fprintf(tw, "============== Topology Spread w/ Existing Nodes ==============\n")
	for _, existingCount := range []int{500, 2000} {
		podCount := 500
		newPods := makeTopologySpreadSingleKeyPods(podCount)
		preseededClient := buildPreseededClient(existingCount)
		nodePool2 := test.NodePool(v1.NodePool{
			Spec: v1.NodePoolSpec{
				Limits: v1.Limits{
					corev1.ResourceCPU:    resource.MustParse("10000000"),
					corev1.ResourceMemory: resource.MustParse("10000000Gi"),
				},
			},
		})
		cp2 := fake.NewCloudProvider()
		its2 := fake.InstanceTypes(400)
		cp2.InstanceTypes = its2
		start := time.Now()
		res := testing.Benchmark(func(b *testing.B) {
			clk := &clock.RealClock{}
			clusterState := state.NewCluster(clk, preseededClient, cp2)
			for i := 0; i < b.N; i++ {
				topo, err := scheduling.NewTopology(ctx, preseededClient, clusterState, nil,
					[]*v1.NodePool{nodePool2},
					map[string][]*cloudprovider.InstanceType{nodePool2.Name: its2},
					newPods)
				if err != nil {
					b.Fatalf("topology: %s", err)
				}
				sched := scheduling.NewScheduler(ctx, preseededClient, []*v1.NodePool{nodePool2}, clusterState, nil, topo,
					map[string][]*cloudprovider.InstanceType{nodePool2.Name: its2}, nil,
					events.NewRecorder(&record.FakeRecorder{}), clk, nil)
				results, err := sched.Solve(ctx, newPods)
				if err != nil {
					b.Fatalf("solve: %s", err)
				}
				if len(results.PodErrors) > 0 {
					b.Fatalf("unscheduled pods: %d", len(results.PodErrors))
				}
			}
		})
		totalTime += time.Since(start) / time.Duration(res.N)
		nodeCount := res.Extra["nodes"]
		totalPods += podCount
		totalNodes += int(nodeCount)
		fmt.Fprintf(tw, "%d existing nodes\t%d pods\t%d nodes\t%s per scheduling\t%s per pod\n",
			existingCount, podCount, int(nodeCount),
			time.Duration(res.NsPerOp()), time.Duration(res.NsPerOp()/int64(podCount)))
	}

	// High Inflight: exercises the sort.Slice / heap path on growing inflight NodeClaims
	fmt.Fprintf(tw, "============== High Inflight ==============\n")
	for _, podCount := range []int{500, 2000} {
		start := time.Now()
		res := testing.Benchmark(func(b *testing.B) {
			benchmarkScheduler(b, makeHighInflightPods(podCount))
		})
		totalTime += time.Since(start) / time.Duration(res.N)
		nodeCount := res.Extra["nodes"]
		totalPods += podCount
		totalNodes += int(nodeCount)
		fmt.Fprintf(tw, "%d Pods\t%d pods\t%d nodes\t%s per scheduling\t%s per pod\n",
			podCount, podCount, int(nodeCount),
			time.Duration(res.NsPerOp()), time.Duration(res.NsPerOp()/int64(podCount)))
	}

	tw.Flush()
}

func benchmarkScheduler(b *testing.B, pods []*corev1.Pod, opts ...scheduling.Options) {
	ctx = options.ToContext(injection.WithControllerName(context.Background(), "provisioner"), test.Options())
	scheduler, err := setupScheduler(ctx, pods, append(opts, scheduling.NumConcurrentReconciles(5))...)
	if err != nil {
		b.Fatalf("creating scheduler, %s", err)
	}

	b.ResetTimer()
	// Pack benchmark
	start := time.Now()
	podsScheduledInRound1 := 0
	nodesInRound1 := 0
	for i := 0; i < b.N; i++ {
		results, err := scheduler.Solve(ctx, pods)
		if err != nil {
			b.Fatalf("expected scheduler to schedule all pods without error, got %s", err)
		}
		if len(results.PodErrors) > 0 {
			b.Fatalf("expected all pods to schedule, got %d pods that didn't", len(results.PodErrors))
		}
		if i == 0 {
			minPods := math.MaxInt64
			maxPods := 0
			var podCounts []int
			for _, n := range results.NewNodeClaims {
				podCounts = append(podCounts, len(n.Pods))
				podsScheduledInRound1 += len(n.Pods)
				nodesInRound1 = len(results.NewNodeClaims)
				if len(n.Pods) > maxPods {
					maxPods = len(n.Pods)
				}
				if len(n.Pods) < minPods {
					minPods = len(n.Pods)
				}
			}
			if PrintStats {
				meanPodsPerNode := float64(podsScheduledInRound1) / float64(nodesInRound1)
				variance := 0.0
				for _, pc := range podCounts {
					variance += math.Pow(float64(pc)-meanPodsPerNode, 2.0)
				}
				variance /= float64(nodesInRound1)
				stddev := math.Sqrt(variance)
				fmt.Printf("400 instance types %d pods resulted in %d nodes with pods per node min=%d max=%d mean=%f stddev=%f\n",
					len(pods), nodesInRound1, minPods, maxPods, meanPodsPerNode, stddev)
			}
		}
	}
	duration := time.Since(start)
	podsPerSec := float64(len(pods)) / (duration.Seconds() / float64(b.N))
	b.ReportMetric(podsPerSec, "pods/sec")
	b.ReportMetric(float64(podsScheduledInRound1), "pods")
	b.ReportMetric(float64(nodesInRound1), "nodes")
}

func setupScheduler(ctx context.Context, pods []*corev1.Pod, opts ...scheduling.Options) (*scheduling.Scheduler, error) {
	nodePool := test.NodePool(v1.NodePool{
		Spec: v1.NodePoolSpec{
			Limits: v1.Limits{
				corev1.ResourceCPU:    resource.MustParse("10000000"),
				corev1.ResourceMemory: resource.MustParse("10000000Gi"),
			},
		},
	})

	// Apply limits to both of the NodePools
	cloudProvider = fake.NewCloudProvider()
	instanceTypes := fake.InstanceTypes(400)
	cloudProvider.InstanceTypes = instanceTypes

	client := fakecr.NewFakeClient()
	clock := &clock.RealClock{}
	cluster = state.NewCluster(clock, client, cloudProvider)
	topology, err := scheduling.NewTopology(ctx, client, cluster, nil, []*v1.NodePool{nodePool}, map[string][]*cloudprovider.InstanceType{
		nodePool.Name: instanceTypes,
	}, pods, opts...)
	if err != nil {
		return nil, fmt.Errorf("creating topology, %w", err)
	}

	return scheduling.NewScheduler(
		ctx,
		client,
		[]*v1.NodePool{nodePool},
		cluster,
		nil,
		topology,
		map[string][]*cloudprovider.InstanceType{nodePool.Name: instanceTypes},
		nil,
		events.NewRecorder(&record.FakeRecorder{}),
		clock,
		nil, // volumeReqsByPod
		opts...,
	), nil
}

func makeDiversePods(count int) []*corev1.Pod {
	var pods []*corev1.Pod
	numTypes := 5
	pods = append(pods, makeGenericPods(count/numTypes)...)
	pods = append(pods, makeTopologySpreadPods(count/numTypes, corev1.LabelTopologyZone)...)
	pods = append(pods, makeTopologySpreadPods(count/numTypes, corev1.LabelHostname)...)
	pods = append(pods, makePodAffinityPods(count/numTypes, corev1.LabelTopologyZone)...)
	pods = append(pods, makePodAntiAffinityPods(count/numTypes, corev1.LabelHostname)...)

	// fill out due to count being not evenly divisible with generic pods
	nRemaining := count - len(pods)
	pods = append(pods, makeGenericPods(nRemaining)...)
	return pods
}

func makePodAntiAffinityPods(count int, key string) []*corev1.Pod {
	var pods []*corev1.Pod
	// all of these pods have anti-affinity to each other
	labels := map[string]string{
		"app": "nginx",
	}
	for i := 0; i < count; i++ {
		pods = append(pods, test.Pod(
			test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
					UID:    uuid.NewUUID(), // set the UUID so the cached data is properly stored in the scheduler
				},
				PodAntiRequirements: []corev1.PodAffinityTerm{
					{
						LabelSelector: &metav1.LabelSelector{MatchLabels: labels},
						TopologyKey:   key,
					},
				},
				ResourceRequirements: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    randomCPU(),
						corev1.ResourceMemory: randomMemory(),
					},
				}}))
	}
	return pods
}
func makePodAffinityPods(count int, key string) []*corev1.Pod {
	var pods []*corev1.Pod
	for i := 0; i < count; i++ {
		// We use self-affinity here because using affinity that relies on other pod
		// domains doens't guarantee that all pods can schedule. In the case where you are not
		// using self-affinity and the domain doesn't exist, scheduling will fail for all pods with
		// affinities against this domain
		labels := randomAffinityLabels()
		pods = append(pods, test.Pod(
			test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
					UID:    uuid.NewUUID(), // set the UUID so the cached data is properly stored in the scheduler
				},
				PodRequirements: []corev1.PodAffinityTerm{
					{
						LabelSelector: &metav1.LabelSelector{MatchLabels: labels},
						TopologyKey:   key,
					},
				},
				ResourceRequirements: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    randomCPU(),
						corev1.ResourceMemory: randomMemory(),
					},
				}}))
	}
	return pods
}

func makeTopologySpreadPods(count int, key string) []*corev1.Pod {
	var pods []*corev1.Pod
	for i := 0; i < count; i++ {
		pods = append(pods, test.Pod(
			test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{
					Labels: randomLabels(),
					UID:    uuid.NewUUID(), // set the UUID so the cached data is properly stored in the scheduler
				},
				TopologySpreadConstraints: []corev1.TopologySpreadConstraint{
					{
						MaxSkew:           1,
						TopologyKey:       key,
						WhenUnsatisfiable: corev1.DoNotSchedule,
						LabelSelector: &metav1.LabelSelector{
							MatchLabels: randomLabels(),
						},
					},
				},
				ResourceRequirements: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    randomCPU(),
						corev1.ResourceMemory: randomMemory(),
					},
				}}))
	}
	return pods
}

func makeGenericPods(count int) []*corev1.Pod {
	var pods []*corev1.Pod
	for i := 0; i < count; i++ {
		pods = append(pods, test.Pod(
			test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{
					Labels: randomLabels(),
					UID:    uuid.NewUUID(), // set the UUID so the cached data is properly stored in the scheduler
				},
				ResourceRequirements: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    randomCPU(),
						corev1.ResourceMemory: randomMemory(),
					},
				}}))
	}
	return pods
}

func makePreferencePods(count int) []*corev1.Pod {
	pods := test.Pods(count, test.PodOptions{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{
				"app": "nginx",
			},
		},
		NodePreferences: []corev1.NodeSelectorRequirement{
			// This is a preference that can be satisfied
			{
				Key:      corev1.LabelTopologyZone,
				Operator: corev1.NodeSelectorOpIn,
				Values:   []string{"test-zone-1"},
			},
		},
		PodAntiPreferences: []corev1.WeightedPodAffinityTerm{
			// This is a preference that can't be satisfied
			{
				Weight: 10,
				PodAffinityTerm: corev1.PodAffinityTerm{
					LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
						"app": "nginx",
					}},
					TopologyKey: corev1.LabelTopologyZone,
				},
			},
			// This is a preference that can be satisfied
			{
				Weight: 1,
				PodAffinityTerm: corev1.PodAffinityTerm{
					LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
						"app": "nginx",
					}},
					TopologyKey: corev1.LabelHostname,
				},
			},
		},
		ResourceRequirements: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    randomCPU(),
				corev1.ResourceMemory: randomMemory(),
			},
		},
	})
	for _, p := range pods {
		p.UID = uuid.NewUUID() // set the UUID so the cached data is properly stored in the scheduler
	}
	return pods
}

func randomAffinityLabels() map[string]string {
	return map[string]string{
		"my-affininity": randomLabelValue(),
	}
}

func randomLabels() map[string]string {
	return map[string]string{
		"my-label": randomLabelValue(),
	}
}

func randomLabelValue() string {
	labelValues := []string{"a", "b", "c", "d", "e", "f", "g"}
	return labelValues[r.Intn(len(labelValues))]
}

func randomMemory() resource.Quantity {
	mem := []int{100, 256, 512, 1024, 2048, 4096}
	return resource.MustParse(fmt.Sprintf("%dMi", mem[r.Intn(len(mem))]))
}

func randomCPU() resource.Quantity {
	cpu := []int{100, 250, 500, 1000, 1500}
	return resource.MustParse(fmt.Sprintf("%dm", cpu[r.Intn(len(cpu))]))
}

// makeTopologySpreadSingleKeyPods creates count pods that all share a single
// TopologySpreadConstraint (zone key, selector app=benchmark-tsc).
// All pods hash to the same TopologyGroup, so countDomains is called exactly
// once per Solve — but that call issues one kubeClient.Get per pre-seeded node.
func makeTopologySpreadSingleKeyPods(count int) []*corev1.Pod {
	const tscAppLabel = "benchmark-tsc"
	var pods []*corev1.Pod
	for i := 0; i < count; i++ {
		pods = append(pods, test.Pod(test.PodOptions{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "default",
				Labels:    map[string]string{"app": tscAppLabel},
				UID:       uuid.NewUUID(),
			},
			TopologySpreadConstraints: []corev1.TopologySpreadConstraint{
				{
					MaxSkew:           1,
					TopologyKey:       corev1.LabelTopologyZone,
					WhenUnsatisfiable: corev1.DoNotSchedule,
					LabelSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"app": tscAppLabel},
					},
				},
			},
			ResourceRequirements: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    randomCPU(),
					corev1.ResourceMemory: randomMemory(),
				},
			},
		}))
	}
	return pods
}

// makeHighInflightPods creates count pods with hostname anti-affinity to each other.
// Every pod must land on a distinct node, growing s.newNodeClaims to count.
// This is the worst case for the sort.Slice call in scheduler.add() — it runs
// on lists of length 1, 2, 3, ..., count, once per pod scheduled.
func makeHighInflightPods(count int) []*corev1.Pod {
	labels := map[string]string{"app": "high-inflight-bench"}
	var pods []*corev1.Pod
	for i := 0; i < count; i++ {
		pods = append(pods, test.Pod(test.PodOptions{
			ObjectMeta: metav1.ObjectMeta{
				Labels: labels,
				UID:    uuid.NewUUID(),
			},
			PodAntiRequirements: []corev1.PodAffinityTerm{
				{
					LabelSelector: &metav1.LabelSelector{MatchLabels: labels},
					TopologyKey:   corev1.LabelHostname,
				},
			},
			ResourceRequirements: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("100m"),
					corev1.ResourceMemory: resource.MustParse("128Mi"),
				},
			},
		}))
	}
	return pods
}

// benchmarkSortSlice measures the cost of one sort.Slice call on a slice of n ints,
// mirroring the sort.Slice(s.newNodeClaims, ...) call in scheduler.add() at inflight
// NodeClaim count n. Called once per pod scheduled, so total cost per batch = n*b.N*ns/op.
func benchmarkSortSlice(b *testing.B, n int) {
	b.Helper()
	podCounts := make([]int, n)
	for i := range podCounts {
		podCounts[i] = r.Intn(50)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sort.Slice(podCounts, func(a, b int) bool { return podCounts[a] < podCounts[b] })
	}
}

// BenchmarkSortSliceNewNodeClaims10/50/100/500 measure the sort.Slice cost at increasing
// inflight NodeClaim counts. Compare against a container/heap replacement.
func BenchmarkSortSliceNewNodeClaims10(b *testing.B)  { benchmarkSortSlice(b, 10) }
func BenchmarkSortSliceNewNodeClaims50(b *testing.B)  { benchmarkSortSlice(b, 50) }
func BenchmarkSortSliceNewNodeClaims100(b *testing.B) { benchmarkSortSlice(b, 100) }
func BenchmarkSortSliceNewNodeClaims500(b *testing.B) { benchmarkSortSlice(b, 500) }

// uniqueTSCPodCount is the number of distinct topology groups created per
// BenchmarkTopologyGroupHash iteration. Divide ns/op by this to get per-Hash() cost.
const uniqueTSCPodCount = 100

// BenchmarkTopologyGroupHash measures the per-call cost of TopologyGroup.Hash(),
// which uses a direct fnv.New64a() encoder (was reflection-based hashstructure).
// It creates uniqueTSCPodCount pods each with a distinct TSC label selector so
// uniqueTSCPodCount distinct Hash() calls are made inside NewTopology.
// countDomains returns immediately (empty fake client).
// Divide ns/op by uniqueTSCPodCount to get per-Hash() cost.
func BenchmarkTopologyGroupHash(b *testing.B) {
	ctx = options.ToContext(injection.WithControllerName(context.Background(), "provisioner"), test.Options())
	pods := makeUniqueTSCPods(uniqueTSCPodCount)

	nodePool := test.NodePool(v1.NodePool{
		Spec: v1.NodePoolSpec{
			Limits: v1.Limits{
				corev1.ResourceCPU:    resource.MustParse("10000000"),
				corev1.ResourceMemory: resource.MustParse("10000000Gi"),
			},
		},
	})
	cp := fake.NewCloudProvider()
	instanceTypes := fake.InstanceTypes(400)
	cp.InstanceTypes = instanceTypes

	// Build client and cluster once outside the timer — only NewTopology is measured.
	client := fakecr.NewFakeClient()
	clk := &clock.RealClock{}
	clusterState := state.NewCluster(clk, client, cp)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := scheduling.NewTopology(ctx, client, clusterState, nil,
			[]*v1.NodePool{nodePool},
			map[string][]*cloudprovider.InstanceType{nodePool.Name: instanceTypes},
			pods)
		if err != nil {
			b.Fatalf("creating topology: %s", err)
		}
	}
	b.ReportMetric(float64(uniqueTSCPodCount), "topology-groups/op")
}

// makeUniqueTSCPods creates count pods each with a distinct TSC label selector,
// forcing count distinct TopologyGroup hashes inside NewTopology.
func makeUniqueTSCPods(count int) []*corev1.Pod {
	var pods []*corev1.Pod
	for i := 0; i < count; i++ {
		appLabel := fmt.Sprintf("tsc-app-%d", i)
		pods = append(pods, test.Pod(test.PodOptions{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "default",
				Labels:    map[string]string{"app": appLabel},
				UID:       uuid.NewUUID(),
			},
			TopologySpreadConstraints: []corev1.TopologySpreadConstraint{
				{
					MaxSkew:           1,
					TopologyKey:       corev1.LabelTopologyZone,
					WhenUnsatisfiable: corev1.DoNotSchedule,
					LabelSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"app": appLabel},
					},
				},
			},
			ResourceRequirements: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("100m"),
					corev1.ResourceMemory: resource.MustParse("128Mi"),
				},
			},
		}))
	}
	return pods
}

// BenchmarkSchedulingHighInflight500 and ...2000 measure scheduling throughput when
// every pod must land on a distinct node (hostname anti-affinity), maximizing
// s.newNodeClaims size and the sort.Slice cost in scheduler.add().
// Compare pods/sec against BenchmarkScheduling500 / BenchmarkScheduling2000 to quantify
// the throughput regression from inflight NodeClaim accumulation.
func BenchmarkSchedulingHighInflight500(b *testing.B) {
	benchmarkScheduler(b, makeHighInflightPods(500))
}

func BenchmarkSchedulingHighInflight2000(b *testing.B) {
	benchmarkScheduler(b, makeHighInflightPods(2000))
}

// buildPreseededClient creates a fake client seeded with existingNodeCount (Node, Pod) pairs
// spread across 3 zones. Pods have label app=benchmark-tsc and Spec.NodeName set so
// countDomains exercises the kubeClient.Get path. Build once outside the benchmark timer.
func buildPreseededClient(existingNodeCount int) client.Client {
	zones := []string{"test-zone-1", "test-zone-2", "test-zone-3"}
	objs := make([]runtime.Object, 0, existingNodeCount*2)
	for i := 0; i < existingNodeCount; i++ {
		nodeName := fmt.Sprintf("bench-node-%d", i)
		zone := zones[i%len(zones)]
		node := test.Node(test.NodeOptions{
			ObjectMeta: metav1.ObjectMeta{
				Name: nodeName,
				Labels: map[string]string{
					corev1.LabelTopologyZone: zone,
					corev1.LabelHostname:     nodeName,
				},
			},
		})
		pod := test.Pod(test.PodOptions{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "default",
				Labels:    map[string]string{"app": "benchmark-tsc"},
				UID:       uuid.NewUUID(),
			},
			ResourceRequirements: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("100m"),
					corev1.ResourceMemory: resource.MustParse("128Mi"),
				},
			},
		})
		pod.Spec.NodeName = nodeName
		objs = append(objs, node, pod)
	}
	return fakecr.NewFakeClient(objs...)
}

// benchmarkSchedulerWithPreseededState benchmarks scheduling newPods against a cluster
// with existingNodeCount pre-seeded (Node, Pod) pairs. A fresh topology+scheduler is
// created per iteration so countDomains runs every time. The fake client is built once
// outside the timer.
func benchmarkSchedulerWithPreseededState(b *testing.B, existingNodeCount int, newPods []*corev1.Pod) {
	b.Helper()
	ctx = options.ToContext(injection.WithControllerName(context.Background(), "provisioner"), test.Options())

	nodePool := test.NodePool(v1.NodePool{
		Spec: v1.NodePoolSpec{
			Limits: v1.Limits{
				corev1.ResourceCPU:    resource.MustParse("10000000"),
				corev1.ResourceMemory: resource.MustParse("10000000Gi"),
			},
		},
	})
	cp := fake.NewCloudProvider()
	instanceTypes := fake.InstanceTypes(400)
	cp.InstanceTypes = instanceTypes

	preseededClient := buildPreseededClient(existingNodeCount)

	clk := &clock.RealClock{}
	b.ResetTimer()
	nodesInRound1 := 0
	start := time.Now()
	for i := 0; i < b.N; i++ {
		clusterState := state.NewCluster(clk, preseededClient, cp)
		topology, err := scheduling.NewTopology(ctx, preseededClient, clusterState, nil,
			[]*v1.NodePool{nodePool},
			map[string][]*cloudprovider.InstanceType{nodePool.Name: instanceTypes},
			newPods)
		if err != nil {
			b.Fatalf("creating topology: %s", err)
		}
		sched := scheduling.NewScheduler(
			ctx,
			preseededClient,
			[]*v1.NodePool{nodePool},
			clusterState,
			nil,
			topology,
			map[string][]*cloudprovider.InstanceType{nodePool.Name: instanceTypes},
			nil,
			events.NewRecorder(&record.FakeRecorder{}),
			clk,
			nil,
		)
		results, err := sched.Solve(ctx, newPods)
		if err != nil {
			b.Fatalf("scheduler.Solve: %s", err)
		}
		if len(results.PodErrors) > 0 {
			b.Fatalf("expected all pods to schedule, got %d errors", len(results.PodErrors))
		}
		if i == 0 {
			nodesInRound1 = len(results.NewNodeClaims)
		}
	}
	duration := time.Since(start)
	podsPerSec := float64(len(newPods)) / (duration.Seconds() / float64(b.N))
	b.ReportMetric(podsPerSec, "pods/sec")
	b.ReportMetric(float64(len(newPods)), "pods")
	b.ReportMetric(float64(nodesInRound1), "nodes")
	b.ReportMetric(float64(existingNodeCount), "existing-nodes")
}

// BenchmarkSchedulingTopologySpreadWith500ExistingNodes and ...2000ExistingNodes prove
// the O(N) countDomains kubeClient.Get cost. The growing ns/op from 500→2000 nodes
// confirms the finding. After the fix (stateNodes map lookup), performance should be flat.
func BenchmarkSchedulingTopologySpreadWith500ExistingNodes(b *testing.B) {
	benchmarkSchedulerWithPreseededState(b, 500, makeTopologySpreadSingleKeyPods(500))
}

func BenchmarkSchedulingTopologySpreadWith2000ExistingNodes(b *testing.B) {
	benchmarkSchedulerWithPreseededState(b, 2000, makeTopologySpreadSingleKeyPods(500))
}
