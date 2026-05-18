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

// Internal-package tests for the NodeClaim optimization pass. These
// exercise functions and state that aren't reachable from the
// scheduling_test package — findSplitPoint, applySplit/RevertTo,
// reconcileReservations — by constructing NodeClaims directly with
// the fields each test needs populated.
//
// Behavioral tests that go through Scheduler.Solve live in
// nodeclaim_optimization_test.go (scheduling_test package).

package scheduling

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
	pscheduling "sigs.k8s.io/karpenter/pkg/scheduling"
)

// reservedOffering returns a minimally-populated reserved offering suitable
// for exercising the strict-mode guard in findSplitPoint. The reservation
// ID and zone are parameterized so callers can construct distinct offerings
// without cross-test collision.
func reservedOffering(reservationID, zone string, price float64) *cloudprovider.Offering {
	return &cloudprovider.Offering{
		Available:           true,
		ReservationCapacity: 1,
		Price:               price,
		Requirements: pscheduling.NewLabelRequirements(map[string]string{
			v1.CapacityTypeLabelKey:          v1.CapacityTypeReserved,
			corev1.LabelTopologyZone:         zone,
			cloudprovider.ReservationIDLabel: reservationID,
		}),
	}
}

// instanceTypeWithOffering builds an InstanceType sized to obviously fit a
// small pod, carrying a single offering. The test fixture below uses two
// tiers to force a cheaper-earlier-state + more-efficient split candidate
// that would otherwise be accepted.
func instanceTypeWithOffering(name string, cpu, mem string, of *cloudprovider.Offering) *cloudprovider.InstanceType {
	return fake.NewInstanceType(fake.InstanceTypeOptions{
		Name: name,
		Resources: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse(cpu),
			corev1.ResourceMemory: resource.MustParse(mem),
			corev1.ResourcePods:   resource.MustParse("32"),
		},
		Offerings: []*cloudprovider.Offering{of},
	})
}

// setupFindSplitPointClaim builds a NodeClaim with:
//   - Two scheduling options in history: a cheap one (candidate) and an
//     expensive one (current). Price delta > minPriceSavingsRatio and
//     efficiency delta > minEfficiencyGain, so findSplitPoint would
//     otherwise accept the split.
//   - No displaced pods, so estimateCheapestPlacement returns (nil, +Inf)
//     but candidate.Price + +Inf >= current.Price still exits via the cost
//     check. Avoid that by leaving the displacement cost calculable — we
//     place one pod per option via cachedPodData.
//
// Returns the NodeClaim and a cachedPodData map sufficient to drive the
// cost estimate.
func setupFindSplitPointClaim(reservedMode ReservedOfferingMode, heldOfferings cloudprovider.Offerings) (*NodeClaim, map[types.UID]*PodData) {
	// Small instance (candidate): cheap, packs well for the first pod.
	smallOf := &cloudprovider.Offering{
		Available: true,
		Price:     0.10,
		Requirements: pscheduling.NewLabelRequirements(map[string]string{
			v1.CapacityTypeLabelKey:  v1.CapacityTypeOnDemand,
			corev1.LabelTopologyZone: "test-zone-1",
		}),
	}
	smallIT := instanceTypeWithOffering("small", "2", "4Gi", smallOf)

	// Large instance (current): forced by a larger-than-small pod. Price
	// and efficiency both satisfy the split heuristic thresholds relative
	// to the small instance.
	largeOf := &cloudprovider.Offering{
		Available: true,
		Price:     0.80,
		Requirements: pscheduling.NewLabelRequirements(map[string]string{
			v1.CapacityTypeLabelKey:  v1.CapacityTypeOnDemand,
			corev1.LabelTopologyZone: "test-zone-1",
		}),
	}
	largeIT := instanceTypeWithOffering("large", "16", "32Gi", largeOf)

	nodePoolReqs := pscheduling.NewLabelRequirements(map[string]string{
		v1.CapacityTypeLabelKey:  v1.CapacityTypeOnDemand,
		corev1.LabelTopologyZone: "test-zone-1",
	})

	// Two pods: one that fit the small tier, one that forces the large.
	pod1 := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod-1", UID: types.UID("pod-1")}}
	pod2 := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod-2", UID: types.UID("pod-2")}}

	n := &NodeClaim{
		Pods:                 []*corev1.Pod{pod1, pod2},
		reservedOfferings:    heldOfferings,
		reservedOfferingMode: reservedMode,
		optimizationState: optimizationState{
			enabled:               true,
			nodePoolRequirements:  nodePoolReqs,
			nodePoolInstanceTypes: []*cloudprovider.InstanceType{smallIT, largeIT},
			schedulingOptions: []SchedulingOption{
				{
					PodCount:         1,
					InstanceTypes:    []*cloudprovider.InstanceType{smallIT, largeIT},
					Requirements:     pscheduling.NewRequirements(nodePoolReqs.Values()...),
					ResourceRequests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("1Gi")},
					CheapestInstance: smallIT,
					Price:            0.10,
				},
				{
					PodCount:         2,
					InstanceTypes:    []*cloudprovider.InstanceType{smallIT, largeIT},
					Requirements:     pscheduling.NewRequirements(nodePoolReqs.Values()...),
					ResourceRequests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("3"), corev1.ResourceMemory: resource.MustParse("3Gi")},
					CheapestInstance: largeIT,
					Price:            0.80,
				},
			},
		},
	}

	podData := map[types.UID]*PodData{
		pod1.UID: {
			Requests:     corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("1Gi")},
			Requirements: pscheduling.NewRequirements(),
		},
		pod2.UID: {
			Requests:     corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2"), corev1.ResourceMemory: resource.MustParse("2Gi")},
			Requirements: pscheduling.NewRequirements(),
		},
	}
	return n, podData
}

