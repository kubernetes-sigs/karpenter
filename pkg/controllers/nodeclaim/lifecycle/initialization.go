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

package lifecycle

import (
	"context"
	"fmt"
	"strings"

	"github.com/awslabs/operatorpkg/status"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/scheduling"
	nodeutils "sigs.k8s.io/karpenter/pkg/utils/node"
	nodeclaimutils "sigs.k8s.io/karpenter/pkg/utils/nodeclaim"
	"sigs.k8s.io/karpenter/pkg/utils/resources"
)

type Initialization struct {
	kubeClient client.Client
	clock      clock.Clock
}

// Reconcile checks for initialization based on if:
// a) its current status is set to Ready
// b) all the startup taints have been removed from the node
// c) all extended resources have been registered
// d) all expected DRA drivers have published a complete resource pool (when DRA is enabled)
// This method handles both nil nodepools and nodes without extended resources gracefully.
//
//nolint:gocyclo
func (i *Initialization) Reconcile(ctx context.Context, nodeClaim *v1.NodeClaim) (reconcile.Result, error) {
	if cond := nodeClaim.StatusConditions().Get(v1.ConditionTypeInitialized); !cond.IsUnknown() {
		// Ensure that we always set the status condition to the latest generation
		nodeClaim.StatusConditions(status.WithClock(i.clock)).Set(*cond)
		return reconcile.Result{}, nil
	}
	if !nodeClaim.StatusConditions().Get(v1.ConditionTypeRegistered).IsTrue() {
		return reconcile.Result{}, nil
	}
	node, err := nodeclaimutils.NodeForNodeClaim(ctx, i.kubeClient, nodeClaim)
	if err != nil {
		nodeClaim.StatusConditions(status.WithClock(i.clock)).SetUnknownWithReason(v1.ConditionTypeInitialized, "NodeNotFound", "Node not registered with cluster")
		return reconcile.Result{}, nil //nolint:nilerr
	}
	if nodeutils.GetCondition(node, corev1.NodeReady).Status != corev1.ConditionTrue {
		nodeClaim.StatusConditions(status.WithClock(i.clock)).SetUnknownWithReason(v1.ConditionTypeInitialized, "NodeNotReady", "Node status is NotReady")
		return reconcile.Result{}, nil
	}
	if taint, ok := StartupTaintsRemoved(node, nodeClaim); !ok {
		nodeClaim.StatusConditions(status.WithClock(i.clock)).SetUnknownWithReason(v1.ConditionTypeInitialized, "StartupTaintsExist", fmt.Sprintf("StartupTaint %q still exists", formatTaint(taint)))
		return reconcile.Result{}, nil
	}
	if taint, ok := KnownEphemeralTaintsRemoved(node); !ok {
		nodeClaim.StatusConditions(status.WithClock(i.clock)).SetUnknownWithReason(v1.ConditionTypeInitialized, "KnownEphemeralTaintsExist", fmt.Sprintf("KnownEphemeralTaint %q still exists", formatTaint(taint)))
		return reconcile.Result{}, nil
	}
	if name, ok := RequestedResourcesRegistered(node, nodeClaim); !ok {
		nodeClaim.StatusConditions(status.WithClock(i.clock)).SetUnknownWithReason(v1.ConditionTypeInitialized, "ResourceNotRegistered", fmt.Sprintf("Resource %q was requested but not registered", name))
		return reconcile.Result{}, nil
	}
	if driver, ok, err := i.draDriverPoolsPublished(ctx, node, nodeClaim); err != nil {
		return reconcile.Result{}, err
	} else if !ok {
		nodeClaim.StatusConditions(status.WithClock(i.clock)).SetUnknownWithReason(v1.ConditionTypeInitialized, "DRADriverPoolsNotPublished", fmt.Sprintf("DRA driver %q has not published a complete resource pool", driver))
		return reconcile.Result{}, nil
	}
	stored := node.DeepCopy()
	node.Labels = lo.Assign(node.Labels, map[string]string{v1.NodeInitializedLabelKey: "true"})
	if !equality.Semantic.DeepEqual(stored, node) {
		if err = i.kubeClient.Patch(ctx, node, client.MergeFrom(stored)); err != nil {
			return reconcile.Result{}, err
		}
	}
	log.FromContext(ctx).WithValues("allocatable", node.Status.Allocatable).Info("initialized nodeclaim")
	nodeClaim.StatusConditions(status.WithClock(i.clock)).SetTrue(v1.ConditionTypeInitialized)
	return reconcile.Result{}, nil
}

