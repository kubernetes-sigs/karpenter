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
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/clock"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	cloudproviderfake "sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
	"sigs.k8s.io/karpenter/pkg/controllers/provisioning/scheduling"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/events"
	"sigs.k8s.io/karpenter/pkg/operator/injection"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/test"
)

func TestPreparedSchedulerInputsProduceEquivalentIndependentSolves(t *testing.T) {
	ctx := options.ToContext(injection.WithControllerName(context.Background(), "prepared-scheduler-test"), test.Options())
	kubeClient := fakeclient.NewFakeClient()
	provider := cloudproviderfake.NewCloudProvider()
	instanceTypes := cloudproviderfake.InstanceTypes(1)
	provider.InstanceTypes = instanceTypes
	nodePool := test.NodePool(v1.NodePool{})
	pod := test.Pod(test.PodOptions{})
	pod.UID = types.UID("prepared-pod")
	pods := []*corev1.Pod{pod}
	cl := state.NewCluster(&clock.RealClock{}, kubeClient, provider)
	recorder := events.NewRecorder(&record.FakeRecorder{})

	prepared, err := scheduling.NewPreparedSchedulerInputs(
		ctx,
		[]*v1.NodePool{nodePool},
		nil,
		map[string][]*cloudprovider.InstanceType{nodePool.Name: instanceTypes},
		nil,
		recorder,
		&clock.RealClock{},
	)
	if err != nil {
		t.Fatalf("preparing scheduler inputs, %v", err)
	}
	if stats := prepared.Stats(); stats.NodePools != 1 || stats.Templates != 1 || stats.InstanceTypes != 1 {
		t.Fatalf("unexpected prepared stats %#v", stats)
	}

	solve := func(preparedOption ...scheduling.Options) scheduling.Results {
		topology, err := scheduling.NewTopology(
			ctx,
			kubeClient,
			cl,
			nil,
			[]*v1.NodePool{nodePool},
			map[string][]*cloudprovider.InstanceType{nodePool.Name: instanceTypes},
			pods,
			preparedOption...,
		)
		if err != nil {
			t.Fatalf("creating topology, %v", err)
		}
		scheduler := scheduling.NewScheduler(
			ctx,
			kubeClient,
			[]*v1.NodePool{nodePool},
			cl,
			nil,
			topology,
			map[string][]*cloudprovider.InstanceType{nodePool.Name: instanceTypes},
			nil,
			recorder,
			&clock.RealClock{},
			nil,
			nil,
			preparedOption...,
		)
		results, err := scheduler.Solve(ctx, pods)
		if err != nil {
			t.Fatalf("solving, %v", err)
		}
		return results
	}

	legacy := solve()
	first := solve(scheduling.WithPreparedSchedulerInputs(prepared))
	second := solve(scheduling.WithPreparedSchedulerInputs(prepared))
	for name, results := range map[string]scheduling.Results{"first": first, "second": second} {
		if len(results.NewNodeClaims) != len(legacy.NewNodeClaims) ||
			len(results.ExistingNodes) != len(legacy.ExistingNodes) ||
			len(results.PodErrors) != len(legacy.PodErrors) {
			t.Fatalf("%s prepared result differs from legacy, legacy=%#v prepared=%#v", name, legacy, results)
		}
	}
}

func TestPreparedSchedulerInputsHonorCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	nodePool := test.NodePool(v1.NodePool{})
	_, err := scheduling.NewPreparedSchedulerInputs(
		ctx,
		[]*v1.NodePool{nodePool},
		nil,
		map[string][]*cloudprovider.InstanceType{nodePool.Name: cloudproviderfake.InstanceTypes(1)},
		nil,
		events.NewRecorder(&record.FakeRecorder{}),
		&clock.RealClock{},
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context cancellation, got %v", err)
	}
}