func TestFindSplitPoint_StrictModeWithHeldReservation_ReturnsMinusOne(t *testing.T) {
	held := cloudprovider.Offerings{reservedOffering("res-1", "test-zone-1", 0.05)}
	n, podData := setupFindSplitPointClaim(ReservedOfferingModeStrict, held)

	if got := n.findSplitPoint(podData); got != -1 {
		t.Fatalf("findSplitPoint returned %d; want -1 (strict mode + held reservation must skip split)", got)
	}
}

func TestFindSplitPoint_StrictModeNoReservation_NotGuardedByStrictMode(t *testing.T) {
	n, podData := setupFindSplitPointClaim(ReservedOfferingModeStrict, cloudprovider.Offerings{})

	// With strict mode but no held reservation, the strict-mode guard must
	// not fire. Whatever findSplitPoint returns (based on price/efficiency)
	// is fine here — what we assert is that the decision reaches the normal
	// heuristic path at all. A test-fixture price/efficiency setup that
	// intentionally admits a split gives us a concrete positive assertion.
	got := n.findSplitPoint(podData)
	if got < 0 {
		t.Fatalf("findSplitPoint returned %d; want a valid split index (strict mode without a held reservation should not be guarded). "+
			"If heuristic thresholds have been retuned such that the fixture no longer admits a split, either update the fixture "+
			"or tighten this assertion to only verify the guard didn't fire.", got)
	}
}

func TestFindSplitPoint_FallbackModeWithHeldReservation_NotGuardedByStrictMode(t *testing.T) {
	held := cloudprovider.Offerings{reservedOffering("res-1", "test-zone-1", 0.05)}
	n, podData := setupFindSplitPointClaim(ReservedOfferingModeFallback, held)

	got := n.findSplitPoint(podData)
	if got < 0 {
		t.Fatalf("findSplitPoint returned %d; want a valid split index (fallback mode is not guarded). "+
			"If heuristic thresholds have been retuned such that the fixture no longer admits a split, either update the fixture "+
			"or tighten this assertion to only verify the guard didn't fire.", got)
	}
}

// newBareTopologyGroup constructs a minimal TopologyGroup with a known Key
// and Type, seeded with the given initial per-domain counts. Used only by
// the RevertTo tests below — production Topology/TopologyGroup is built via
// NewTopology/NewTopologyGroup and driven by pod affinity specs.
func newBareTopologyGroup(key string, typ TopologyType, initial map[string]int32) *TopologyGroup {
	tg := &TopologyGroup{
		Key:          key,
		Type:         typ,
		domains:      map[string]int32{},
		emptyDomains: sets.New[string](),
		owners:       map[types.UID]struct{}{},
	}
	for d, count := range initial {
		tg.domains[d] = count
		if count == 0 {
			tg.emptyDomains.Insert(d)
		}
	}
	return tg
}