// KnownEphemeralTaintsRemoved validates whether all the ephemeral taints are removed
func KnownEphemeralTaintsRemoved(node *corev1.Node) (*corev1.Taint, bool) {
	for i := range node.Spec.Taints {
		if scheduling.IsKnownEphemeralTaint(&node.Spec.Taints[i]) {
			return &node.Spec.Taints[i], false
		}
	}
	return nil, true
}

// StartupTaintsRemoved returns true if there are no startup taints registered for the nodepool, or if all startup
// taints have been removed from the node
func StartupTaintsRemoved(node *corev1.Node, nodeClaim *v1.NodeClaim) (*corev1.Taint, bool) {
	if nodeClaim != nil {
		for _, startupTaint := range nodeClaim.Spec.StartupTaints {
			for i := range node.Spec.Taints {
				// if the node still has a startup taint applied, it's not ready
				if startupTaint.MatchTaint(&node.Spec.Taints[i]) {
					return &node.Spec.Taints[i], false
				}
			}
		}
	}
	return nil, true
}

// RequestedResourcesRegistered returns true if there are no extended resources on the node, or they have all been
// registered by device plugins
func RequestedResourcesRegistered(node *corev1.Node, nodeClaim *v1.NodeClaim) (corev1.ResourceName, bool) {
	for resourceName, quantity := range nodeClaim.Spec.Resources.Requests {
		if quantity.IsZero() {
			continue
		}
		// kubelet will zero out both the capacity and allocatable for an extended resource on startup, so if our
		// annotation says the resource should be there, but it's zero'd in both then the device plugin hasn't
		// registered it yet.
		// We wait on allocatable since this is the value that is used in scheduling
		if resources.IsZero(node.Status.Allocatable[resourceName]) {
			return resourceName, false
		}
	}
	return "", true
}

// draDriverPoolsPublished reports whether the NodeClaim's expected DRA drivers have all published a complete pool for
// the node. When DRA is disabled or the NodeClaim has no requested-dra-drivers annotation, it is a no-op and returns ("", true,
// nil). Otherwise it lists the node's ResourceSlices and returns the first driver missing a complete pool (with ok
// false), or ("", true, nil) when all expected drivers are satisfied.
func (i *Initialization) draDriverPoolsPublished(ctx context.Context, node *corev1.Node, nodeClaim *v1.NodeClaim) (string, bool, error) {
	if options.FromContext(ctx).IgnoreDRARequests {
		return "", true, nil
	}
	if _, ok := nodeClaim.Annotations[v1.DRADriversAnnotationKey]; !ok {
		return "", true, nil
	}
	slices, err := i.resourceSlicesForNode(ctx, node)
	if err != nil {
		return "", false, err
	}
	driver, ok := DRADriversPublished(nodeClaim, slices)
	return driver, ok, nil
}

// resourceSlicesForNode lists the ResourceSlices belonging to the given node. A slice belongs to the node when it pins
// itself via spec.nodeName or carries a Node owner reference naming the node — the two forms node-local DRA drivers
// use. Cluster-wide (AllNodes) slices are not node-owned and are intentionally excluded.
func (i *Initialization) resourceSlicesForNode(ctx context.Context, node *corev1.Node) ([]resourcev1.ResourceSlice, error) {
	sliceList := &resourcev1.ResourceSliceList{}
	if err := i.kubeClient.List(ctx, sliceList); err != nil {
		return nil, fmt.Errorf("listing resourceslices, %w", err)
	}
	var slices []resourcev1.ResourceSlice
	for i := range sliceList.Items {
		slice := &sliceList.Items[i]
		if sliceBelongsToNode(slice, node.Name) {
			slices = append(slices, *slice)
		}
	}
	return slices, nil
}

