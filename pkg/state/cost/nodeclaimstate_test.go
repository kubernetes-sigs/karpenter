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

package cost_test

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
	"sigs.k8s.io/karpenter/pkg/scheduling"
	"sigs.k8s.io/karpenter/pkg/state/cost"
)

//nolint:gocyclo // Assert each stage of the tracked-node cost lifecycle and ordering guards.
func TestMutableNodeCostLifecycle(t *testing.T) {
	ctx := context.Background()

	np := &v1.NodePool{ObjectMeta: metav1.ObjectMeta{Name: "pool"}}
	kube := clientfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(np).Build()
	cp := fake.NewCloudProvider()
	cp.InstanceTypes = []*cloudprovider.InstanceType{{Name: "m5.large", Offerings: cloudprovider.Offerings{{Price: 1, Available: true, Requirements: scheduling.NewRequirements(scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "zone"), scheduling.NewRequirement(v1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, v1.CapacityTypeOnDemand))}}}}
	tracker := cost.NewClusterCost(ctx, cp, kube)
	nc := &v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Name: "node", Labels: map[string]string{v1.NodePoolLabelKey: "pool", corev1.LabelInstanceTypeStable: "m5.large", corev1.LabelTopologyZone: "zone", v1.CapacityTypeLabelKey: v1.CapacityTypeReserved}, Annotations: map[string]string{v1.CapacityTypeTransitionsAnnotationKey: "on-demand,reserved"}}}
	if err := tracker.UpdateNodeClaim(ctx, nc); err != nil {
		t.Fatal(err)
	}
	if got := tracker.GetClusterCost(); got != 1 {
		t.Fatalf("initial conservative cost = %v, want 1", got)
	}
	for _, price := range []float64{0.0000001, 1, .25, 0} {
		tracker.UpdateNodeClaimState(nc, cloudprovider.NodeClaimState{ObservedAt: time.Now(), Price: price})
		if err := tracker.UpdateNodeClaim(ctx, nc); err != nil {
			t.Fatal(err)
		}
		tracker.UpdateOfferings(ctx, np, cp.InstanceTypes)
		if got := tracker.GetClusterCost(); got != price {
			t.Fatalf("cluster cost = %v, want %v", got, price)
		}
		if got := tracker.GetNodepoolCost(np); got != price {
			t.Fatalf("pool cost = %v, want %v", got, price)
		}
	}
	// A slower, older API request cannot overwrite the newest cost.
	tracker.UpdateNodeClaimState(nc, cloudprovider.NodeClaimState{ObservedAt: time.Now().Add(-time.Hour), Price: 99})
	if tracker.GetClusterCost() != 0 {
		t.Fatal("older observation overwrote newer cost")
	}
	if err := tracker.DeleteNodeClaim(ctx, client.ObjectKeyFromObject(nc)); err != nil {
		t.Fatal(err)
	}
	if tracker.GetClusterCost() != 0 {
		t.Fatal("deleted node retained cost")
	}
	tracker.UpdateNodeClaimState(nc, cloudprovider.NodeClaimState{Price: 1})
	if tracker.GetClusterCost() != 0 {
		t.Fatal("late observation resurrected deleted node")
	}
	if err := tracker.UpdateNodeClaim(ctx, nc); err != nil {
		t.Fatal(err)
	}
	tracker.UpdateNodeClaimState(nc, cloudprovider.NodeClaimState{ObservedAt: time.Now(), Price: 1})
	tracker.DeleteNodePool(ctx, np.Name)
	if tracker.GetClusterCost() != 0 {
		t.Fatal("deleted pool retained observed costs")
	}
}