// podWithHostPort builds a pod object with a single container declaring the
// given HostPort. HostPortUsage keys off client.ObjectKeyFromObject, so the
// pod's name and namespace are what matters for accounting — not the
// container spec itself.
func podWithHostPort(name string, port int32) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", UID: types.UID(name)},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name:  "c",
				Ports: []corev1.ContainerPort{{HostPort: port, Protocol: corev1.ProtocolTCP}},
			}},
		},
	}
}

// TestRevertTo_RestoresLocalFields verifies the minimum contract from the
// local-state-only step 5: Pods, InstanceTypeOptions, Requirements, and
// Spec.Resources.Requests all match the snapshot after revert.
//
// This complements the Solve-level coverage in nodeclaim_optimization_test.go
// with a tight, named assertion on each field — future changes to RevertTo
// shouldn't silently drop one.
func TestRevertTo_RestoresLocalFields(t *testing.T) {
	smallOf := &cloudprovider.Offering{
		Available: true,
		Price:     0.10,
		Requirements: pscheduling.NewLabelRequirements(map[string]string{
			v1.CapacityTypeLabelKey: v1.CapacityTypeOnDemand,
		}),
	}
	largeOf := &cloudprovider.Offering{
		Available: true,
		Price:     0.80,
		Requirements: pscheduling.NewLabelRequirements(map[string]string{
			v1.CapacityTypeLabelKey: v1.CapacityTypeOnDemand,
		}),
	}
	smallIT := instanceTypeWithOffering("small", "2", "4Gi", smallOf)
	largeIT := instanceTypeWithOffering("large", "16", "32Gi", largeOf)

	snapReqs := pscheduling.NewLabelRequirements(map[string]string{v1.CapacityTypeLabelKey: v1.CapacityTypeOnDemand})
	snapRequests := corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")}

	n := &NodeClaim{
		Pods:          []*corev1.Pod{{ObjectMeta: metav1.ObjectMeta{Name: "p1", UID: "p1"}}, {ObjectMeta: metav1.ObjectMeta{Name: "p2", UID: "p2"}}},
		topology:      &Topology{topologyGroups: map[uint64]*TopologyGroup{}, inverseTopologyGroups: map[uint64]*TopologyGroup{}},
		hostPortUsage: pscheduling.NewHostPortUsage(),
		optimizationState: optimizationState{
			enabled: true,
			schedulingOptions: []SchedulingOption{
				{PodCount: 1, InstanceTypes: []*cloudprovider.InstanceType{smallIT, largeIT}, Requirements: snapReqs, ResourceRequests: snapRequests, CheapestInstance: smallIT, Price: 0.10},
				{PodCount: 2, InstanceTypes: []*cloudprovider.InstanceType{largeIT}, Requirements: pscheduling.NewRequirements(), ResourceRequests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("3")}, CheapestInstance: largeIT, Price: 0.80},
			},
			podTopologyDeltas: [][]TopologyDelta{nil, nil}, // no topology mutations for this pod
		},
		NodeClaimTemplate: NodeClaimTemplate{
			NodeClaim:           v1.NodeClaim{Spec: v1.NodeClaimSpec{Resources: v1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("3")}}}},
			InstanceTypeOptions: []*cloudprovider.InstanceType{largeIT},
			Requirements:        pscheduling.NewRequirements(),
		},
	}

	displaced := n.RevertTo(0)

	if len(displaced) != 1 || displaced[0].Name != "p2" {
		t.Fatalf("displaced pods = %v; want [p2]", displaced)
	}
	if len(n.Pods) != 1 || n.Pods[0].Name != "p1" {
		t.Fatalf("n.Pods after revert = %v; want [p1]", n.Pods)
	}
	if len(n.InstanceTypeOptions) != 2 {
		t.Fatalf("n.InstanceTypeOptions len = %d; want 2 (both tiers from snapshot)", len(n.InstanceTypeOptions))
	}
	if got, want := n.Spec.Resources.Requests.Cpu().String(), "1"; got != want {
		t.Fatalf("n.Spec.Resources.Requests.Cpu = %s; want %s", got, want)
	}
	if len(n.schedulingOptions) != 0 {
		t.Fatalf("schedulingOptions len = %d; want 0 (truncated to [:index])", len(n.schedulingOptions))
	}
	if len(n.podTopologyDeltas) != 1 {
		t.Fatalf("podTopologyDeltas len = %d; want 1 (truncated to [:PodCount])", len(n.podTopologyDeltas))
	}
}