// sliceBelongsToNode reports whether a ResourceSlice is local to the named node, via spec.nodeName or a Node owner
// reference.
func sliceBelongsToNode(slice *resourcev1.ResourceSlice, nodeName string) bool {
	if lo.FromPtr(slice.Spec.NodeName) == nodeName {
		return true
	}
	for _, ref := range slice.OwnerReferences {
		if ref.Kind == "Node" && ref.Name == nodeName {
			return true
		}
	}
	return false
}

// DRADriversPublished checks whether every DRA driver recorded in the NodeClaim's requested-dra-drivers annotation has published
// at least one complete ResourceSlice pool among the node's slices. It returns the name of the first driver missing a
// complete pool and false, or ("", true) when all expected drivers are satisfied. A NodeClaim with no requested-dra-drivers
// annotation (or an empty value) is treated as having no expectation, so it returns ("", true).
//
// A pool is complete when the number of observed slices at the pool's highest generation equals the slices'
// declared ResourceSliceCount. This mirrors the allocator's pool-completeness notion (see Pool.Incomplete in
// pkg/scheduling/dynamicresources/pool.go) without taking on that package's dependencies.
func DRADriversPublished(nodeClaim *v1.NodeClaim, slices []resourcev1.ResourceSlice) (string, bool) {
	annotation := strings.TrimSpace(nodeClaim.Annotations[v1.DRADriversAnnotationKey])
	if annotation == "" {
		return "", true
	}
	driversWithCompletePool := completePoolDrivers(slices)
	for _, driver := range strings.Split(annotation, ",") {
		driver = strings.TrimSpace(driver)
		if driver == "" {
			continue
		}
		if !driversWithCompletePool.Has(driver) {
			return driver, false
		}
	}
	return "", true
}

// completePoolDrivers returns the set of driver names that have at least one complete pool among the given slices. A
// pool is complete when the number of observed slices at its highest generation equals their declared
// ResourceSliceCount.
func completePoolDrivers(slices []resourcev1.ResourceSlice) sets.Set[string] {
	type poolKey struct{ driver, pool string }
	// Track, per (driver, pool), the highest generation seen and the observed/declared slice counts at that generation.
	type poolObservation struct {
		generation         int64
		resourceSliceCount int64
		observed           int64
	}
	pools := map[poolKey]*poolObservation{}
	for i := range slices {
		s := &slices[i]
		key := poolKey{driver: s.Spec.Driver, pool: s.Spec.Pool.Name}
		gen := s.Spec.Pool.Generation
		obs, ok := pools[key]
		if !ok {
			pools[key] = &poolObservation{generation: gen, resourceSliceCount: s.Spec.Pool.ResourceSliceCount, observed: 1}
			continue
		}
		switch {
		case gen > obs.generation:
			// A newer generation supersedes older slices — reset the observation to this generation.
			obs.generation = gen
			obs.resourceSliceCount = s.Spec.Pool.ResourceSliceCount
			obs.observed = 1
		case gen == obs.generation:
			obs.observed++
		}
	}
	drivers := sets.New[string]()
	for key, obs := range pools {
		if obs.resourceSliceCount > 0 && obs.observed == obs.resourceSliceCount {
			drivers.Insert(key.driver)
		}
	}
	return drivers
}

func formatTaint(taint *corev1.Taint) string {
	if taint == nil {
		return "<nil>"
	}
	if taint.Value == "" {
		return fmt.Sprintf("%s:%s", taint.Key, taint.Effect)
	}
	return fmt.Sprintf("%s=%s:%s", taint.Key, taint.Value, taint.Effect)
}
