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
	"errors"
	"fmt"
	"maps"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/utils/clock"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	pscheduling "sigs.k8s.io/karpenter/pkg/controllers/provisioning/scheduling"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/operator/options"
)

func priceSnapshot(price float64) *nodeClaimPriceSnapshot {
	nc := &v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Name: "a", UID: "a", Labels: map[string]string{v1.NodePoolLabelKey: "pool-a"}}}
	return &nodeClaimPriceSnapshot{
		claims: map[string]*v1.NodeClaim{"a": nc},
		scopes: map[string]string{"a": "group-a"},
		states: map[string]cloudprovider.NodeClaimState{"a": {ObservedAt: time.Now(), Price: price, Scope: "group-a", Labels: map[string]string{v1.CapacityTypeLabelKey: v1.CapacityTypeOnDemand}}},
	}
}

//nolint:gocyclo // Each case represents a distinct change that must invalidate a decision.
func TestMutablePriceSnapshotValidation(t *testing.T) {
	for _, change := range []string{"unchanged", "promotion", "demotion", "reassociation", "price", "missing", "new consumer", "cross-pool reassignment", "unrelated failure"} {
		t.Run(change, func(t *testing.T) {
			baseline, fresh := priceSnapshot(1), priceSnapshot(1)
			switch change {
			case "promotion":
				fresh.states["a"] = cloudprovider.NodeClaimState{Price: 0, Scope: "group-a", Labels: map[string]string{v1.CapacityTypeLabelKey: v1.CapacityTypeReserved}}
			case "demotion":
				baseline.states["a"] = cloudprovider.NodeClaimState{Price: 0, Scope: "group-a", Labels: map[string]string{v1.CapacityTypeLabelKey: v1.CapacityTypeReserved}}
			case "reassociation":
				before, after := baseline.states["a"], fresh.states["a"]
				before.Labels = maps.Clone(before.Labels)
				after.Labels = maps.Clone(after.Labels)
				before.Labels["reservation"] = "cr-a"
				after.Labels["reservation"] = "cr-b"
				baseline.states["a"], fresh.states["a"] = before, after
			case "price":
				fresh.states["a"] = cloudprovider.NodeClaimState{Price: 2, Scope: "group-a"}
			case "missing":
				delete(fresh.states, "a")
			case "new consumer", "cross-pool reassignment", "unrelated failure":
				nc := &v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{UID: "peer", Labels: map[string]string{v1.NodePoolLabelKey: "pool-b"}}}
				fresh.claims["peer"] = nc
				fresh.scopes["peer"] = "group-a"
				fresh.states["peer"] = cloudprovider.NodeClaimState{Price: 1, Scope: "group-a"}
				if change == "cross-pool reassignment" {
					baseline.claims["peer"] = nc
					baseline.scopes["peer"] = "group-a"
					baseline.states["peer"] = cloudprovider.NodeClaimState{Price: 0, Scope: "group-a"}
				}
				if change == "unrelated failure" {
					fresh.scopes["peer"] = "group-b"
					delete(fresh.states, "peer")
				}
			}
			err := baseline.validate(fresh, sets.New("pool-a"))
			wantError := change != "unchanged" && change != "unrelated failure"
			if (err != nil) != wantError {
				t.Fatalf("validation error = %v, want error %t", err, wantError)
			}
		})
	}
}

type observingProvider struct {
	cloudprovider.CloudProvider
	observations map[string]cloudprovider.NodeClaimState
	err          error
}

func (p observingProvider) RefreshNodeClaimStates(context.Context, []*v1.NodeClaim) (map[string]cloudprovider.NodeClaimState, error) {
	return p.observations, p.err
}