// TestRevertTo_ReversesTopologyCounts is the load-bearing correctness check
// for step 6's per-pod topology reversal. Two displaced pods have incremented
// distinct domains on the same TopologyGroup; after RevertTo those
// decrements must leave counts matching only the pods still on the claim.
//
// The requirement-tightening angle from the 2026-05-04 revision log: pod A
// was added with a delta targeting "zone-a" (because at the time the claim's
// zone requirement was looser and resolved to zone-a). Pod B later tightened
// the claim's zone to "zone-b". Under a mirror-iteration Unrecord that
// re-derives domains from n.Requirements we'd incorrectly decrement zone-b
// twice. Delta-recording ensures each pod's reversal targets the exact
// domain it incremented.
func TestRevertTo_ReversesTopologyCounts_RequirementTightening(t *testing.T) {
	// TopologyGroup seeded as if pod A and pod B had each incremented their
	// own zone (zone-a and zone-b). We construct the group directly; what
	// matters for RevertTo is the stored deltas, not how the group got here.
	tg := newBareTopologyGroup(corev1.LabelTopologyZone, TopologyTypeSpread, map[string]int32{"zone-a": 1, "zone-b": 1})

	// A "snapshot" pod that stays on the claim — its zone count must NOT be
	// decremented by revert.
	tg.domains["zone-kept"] = 1
	tg.emptyDomains.Delete("zone-kept")

	podSnap := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "snap", Namespace: "default", UID: "snap"}}
	podA := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "default", UID: "a"}}
	podB := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "b", Namespace: "default", UID: "b"}}

	deltasSnap := []TopologyDelta{{group: tg, domains: []string{"zone-kept"}}}
	deltasA := []TopologyDelta{{group: tg, domains: []string{"zone-a"}}} // recorded when zone was multi-valued
	deltasB := []TopologyDelta{{group: tg, domains: []string{"zone-b"}}} // tightened claim Requirements before being added

	n := &NodeClaim{
		Pods: []*corev1.Pod{podSnap, podA, podB},
		topology: &Topology{
			topologyGroups:        map[uint64]*TopologyGroup{1: tg},
			inverseTopologyGroups: map[uint64]*TopologyGroup{},
		},
		hostPortUsage: pscheduling.NewHostPortUsage(),
		optimizationState: optimizationState{
			enabled: true,
			schedulingOptions: []SchedulingOption{
				{PodCount: 1, Requirements: pscheduling.NewRequirements(), ResourceRequests: corev1.ResourceList{}, InstanceTypes: nil, CheapestInstance: nil},
				{PodCount: 3, Requirements: pscheduling.NewRequirements(), ResourceRequests: corev1.ResourceList{}, InstanceTypes: nil, CheapestInstance: nil},
			},
			podTopologyDeltas: [][]TopologyDelta{deltasSnap, deltasA, deltasB},
		},
	}

	_ = n.RevertTo(0)

	if got, want := tg.domains["zone-a"], int32(0); got != want {
		t.Fatalf("tg.domains[zone-a] = %d; want %d (decremented by A's delta)", got, want)
	}
	if got, want := tg.domains["zone-b"], int32(0); got != want {
		t.Fatalf("tg.domains[zone-b] = %d; want %d (decremented by B's delta)", got, want)
	}
	if got, want := tg.domains["zone-kept"], int32(1); got != want {
		t.Fatalf("tg.domains[zone-kept] = %d; want %d (must not be touched — pod still on claim)", got, want)
	}
	if !tg.emptyDomains.Has("zone-a") || !tg.emptyDomains.Has("zone-b") {
		t.Fatalf("zone-a and zone-b should be in emptyDomains after reaching zero; got %v", tg.emptyDomains)
	}
	if tg.emptyDomains.Has("zone-kept") {
		t.Fatalf("zone-kept should not be in emptyDomains; got %v", tg.emptyDomains)
	}
}

