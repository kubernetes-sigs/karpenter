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

// Shared toolkit for the scheduling microbenchmarks. Benches are organized by the
// function under test; each sweeps one scaling vector and routes through buildScenario.
//
// Sub-benchmark naming contract: vector=<name>/<param>=<n> (e.g. vector=nodes/n=500).
// The benchstat gate parses this to group points by vector and gate on the per-vector
// growth slope — not a single-point cost — which is what catches regressions that are
// cheap at small scale but blow up along one axis. Keep the shape exact.

import (
	"context"
	"fmt"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakecr "sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
	"sigs.k8s.io/karpenter/pkg/controllers/provisioning/scheduling"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/events"
	"sigs.k8s.io/karpenter/pkg/operator/injection"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/test"
)

// Scenario is one knob per scaling vector. A benchmark sweeps one field, holds the
// rest fixed. It's a comparable value, so it doubles as the buildScenario cache key.
type Scenario struct {
	Nodes       int
	PodsPerNode int
	NodePools   int
	DaemonSets  int
}

// ScenarioFixture holds everything scheduling.NewScheduler needs.
type ScenarioFixture struct {
	ctx           context.Context
	client        client.Client
	cluster       *state.Cluster
	nodePools     []*v1.NodePool
	itsByNP       map[string][]*cloudprovider.InstanceType
	topology      *scheduling.Topology
	stateNodes    []*state.StateNode
	daemonSetPods []*corev1.Pod
	recorder      events.Recorder
	clk           clock.Clock
}

func benchCtx() context.Context {
	return options.ToContext(injection.WithControllerName(context.Background(), "provisioner"), test.Options())
}

func benchNodePools(count int) []*v1.NodePool {
	nps := make([]*v1.NodePool, count)
	for i := range nps {
		np := test.NodePool(v1.NodePool{
			Spec: v1.NodePoolSpec{
				Limits: v1.Limits{
					corev1.ResourceCPU:    resource.MustParse("10000000"),
					corev1.ResourceMemory: resource.MustParse("10000000Gi"),
				},
			},
		})
		np.Spec.Template.Spec.Taints = []corev1.Taint{{
			Key:    fmt.Sprintf("bench.example.com/pool-%d", i),
			Value:  "true",
			Effect: corev1.TaintEffectNoSchedule,
		}}
		nps[i] = np
	}
	return nps
}

// benchStateNodes builds initialized+registered StateNodes directly, distributed
// round-robin across the given NodePools.
func benchStateNodes(count int, nodePools []*v1.NodePool) []*state.StateNode {
	rl := corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("16"),
		corev1.ResourceMemory: resource.MustParse("32Gi"),
		corev1.ResourcePods:   resource.MustParse("110"),
	}
	sn := make([]*state.StateNode, 0, count)
	for i := 0; i < count; i++ {
		nc := test.NodeClaim(v1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{v1.NodePoolLabelKey: nodePools[i%len(nodePools)].Name},
			},
			Status: v1.NodeClaimStatus{
				ProviderID:  test.RandomProviderID(),
				Capacity:    rl,
				Allocatable: rl,
			},
		})
		node := test.NodeClaimLinkedNode(nc)
		node.Labels[corev1.LabelHostname] = fmt.Sprintf("bench-host-%d", i)
		node.Labels[corev1.LabelTopologyZone] = "test-zone-1"
		node.Labels[corev1.LabelInstanceTypeStable] = "fake-it-0"
		node.Labels[v1.NodeRegisteredLabelKey] = "true"
		node.Labels[v1.NodeInitializedLabelKey] = "true"
		sn = append(sn, &state.StateNode{Node: node, NodeClaim: nc})
	}
	return sn
}

// benchDaemonSetPods buils distinct daemon-overhead groups, a scaling vector
// introduced with https://github.com/kubernetes-sigs/karpenter/pull/2975
func benchDaemonSetPods(count int) []*corev1.Pod {
	pods := make([]*corev1.Pod, count)
	for i := 0; i < count; i++ {
		// keeps every pod compatible with the existing fake-it-0 nodes
		instanceTypes := []string{"fake-it-0"}
		if i > 0 {
			instanceTypes = append(instanceTypes, fmt.Sprintf("fake-it-%d", i))
		}
		pods[i] = test.Pod(test.PodOptions{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{"app": fmt.Sprintf("ds-%d", i)},
				UID:    uuid.NewUUID(),
			},
			NodeRequirements: []corev1.NodeSelectorRequirement{
				{Key: corev1.LabelTopologyZone, Operator: corev1.NodeSelectorOpIn, Values: []string{"test-zone-1"}},
				{Key: corev1.LabelInstanceTypeStable, Operator: corev1.NodeSelectorOpIn, Values: instanceTypes},
			},
			Tolerations: []corev1.Toleration{{Operator: corev1.TolerationOpExists}},
			ResourceRequirements: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("100m"),
					corev1.ResourceMemory: resource.MustParse("128Mi"),
				},
			},
		})
	}
	return pods
}

// Memoized by full Scenario key so -count=N and the framework's b.Run re-invocation
// don't rebuild the fixture repeatedly
var scenarioCache = map[Scenario]*ScenarioFixture{}

func buildScenario(tb testing.TB, s Scenario) *ScenarioFixture {
	tb.Helper()
	if f, ok := scenarioCache[s]; ok {
		return f
	}
	ctx := benchCtx()

	cp := fake.NewCloudProvider()
	instanceTypes := fake.InstanceTypes(400)
	cp.InstanceTypes = instanceTypes
	c := fakecr.NewFakeClient()
	clk := &clock.RealClock{}
	cl := state.NewCluster(clk, c, cp)

	nodePools := benchNodePools(s.NodePools)
	itsByNP := map[string][]*cloudprovider.InstanceType{}
	for _, np := range nodePools {
		itsByNP[np.Name] = instanceTypes
	}
	topology, err := scheduling.NewTopology(ctx, c, cl, nil, nodePools, itsByNP, makeDiversePods(s.Nodes*s.PodsPerNode))
	if err != nil {
		tb.Fatalf("creating topology: %s", err)
	}
	stateNodes := benchStateNodes(s.Nodes, nodePools)
	daemonSetPods := benchDaemonSetPods(s.DaemonSets)

	f := &ScenarioFixture{
		ctx: ctx, client: c, cluster: cl, nodePools: nodePools, itsByNP: itsByNP,
		topology: topology, stateNodes: stateNodes, daemonSetPods: daemonSetPods,
		recorder: events.NewRecorder(&record.FakeRecorder{}), clk: clk,
	}
	scenarioCache[s] = f
	return f
}

// newSchedulerFromScenario is the single timed call shared by every NewScheduler sweep.
func newSchedulerFromScenario(f *ScenarioFixture) *scheduling.Scheduler {
	return scheduling.NewScheduler(
		f.ctx,
		f.client,
		f.nodePools,
		f.cluster,
		f.stateNodes,
		f.topology,
		f.itsByNP,
		f.daemonSetPods,
		f.recorder,
		f.clk,
		nil, // volumeReqsByPod
		nil, // allocator
		scheduling.NumConcurrentReconciles(5),
	)
}