func TestMutablePriceRefresh(t *testing.T) {
	ctx := options.ToContext(context.Background(), &options.Options{})
	kubeClient := clientfake.NewClientBuilder().Build()
	cp := observingProvider{CloudProvider: fake.NewCloudProvider(), observations: map[string]cloudprovider.NodeClaimState{}, err: fmt.Errorf("partial read failure")}
	cluster := state.NewCluster(clock.RealClock{}, kubeClient, cp)
	for _, name := range []string{"fresh", "stale", "failed", "unrelated"} {
		nc := &v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID("uid-" + name), Labels: map[string]string{v1.NodePoolLabelKey: "pool-" + name}, Annotations: map[string]string{v1.CapacityTypeTransitionsAnnotationKey: "on-demand,reserved"}}, Status: v1.NodeClaimStatus{ProviderID: name}}
		cluster.UpdateNodeClaim(nc)
		cp.observations[name] = cloudprovider.NodeClaimState{ObservedAt: time.Now(), Scope: "shared", Price: 1, Labels: map[string]string{v1.CapacityTypeLabelKey: v1.CapacityTypeOnDemand}}
	}
	stale := cp.observations["stale"]
	stale.ObservedAt = time.Now().Add(-2 * time.Minute)
	cp.observations["stale"] = stale
	cp.observations["failed"] = cloudprovider.NodeClaimState{Scope: "shared"}
	unrelated := cp.observations["unrelated"]
	unrelated.Scope = "unrelated"
	cp.observations["unrelated"] = unrelated
	snapshot := refreshNodeClaimPrices(ctx, cluster, cp)
	if len(snapshot.states) != 2 {
		t.Fatalf("fresh observations = %d, want 2", len(snapshot.states))
	}
	blocked := snapshot.unavailablePools()
	if !blocked.HasAll("pool-fresh", "pool-stale", "pool-failed") || blocked.Has("pool-unrelated") {
		t.Fatalf("blocked pools = %v", blocked)
	}
}

func TestPriceGuardBeforeDisruptionActions(t *testing.T) {
	ctx := context.Background()
	q := NewQueue(nil, nil, nil, clock.RealClock{}, nil)
	sentinel := errors.New("coverage changed")
	cmd := &Command{CreationTimestamp: time.Now(), validatePrices: func(context.Context) error { return sentinel }}
	// A nil client would panic if either path reached tainting, launch, or deletion.
	if err := q.StartCommand(ctx, cmd); !errors.Is(err, sentinel) {
		t.Fatalf("start error = %v", err)
	}
	if err := q.waitOrTerminate(ctx, cmd); !IsUnrecoverableError(err) {
		t.Fatalf("delete error = %v", err)
	}
}

func TestReplacementObservedCost(t *testing.T) {
	replacement := &Replacement{Name: "replacement", NodeClaim: &pscheduling.NodeClaim{NodeClaimTemplate: pscheduling.NodeClaimTemplate{InstanceTypeOptions: cloudprovider.InstanceTypes{makeInstanceType("m5.large", 1)}}}}
	for _, tc := range []struct {
		name        string
		price       float64
		unknown     bool
		peerChanged bool
		reject      bool
	}{
		{name: "new replacement scope reassigns coverage in another pool", price: .0000001, peerChanged: true, reject: true},
		{name: "uncovered", price: 1}, {name: "covered", price: .0000001}, {name: "reserved overlay increases cost", price: 2, reject: true}, {name: "unknown replacement", unknown: true, reject: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before, fresh := priceSnapshot(2), priceSnapshot(2)
			if tc.peerChanged {
				before.claims["a"].Labels[v1.NodePoolLabelKey] = "pool-b"
				fresh.claims["a"].Labels[v1.NodePoolLabelKey] = "pool-b"
				peer := fresh.states["a"]
				peer.Price = 1
				fresh.states["a"] = peer
			}
			fresh.claims["replacement"] = &v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Name: "replacement", Labels: map[string]string{corev1.LabelInstanceTypeStable: "m5.large", corev1.LabelTopologyZone: "test-zone-1", v1.NodePoolLabelKey: "pool-a"}}}
			fresh.scopes["replacement"] = "group-a"
			if !tc.unknown {
				fresh.states["replacement"] = cloudprovider.NodeClaimState{Price: tc.price, Scope: "group-a"}
			}
			err := before.validateReplacements(fresh, []*Replacement{replacement})
			if err == nil {
				err = before.validate(fresh, sets.New("pool-a"))
			}
			if (err != nil) != tc.reject {
				t.Fatalf("replacement validation = %v, reject = %t", err, tc.reject)
			}
			if !tc.reject {
				if err := before.validate(fresh, sets.New("pool-a")); err != nil {
					t.Fatalf("own replacement invalidated unchanged sources: %v", err)
				}
			}
		})
	}
}