// TestRevertTo_DeletesHostPortUsageForDisplacedPods asserts that
// HostPortUsage.reserved contains no entries for displaced pods after
// revert, while entries for retained pods are preserved. Mirror of
// hostPortUsage.Add in NodeClaim.Add.
func TestRevertTo_DeletesHostPortUsageForDisplacedPods(t *testing.T) {
	hpu := pscheduling.NewHostPortUsage()

	podSnap := podWithHostPort("snap", 8080)
	podDisp := podWithHostPort("disp", 8081)

	hpu.Add(podSnap, pscheduling.GetHostPorts(podSnap))
	hpu.Add(podDisp, pscheduling.GetHostPorts(podDisp))

	n := &NodeClaim{
		Pods: []*corev1.Pod{podSnap, podDisp},
		topology: &Topology{
			topologyGroups:        map[uint64]*TopologyGroup{},
			inverseTopologyGroups: map[uint64]*TopologyGroup{},
		},
		hostPortUsage: hpu,
		optimizationState: optimizationState{
			enabled: true,
			schedulingOptions: []SchedulingOption{
				{PodCount: 1, Requirements: pscheduling.NewRequirements(), ResourceRequests: corev1.ResourceList{}},
				{PodCount: 2, Requirements: pscheduling.NewRequirements(), ResourceRequests: corev1.ResourceList{}},
			},
			podTopologyDeltas: [][]TopologyDelta{nil, nil},
		},
	}

	n.RevertTo(0)

	// Re-Adding podDisp's original ports on a fresh pod object should succeed
	// now that DeletePod reclaimed the seat. If revert didn't reverse the
	// host-port reservation, Conflicts would return an error here.
	freshDisp := podWithHostPort("disp-fresh", 8081)
	if err := hpu.Conflicts(freshDisp, pscheduling.GetHostPorts(freshDisp)); err != nil {
		t.Fatalf("HostPortUsage still holds a reservation for displaced pod's port 8081 after RevertTo: %v", err)
	}

	// And podSnap's port must still be reserved — Conflicts from a different
	// pod trying to use port 8080 should fail.
	interloper := podWithHostPort("interloper", 8080)
	if err := hpu.Conflicts(interloper, pscheduling.GetHostPorts(interloper)); err == nil {
		t.Fatalf("HostPortUsage lost the retained pod's port 8080 reservation during RevertTo")
	}

	// Sanity: the displaced pod's original key is gone.
	// We check via Conflicts for an unrelated pod trying to reuse the key —
	// harder to test directly without exporting reserved, but the two
	// Conflicts checks above together establish the contract.
	_ = client.ObjectKeyFromObject(podDisp)
}

// instanceTypeWithReservedOfferings builds an InstanceType carrying the
// given reserved offerings. Used to construct the snapshot's instance-type
// list so reconcileReservations's snapshotIDs rebuild picks up exactly
// the intended reservation IDs.
func instanceTypeWithReservedOfferings(name, cpu, mem string, ofs ...*cloudprovider.Offering) *cloudprovider.InstanceType {
	return fake.NewInstanceType(fake.InstanceTypeOptions{
		Name: name,
		Resources: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse(cpu),
			corev1.ResourceMemory: resource.MustParse(mem),
			corev1.ResourcePods:   resource.MustParse("32"),
		},
		Offerings: ofs,
	})
}

