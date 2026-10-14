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
	"io"
	"testing"
	"time"

	"github.com/go-logr/zapr"
	"github.com/samber/lo"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clock "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakecr "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
	"sigs.k8s.io/karpenter/pkg/controllers/node/health"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/test"
)

const benchmarkAcceleratorCondition corev1.NodeConditionType = "AcceleratedHardwareReady"

var benchmarkRepairResult health.RepairResult

// benchmarkRepairPolicies resembles a production provider policy set: kubelet readiness, reason-specific accelerator
// policies, a few node-agent conditions and the default fallback.
func benchmarkRepairPolicies() []cloudprovider.RepairPolicy {
	policies := []cloudprovider.RepairPolicy{
		{ConditionType: corev1.NodeReady, ConditionStatus: corev1.ConditionFalse, TolerationDuration: 30 * time.Minute, Action: cloudprovider.ReplaceNode},
		{ConditionType: corev1.NodeReady, ConditionStatus: corev1.ConditionUnknown, ReasonRegex: ".*", TolerationDuration: 30 * time.Minute, Action: cloudprovider.ReplaceNode},
		{ConditionType: benchmarkAcceleratorCondition, ConditionStatus: corev1.ConditionFalse, ReasonRegex: "^NvidiaXID(13|31|43|45|48|63|64|74|79|94|95)Error$", TolerationDuration: 10 * time.Minute, Priority: 50, Action: cloudprovider.ReplaceNode},
		{ConditionType: benchmarkAcceleratorCondition, ConditionStatus: corev1.ConditionFalse, ReasonRegex: "^NeuronSRAMUncorrectable", TolerationDuration: 10 * time.Minute, Priority: 50, Action: cloudprovider.RebootNode},
		{ConditionType: benchmarkAcceleratorCondition, ConditionStatus: corev1.ConditionFalse, ReasonRegex: "^Neuron", TolerationDuration: 10 * time.Minute, Priority: 50, Action: cloudprovider.ReplaceNode},
	}
	for _, conditionType := range []corev1.NodeConditionType{"StorageReady", "NetworkingReady", "KernelReady", "ContainerRuntimeReady"} {
		policies = append(policies, cloudprovider.RepairPolicy{ConditionType: conditionType, ConditionStatus: corev1.ConditionFalse, ReasonRegex: ".*", TolerationDuration: 30 * time.Minute, Action: cloudprovider.ReplaceNode})
	}
	return policies
}

// benchmarkNode returns a managed Node with the kubelet and node-agent conditions. Unhealthy Nodes report an
// accelerator failure with the given reason, matched by a reason-specific policy and past its toleration.
func benchmarkNode(i int, unhealthy bool, acceleratorReason string, now, heartbeat time.Time) *corev1.Node {
	created := metav1.NewTime(now.Add(-24 * time.Hour))
	condition := func(conditionType corev1.NodeConditionType, status corev1.ConditionStatus, reason string) corev1.NodeCondition {
		return corev1.NodeCondition{Type: conditionType, Status: status, Reason: reason, LastHeartbeatTime: metav1.NewTime(heartbeat), LastTransitionTime: created}
	}
	accelerator := condition(benchmarkAcceleratorCondition, lo.Ternary(unhealthy, corev1.ConditionFalse, corev1.ConditionTrue), acceleratorReason)
	accelerator.LastTransitionTime = metav1.NewTime(now.Add(-time.Hour))
	name := fmt.Sprintf("node-%d", i)
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, CreationTimestamp: created, Labels: map[string]string{
			v1.NodePoolLabelKey:            "default",
			v1.NodeInitializedLabelKey:     "true",
			v1.NodeRegisteredLabelKey:      "true",
			corev1.LabelInstanceTypeStable: "default-instance-type",
		}},
		Spec: corev1.NodeSpec{ProviderID: "fake:///" + name},
		Status: corev1.NodeStatus{
			Capacity: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("16Gi"), corev1.ResourcePods: resource.MustParse("110")},
			Conditions: []corev1.NodeCondition{
				condition(corev1.NodeReady, corev1.ConditionTrue, "KubeletReady"),
				condition(corev1.NodeMemoryPressure, corev1.ConditionFalse, "KubeletHasSufficientMemory"),
				condition(corev1.NodeDiskPressure, corev1.ConditionFalse, "KubeletHasNoDiskPressure"),
				condition(corev1.NodePIDPressure, corev1.ConditionFalse, "KubeletHasSufficientPID"),
				accelerator,
				condition("StorageReady", corev1.ConditionTrue, "DiskIsHealthy"),
				condition("NetworkingReady", corev1.ConditionTrue, "NetworkingIsHealthy"),
				condition("KernelReady", corev1.ConditionTrue, "KernelIsHealthy"),
				condition("ContainerRuntimeReady", corev1.ConditionTrue, "ContainerRuntimeIsHealthy"),
			},
		},
	}
}

// benchmarkAcceleratorReason returns the accelerator condition's reason. The two variants differ, so alternating them
// changes the Node's repair policy match inputs.
func benchmarkAcceleratorReason(unhealthy bool, variant int) string {
	if unhealthy {
		return []string{"NvidiaXID79Error", "NvidiaXID48Error"}[variant]
	}
	return []string{"AcceleratedHardwareIsHealthy", "DriverReloaded"}[variant]
}

