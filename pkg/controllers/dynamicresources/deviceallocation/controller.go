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

package deviceallocation

import (
	"context"
	"fmt"
	"iter"
	"sync"
	"unique"

	"github.com/awslabs/operatorpkg/serrors"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	utilscontroller "sigs.k8s.io/karpenter/pkg/utils/controller"
)

const (
	minReconciles = 10
	maxReconciles = 3000
)

// DeviceContribution records what a single claim contributes to a device's allocation
type DeviceContribution struct {
	// ConsumedCapacity is the capacity this claim consumes on the device.
	// nil for exclusive allocations.
	ConsumedCapacity map[resourcev1.QualifiedName]resource.Quantity
}

type ClaimMetadata struct {
	Releasable    bool
	PodUIDs       []types.UID
	Contributions map[cloudprovider.DeviceID]DeviceContribution
}

// ContributionMetadata records a single claim's contribution to a shared device's consumed capacity, paired with the
// pods that reserve that claim. It lets consumers reason about which portion of a shared device's aggregated capacity
// belongs to which pods — for example, to free only the capacity held by pods that are being deleted.
type ContributionMetadata struct {
	// PodUIDs is the set of pod UIDs that reserve the claim contributing this capacity. Non-pod consumer UIDs are
	// excluded. Empty when the claim is reserved but not by any pod.
	PodUIDs []types.UID
	// ConsumedCapacity is the capacity this single claim consumes on the device.
	ConsumedCapacity map[resourcev1.QualifiedName]resource.Quantity
}

// Metadata contains supplementary information about an allocated device, derived from the ReservedFor status of all
// ResourceClaims that reference it.
type DeviceMetadata struct {
	// Releasable is true when every ResourceClaim referencing the device has a non-empty ReservedFor list composed
	// entirely of pod consumers. A device that is not reserved, or that is reserved by any non-pod consumer, is not
	// releasable.
	Releasable bool
	// PodUIDs is the aggregate set of pod UIDs from the ReservedFor entries of all ResourceClaims that reference the
	// device. Non-pod consumer UIDs are excluded. Duplicates are possible and consumers should not assume uniqueness.
	// This is intentionally a slice rather than a set for performance reasons, as membership is expected to be small.
	PodUIDs []types.UID
	// Shared is true when the device is multi-allocatable.
	Shared bool
	// ConsumedCapacity is the aggregated capacity consumed across all claims referencing this device.
	// Only populated when Shared is true.
	ConsumedCapacity map[resourcev1.QualifiedName]resource.Quantity
	// Contributions is the per-claim breakdown of consumed capacity on this device, one entry per referencing claim
	// that consumes capacity, each paired with the pods reserving that claim. Only populated when Shared is true. This
	// is the per-claim view that ConsumedCapacity aggregates; consumers that need to attribute capacity to specific
	// pods (e.g. to release only a deleting pod's share) use this instead of the aggregate.
	Contributions []ContributionMetadata
}

type Controller struct {
	kubeClient client.Client

	mu               sync.RWMutex
	allocatedDevices map[cloudprovider.DeviceID]DeviceMetadata
	claimsPerDevice  map[cloudprovider.DeviceID]sets.Set[types.NamespacedName]
	claimMetadata    map[types.NamespacedName]ClaimMetadata

	hydrationCh   chan struct{}
	hydrationOnce sync.Once
}

func NewController(kubeClient client.Client) *Controller {
	return &Controller{
		kubeClient:       kubeClient,
		allocatedDevices: make(map[cloudprovider.DeviceID]DeviceMetadata),
		claimsPerDevice:  make(map[cloudprovider.DeviceID]sets.Set[types.NamespacedName]),
		claimMetadata:    make(map[types.NamespacedName]ClaimMetadata),
		hydrationCh:      make(chan struct{}),
	}
}