// TestRevertTo_ReservationIntersection_HoldDropReacquire covers the
// motivating case from the 2026-05-04 reservation revision log:
//
//	snapshot    : holds {r-A, r-B}
//	later Add   : drops r-B (requirements tightened past it)
//	later Add   : re-acquires r-B (another claim released a seat)
//	RevertTo(0) : intersection keeps both {r-A, r-B}
//
// An earlier design draft that used "current minus snapshot" would release
// the re-acquired r-B seat even though the snapshot includes it. The
// intersection approach covers that case correctly.
func TestRevertTo_ReservationIntersection_HoldDropReacquire(t *testing.T) {
	offA := reservedOfferingWithCapacity("res-A", "zone-1")
	offB := reservedOfferingWithCapacity("res-B", "zone-2")

	// Snapshot instance types contain both offerings.
	snapIT := instanceTypeWithReservedOfferings("snap", "4", "8Gi", offA, offB)

	rm := NewReservationManager(map[string][]*cloudprovider.InstanceType{"np": {snapIT}})

	hostname := "hostname-under-test"
	// Reserve both offerings up-front so the manager accounts for them.
	rm.Reserve(hostname, offA, offB)

	n := &NodeClaim{
		hostname:           hostname,
		reservationManager: rm,
		reservedOfferings:  cloudprovider.Offerings{offA, offB},
		topology: &Topology{
			topologyGroups:        map[uint64]*TopologyGroup{},
			inverseTopologyGroups: map[uint64]*TopologyGroup{},
		},
		hostPortUsage: pscheduling.NewHostPortUsage(),
		optimizationState: optimizationState{
			enabled: true,
			schedulingOptions: []SchedulingOption{
				{PodCount: 0, InstanceTypes: []*cloudprovider.InstanceType{snapIT}, Requirements: pscheduling.NewRequirements(), ResourceRequests: corev1.ResourceList{}},
				{PodCount: 0, InstanceTypes: []*cloudprovider.InstanceType{snapIT}, Requirements: pscheduling.NewRequirements(), ResourceRequests: corev1.ResourceList{}},
			},
			podTopologyDeltas: [][]TopologyDelta{},
		},
	}

	n.RevertTo(0)

	ids := sets.New[string]()
	for _, o := range n.reservedOfferings {
		ids.Insert(o.ReservationID())
	}
	if !ids.Has("res-A") || !ids.Has("res-B") {
		t.Fatalf("n.reservedOfferings IDs = %v; want both res-A and res-B retained (intersection)", ids)
	}
	if got := len(n.reservedOfferings); got != 2 {
		t.Fatalf("n.reservedOfferings len = %d; want 2", got)
	}

	// Both seats must still be held by this hostname — releaseReservedOfferings
	// should have been a no-op here (nothing fell outside the intersection).
	if !rm.HasReservation(hostname, offA) {
		t.Fatalf("reservation manager lost res-A on hostname after revert")
	}
	if !rm.HasReservation(hostname, offB) {
		t.Fatalf("reservation manager lost res-B on hostname after revert")
	}
}