// benchmarkCluster returns cluster state holding n Nodes matched by matcher, the first unhealthy of which are unhealthy.
func benchmarkCluster(ctx context.Context, b *testing.B, provider *fake.CloudProvider, matcher *health.RepairPolicyMatcher, n, unhealthy int, now time.Time) *state.Cluster {
	b.Helper()
	kubeClient := fakecr.NewClientBuilder().WithIndex(&corev1.Pod{}, "spec.nodeName", func(o client.Object) []string {
		return []string{o.(*corev1.Pod).Spec.NodeName}
	}).Build()
	clusterState := state.NewCluster(clock.NewFakeClock(now), kubeClient, provider, state.WithRepairPolicyMatcher(matcher))
	for i := range n {
		if err := clusterState.UpdateNode(ctx, benchmarkNode(i, i < unhealthy, benchmarkAcceleratorReason(i < unhealthy, 0), now, now)); err != nil {
			b.Fatal(err)
		}
	}
	return clusterState
}

// benchmarkContext enables node repair and carries an Info-level logger, as the controller runs with, so debug logging
// costs what it does there.
func benchmarkContext() context.Context {
	core := zapcore.NewCore(zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig()), zapcore.AddSync(io.Discard), zapcore.InfoLevel)
	ctx := log.IntoContext(context.Background(), zapr.NewLogger(zap.New(core)))
	return options.ToContext(ctx, test.Options(test.OptionsFields{FeatureGates: test.FeatureGates{NodeRepair: lo.ToPtr(true)}}))
}

func benchmarkMatcher(ctx context.Context, b *testing.B) (*fake.CloudProvider, *health.RepairPolicyMatcher) {
	b.Helper()
	provider := fake.NewCloudProvider()
	provider.RepairPolicy = benchmarkRepairPolicies()
	matcher, err := health.NewRepairPolicyMatcher(ctx, provider)
	if err != nil {
		b.Fatal(err)
	}
	return provider, matcher
}

// BenchmarkClusterUpdateNode measures one Node update in cluster state. Heartbeat updates only advance
// LastHeartbeatTime, so the Node's cached repair policy matches carry over; ConditionChange updates alternate the
// accelerator condition's reason, so the Node is matched again.
func BenchmarkClusterUpdateNode(b *testing.B) {
	ctx := benchmarkContext()
	provider, matcher := benchmarkMatcher(ctx, b)
	for _, n := range []int{1000, 5000} {
		for _, unhealthyPercent := range []int{0, 5, 100} {
			for _, update := range []string{"Heartbeat", "ConditionChange"} {
				b.Run(fmt.Sprintf("Nodes=%d/Unhealthy=%d%%/Update=%s", n, unhealthyPercent, update), func(b *testing.B) {
					now := time.Now()
					unhealthy := n * unhealthyPercent / 100
					clusterState := benchmarkCluster(ctx, b, provider, matcher, n, unhealthy, now)
					// Two prebuilt versions per Node, alternated so every update differs from the one cluster state holds.
					versions := [2][]*corev1.Node{}
					for v := range versions {
						heartbeat := now.Add(time.Duration(v+1) * time.Minute)
						variant := lo.Ternary(update == "ConditionChange", 1-v, 0)
						for i := range n {
							versions[v] = append(versions[v], benchmarkNode(i, i < unhealthy, benchmarkAcceleratorReason(i < unhealthy, variant), now, heartbeat))
						}
					}
					b.ReportAllocs()
					b.ResetTimer()
					for i := 0; i < b.N; i++ {
						if err := clusterState.UpdateNode(ctx, versions[(i/n)%2][i%n]); err != nil {
							b.Fatal(err)
						}
					}
				})
			}
		}
	}
}

// BenchmarkRepairDecision measures the repair decision for every Node in cluster state, as one disruption pass makes
// it: each Node resolves the repair policy matches cluster state keeps for it, which healthy Nodes have none of.
func BenchmarkRepairDecision(b *testing.B) {
	ctx := benchmarkContext()
	provider, matcher := benchmarkMatcher(ctx, b)
	for _, n := range []int{1000, 5000} {
		for _, unhealthyPercent := range []int{0, 5, 100} {
			b.Run(fmt.Sprintf("Nodes=%d/Unhealthy=%d%%", n, unhealthyPercent), func(b *testing.B) {
				now := time.Now()
				unhealthy := n * unhealthyPercent / 100
				clusterState := benchmarkCluster(ctx, b, provider, matcher, n, unhealthy, now)
				var nodes []*state.StateNode
				for node := range clusterState.Nodes() {
					nodes = append(nodes, node)
				}
				if repairable := lo.CountBy(nodes, func(node *state.StateNode) bool {
					return node.GetRepairResult(now).Action != ""
				}); repairable != unhealthy {
					b.Fatalf("expected %d repairable nodes, got %d", unhealthy, repairable)
				}
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					for _, node := range nodes {
						benchmarkRepairResult = node.GetRepairResult(now)
					}
				}
			})
		}
	}
}