func (c *Controller) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	c.Hydrate(ctx)

	claim := &resourcev1.ResourceClaim{}
	if err := c.kubeClient.Get(ctx, req.NamespacedName, claim, client.UnsafeDisableDeepCopy); err != nil {
		if apierrors.IsNotFound(err) {
			c.finalizeClaim(ctx, req.NamespacedName)
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, serrors.Wrap(fmt.Errorf("getting resourceclaim, %w", err), "resourceclaim", klog.KRef(req.Namespace, req.Name))
	}
	c.reconcileClaim(ctx, req.NamespacedName, claim)
	return reconcile.Result{}, nil
}

func (c *Controller) Hydrate(ctx context.Context) {
	c.hydrationOnce.Do(func() {
		// SAFETY: This list hits the informer cache, and should not error since it's already guaranteed to be synced.
		claimList := &resourcev1.ResourceClaimList{}
		lo.Must0(c.kubeClient.List(ctx, claimList, client.UnsafeDisableDeepCopy))
		for i := range claimList.Items {
			c.reconcileClaim(ctx, client.ObjectKeyFromObject(&claimList.Items[i]), &claimList.Items[i])
		}
		close(c.hydrationCh)
	})
}

//nolint:gocyclo
func (c *Controller) reconcileClaim(ctx context.Context, nn types.NamespacedName, claim *resourcev1.ResourceClaim) {
	if claim.Status.Allocation == nil || len(claim.Status.Allocation.Devices.Results) == 0 {
		c.finalizeClaim(ctx, nn)
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	contributions := make(map[cloudprovider.DeviceID]DeviceContribution, len(claim.Status.Allocation.Devices.Results))
	for i := range claim.Status.Allocation.Devices.Results {
		result := &claim.Status.Allocation.Devices.Results[i]
		deviceID := cloudprovider.DeviceID{
			Driver: unique.Make(result.Driver),
			Pool:   unique.Make(result.Pool),
			Device: unique.Make(result.Device),
		}
		contributions[deviceID] = DeviceContribution{ConsumedCapacity: result.ConsumedCapacity}
	}

	devicesToRemove := make(sets.Set[cloudprovider.DeviceID])
	for device := range c.claimMetadata[nn].Contributions {
		if _, still := contributions[device]; !still {
			devicesToRemove.Insert(device)
		}
	}

	var devicesToAdd sets.Set[cloudprovider.DeviceID]
	if log.FromContext(ctx).V(1).Enabled() {
		devicesToAdd = make(sets.Set[cloudprovider.DeviceID])
		for device := range contributions {
			if _, existed := c.claimMetadata[nn].Contributions[device]; !existed {
				devicesToAdd.Insert(device)
			}
		}
	}

	c.claimMetadata[nn] = claimMetadata(claim, contributions)
	for device := range devicesToRemove {
		c.claimsPerDevice[device].Delete(nn)
		if len(c.claimsPerDevice[device]) == 0 {
			delete(c.claimsPerDevice, device)
			delete(c.allocatedDevices, device)
		} else {
			c.allocatedDevices[device] = c.computeDeviceMetadata(device)
		}
	}
	for device := range contributions {
		claims, ok := c.claimsPerDevice[device]
		if !ok {
			claims = sets.New[types.NamespacedName]()
			c.claimsPerDevice[device] = claims
		}
		claims.Insert(nn)
		c.allocatedDevices[device] = c.computeDeviceMetadata(device)
	}

	if log.FromContext(ctx).V(1).Enabled() {
		log.FromContext(ctx).V(1).Info(
			"updated tracked devices for claim",
			"ResourceClaim", klog.KRef(nn.Namespace, nn.Name),
			"added", lo.Map(lo.Keys(devicesToAdd), deviceIDToString),
			"removed", lo.Map(lo.Keys(devicesToRemove), deviceIDToString),
			"tracked", lo.Map(lo.Keys(contributions), deviceIDToString),
		)
	}
}

func (c *Controller) finalizeClaim(ctx context.Context, nn types.NamespacedName) {
	c.mu.Lock()
	defer c.mu.Unlock()

	devices := c.claimMetadata[nn].Contributions
	for device := range devices {
		claims := c.claimsPerDevice[device]
		claims.Delete(nn)
		if len(claims) == 0 {
			delete(c.allocatedDevices, device)
			delete(c.claimsPerDevice, device)
		} else {
			c.allocatedDevices[device] = c.computeDeviceMetadata(device)
		}
	}
	delete(c.claimMetadata, nn)
	if log.FromContext(ctx).V(1).Enabled() {
		log.FromContext(ctx).V(1).Info(
			"updated tracked devices for claim",
			"ResourceClaim", klog.KRef(nn.Namespace, nn.Name),
			"added", []string{},
			"removed", lo.Map(lo.Keys(devices), deviceIDToString),
			"tracked", []string{},
		)
	}
}

// AllocatedDevices returns an iterator over all allocated devices and their metadata. The read lock is held for the
// duration of iteration and released when the iterator completes or the caller breaks out of the loop.
func (c *Controller) AllocatedDevices(ctx context.Context) (iter.Seq2[cloudprovider.DeviceID, DeviceMetadata], error) {
	select {
	case <-c.hydrationCh:
		return func(yield func(cloudprovider.DeviceID, DeviceMetadata) bool) {
			c.mu.RLock()
			defer c.mu.RUnlock()
			for id, meta := range c.allocatedDevices {
				if !yield(id, meta) {
					return
				}
			}
		}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// claimMetadata computes Metadata for a single claim from its ReservedFor entries.
func claimMetadata(claim *resourcev1.ResourceClaim, contributions map[cloudprovider.DeviceID]DeviceContribution) ClaimMetadata {
	meta := ClaimMetadata{Releasable: len(claim.Status.ReservedFor) > 0, Contributions: contributions}
	for i := range claim.Status.ReservedFor {
		ref := &claim.Status.ReservedFor[i]
		if ref.Resource == string(corev1.ResourcePods) && ref.APIGroup == "" {
			meta.PodUIDs = append(meta.PodUIDs, ref.UID)
		} else {
			meta.Releasable = false
		}
	}
	return meta
}

// computeDeviceMetadata aggregates metadata across all claims that reference a device.
// Must be called while holding c.mu.
func (c *Controller) computeDeviceMetadata(device cloudprovider.DeviceID) DeviceMetadata {
	meta := DeviceMetadata{Releasable: true}
	for nn := range c.claimsPerDevice[device] {
		claimMeta := c.claimMetadata[nn]
		if !claimMeta.Releasable {
			meta.Releasable = false
		}
		meta.PodUIDs = append(meta.PodUIDs, claimMeta.PodUIDs...)

		contributions := claimMeta.Contributions[device]
		if contributions.ConsumedCapacity != nil {
			meta.Shared = true
			meta.ConsumedCapacity = addCapacity(meta.ConsumedCapacity, contributions.ConsumedCapacity)
			meta.Contributions = append(meta.Contributions, ContributionMetadata{
				PodUIDs:          claimMeta.PodUIDs,
				ConsumedCapacity: contributions.ConsumedCapacity,
			})
		}
	}
	return meta
}

func (c *Controller) Register(ctx context.Context, m manager.Manager) error {
	if err := m.Add(manager.RunnableFunc(func(ctx context.Context) error {
		c.Hydrate(ctx)
		return nil
	})); err != nil {
		return fmt.Errorf("adding hydration runnable, %w", err)
	}
	return controllerruntime.NewControllerManagedBy(m).
		Named("dynamicresources.deviceallocation").
		For(&resourcev1.ResourceClaim{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: utilscontroller.LinearScaleReconciles(utilscontroller.CPUCount(ctx), minReconciles, maxReconciles)}).
		Complete(c)
}

func deviceIDToString(d cloudprovider.DeviceID, _ int) string {
	return d.String()
}

func addCapacity(dest, src map[resourcev1.QualifiedName]resource.Quantity) map[resourcev1.QualifiedName]resource.Quantity {
	if dest == nil {
		dest = make(map[resourcev1.QualifiedName]resource.Quantity, len(src))
	}
	for name, quantity := range src {
		current := dest[name]
		current.Add(quantity)
		dest[name] = current
	}
	return dest
}