// TestRevertTo_ReservationIntersection_ReleasesCurrentNotInSnapshot
// establishes the other half of the intersection contract. The claim holds
// {r-A, r-B}; the snapshot only contains r-A (r-B was never reachable at
// snapshot time). RevertTo must release r-B back to the shared pool so a
// fresh NodeClaim built from displaced pods can reserve it.
func TestRevertTo_ReservationIntersection_ReleasesCurrentNotInSnapshot(t *testing.T) {
	offA := reservedOfferingWithCapacity("res-A", "zone-1")
	offB := reservedOfferingWithCapacity("res-B", "zone-2")

	// Snapshot instance type carries only offA — r-B is NOT reachable at
	// snapshot time (simulates the "monotonic narrowing" assumption from
	// the design's reservation revision: at snapshot time the claim's
	// instance-type list was broader and only contained the A tier).
	snapIT := instanceTypeWithReservedOfferings("snap", "4", "8Gi", offA)

	rm := NewReservationManager(map[string][]*cloudprovider.InstanceType{
		"np": {instanceTypeWithReservedOfferings("big", "16", "32Gi", offA, offB)},
	})

	hostname := "hostname-under-test"
	rm.Reserve(hostname, offA, offB)
	preRevertCapB := rm.RemainingCapacity(offB)

	n := &NodeClaim{
		hostname:           hostname,
		reservationManager: rm,
		reservedOfferings:  cloudprovider.Offerings{offA, offB},
		topology: &Topology{
			topologyGroups:        map[uint64]*TopologyGroup{},
			inverseTopologyGroups: map[uint64]*TopologyGroup{},
		},
		hostPortUsage: pscheduling.NewHostPortUsage(),
		optimizationState: optimizationState{
			enabled: true,
			schedulingOptions: []SchedulingOption{
				{PodCount: 0, InstanceTypes: []*cloudprovider.InstanceType{snapIT}, Requirements: pscheduling.NewRequirements(), ResourceRequests: corev1.ResourceList{}},
				{PodCount: 0, InstanceTypes: []*cloudprovider.InstanceType{snapIT}, Requirements: pscheduling.NewRequirements(), ResourceRequests: corev1.ResourceList{}},
			},
			podTopologyDeltas: [][]TopologyDelta{},
		},
	}

	n.RevertTo(0)

	ids := sets.New[string]()
	for _, o := range n.reservedOfferings {
		ids.Insert(o.ReservationID())
	}
	if !ids.Has("res-A") {
		t.Fatalf("n.reservedOfferings must retain res-A; got IDs = %v", ids)
	}
	if ids.Has("res-B") {
		t.Fatalf("n.reservedOfferings must release res-B (not in snapshot); got IDs = %v", ids)
	}

	// Manager-side assertion: capacity for res-B must have grown back by 1
	// (the seat was returned to the pool). Capacity for res-A unchanged.
	if rm.HasReservation(hostname, offB) {
		t.Fatalf("reservation manager still shows res-B held by %s after revert", hostname)
	}
	if got, want := rm.RemainingCapacity(offB), preRevertCapB+1; got != want {
		t.Fatalf("RemainingCapacity(res-B) = %d; want %d (seat should have been released)", got, want)
	}
	if !rm.HasReservation(hostname, offA) {
		t.Fatalf("reservation manager lost res-A on %s during revert", hostname)
	}
}

// reservedOfferingWithCapacity builds a reserved offering with an explicit
// capacity count so the ReservationManager constructor picks it up with
// room to actually reserve. The simpler reservedOffering helper doesn't
// set ReservationCapacity, which would cause rm.capacity[id] to be 0 and
// Reserve to panic under over-reservation.
func reservedOfferingWithCapacity(reservationID, zone string) *cloudprovider.Offering {
	return &cloudprovider.Offering{
		Available:           true,
		ReservationCapacity: 1,
		Price:               0.05,
		Requirements: pscheduling.NewLabelRequirements(map[string]string{
			v1.CapacityTypeLabelKey:          v1.CapacityTypeReserved,
			corev1.LabelTopologyZone:         zone,
			cloudprovider.ReservationIDLabel: reservationID,
		}),
	}
}

// TestRevertTo_DoubleRevertIsNoOpAtSchedulerLevel verifies the spec's
// double-revert guarantee: once optimizeNodeClaims has locked a claim, a
// second pass finds no unlocked claims and produces no displaced pods.
// RevertTo itself has no internal lock check (the caller enforces the
// precondition); the lock lives one layer up. This test captures the
// contract at the right layer.
func TestRevertTo_DoubleRevertIsNoOpAtSchedulerLevel(t *testing.T) {
	n := &NodeClaim{
		Pods: []*corev1.Pod{{ObjectMeta: metav1.ObjectMeta{Name: "p1", UID: "p1"}}},
		optimizationState: optimizationState{
			enabled: true,
			// Locked — mirrors the state after a first optimizeNodeClaims pass.
			locked: true,
			schedulingOptions: []SchedulingOption{
				{PodCount: 1, Requirements: pscheduling.NewRequirements(), ResourceRequests: corev1.ResourceList{}},
			},
			podTopologyDeltas: [][]TopologyDelta{nil},
		},
	}

	s := &Scheduler{
		newNodeClaims: []*NodeClaim{n},
		cachedPodData: map[types.UID]*PodData{},
	}

	q := NewQueue(nil, s.cachedPodData)
	if ok := s.optimizeNodeClaims(q); ok {
		t.Fatalf("optimizeNodeClaims returned true for a locked claim; want false (no displacements)")
	}
	if len(n.Pods) != 1 {
		t.Fatalf("locked claim's Pods changed: %d; want 1 (locked claim must be skipped)", len(n.Pods))
	}
}
