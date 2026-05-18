# DRA — NodeClaim Lifecycle

## Table of Contents

- [Overview](#overview)
- [Background: How DRA Works](#background-how-dra-works)
- [Scope](#scope)
- [NodeClaim Initialization](#nodeclaim-initialization)
  - [Problem Statement](#problem-statement)
  - [API Extension](#api-extension)
  - [Initialization Logic](#initialization-logic)
  - [Limitations](#limitations)
- [Resource Tracking During Scheduling](#resource-tracking-during-scheduling)
  - [Current Approach for Standard Resources](#current-approach-for-standard-resources)
  - [DRA Resource Tracking Strategy](#dra-resource-tracking-strategy)
  - [In-Memory ResourceSlice Model](#in-memory-resourceslice-model)
  - [Transition to Published ResourceSlices](#transition-to-published-resourceslices)
- [Scheduling Simulation](#scheduling-simulation)
- [Disruption](#disruption)
  - [Draining Nodes with DRA Devices](#draining-nodes-with-dra-devices)
  - [Non-Graceful Node Shutdown](#non-graceful-node-shutdown)
- [Drift Detection](#drift-detection)
- [Open Questions](#open-questions)

---

## Overview

This document describes how Karpenter manages the NodeClaim lifecycle in the presence of DRA (Dynamic Resource Allocation) devices. DRA introduces a fundamentally different resource model from extended resources: instead of scalar quantities published on the Node object, DRA uses `ResourceSlice` objects published by drivers, `ResourceClaim` objects that represent device requests, and an allocation model that tracks individual devices rather than fungible quantities.

Karpenter must adapt its NodeClaim lifecycle — initialization, resource tracking, and disruption — to account for DRA's device model while preserving the existing behavior for standard and extended resources.

## Background: How DRA Works

A brief summary of the DRA model relevant to Karpenter's concerns:

- **ResourceSlice**: Published by DRA drivers (one or more per node), containing a list of devices with attributes and capacities. Devices are grouped into **pools** within a slice. A pool is identified by `(driver, pool name)` and may span multiple ResourceSlice objects (tracked via `pool.resourceSliceCount`).
- **ResourceClaim**: A namespace-scoped request for one or more devices. Claims specify device requirements through `DeviceClass` selectors and optional CEL expressions. Once allocated, `status.allocation.devices.results[]` records the specific `(driver, pool, device)` tuples assigned.
- **Allocation lifecycle**: The scheduler allocates devices to claims, kubelet prepares them on the node, and deallocation occurs when all consumers release the claim.
- **Device taints**: Devices can carry taints (`NoSchedule`, `NoExecute`) that prevent new allocations or trigger eviction, similar to node taints.

## Scope

This document covers the NodeClaim lifecycle aspects of DRA support: initialization gating, resource tracking phases, disruption behavior, and drift detection.

The following DRA features are **out of scope** for the initial implementation and will be addressed in follow-up designs:
- **Admin access** (`adminAccess: true` on ResourceClaims)
- **Partitionable devices** (shared counters / overlapping device partitions)
- **Consumable capacity** (multi-allocatable devices with `allowMultipleAllocations`)
- **Consolidation** (device-aware consolidation, persistent cross-node claims)

The scheduling algorithm — including device matching, CEL selector evaluation, and bin-packing with DRA — is covered in a separate design document.

## NodeClaim Initialization

### Problem Statement

NodeClaim initialization is Karpenter's gate for transitioning from in-memory assumed resources to the actual resources published by the node. Once initialized, Karpenter relies on published data for scheduling decisions. For DRA, this means waiting for all expected DRA drivers to publish their `ResourceSlice` objects before marking the node as ready.

For extended resources, initialization tracks specific resource names via `nodeClaim.Spec.Resources.Requests` and waits for them to appear in `node.Status.Allocatable`. DRA cannot use the same mechanism because:

1. **High cardinality**: A single driver may publish dozens of devices across multiple pools. Enumerating every expected device attribute in the NodeClaim spec is impractical.
2. **Runtime-only attributes**: Some device attributes (e.g., serial numbers, PCI addresses) are only known after the instance launches. The in-memory model cannot perfectly predict the published slice shape.
3. **Non-deterministic pool names**: Pool names are driver-defined and may not be predictable at launch time.

Instead, we track initialization at the **driver** granularity: once at least one complete pool has been published for each expected driver, the node is considered initialized from a DRA perspective.

### Annotation

Store the expected DRA drivers as a comma-separated annotation on the NodeClaim rather than a spec field. The NodeClaim API is likely to evolve as DRA support matures, and an annotation avoids premature API commitment.

```go
const (
    // AnnotationDRADrivers is a comma-separated list of DRA driver names that are
    // expected to publish ResourceSlices for this node. The node will not be marked
    // as initialized until at least one complete pool has been published for each
    // listed driver.
    AnnotationDRADrivers = "karpenter.sh/dra-drivers"
)
```

Example:
```yaml
metadata:
  annotations:
    karpenter.sh/dra-drivers: "gpu.nvidia.com,network.aws.com"
```

This mirrors the philosophy of `Spec.Resources.Requests` for extended resources: we only block initialization on drivers explicitly required by scheduled pods, not all drivers that _might_ be installed. If a pod requests a device from a specific driver, we assume that driver is installed and will eventually publish its resources.

**Population**: During provisioning, when the scheduler creates a new `NodeClaim` for pods with DRA requirements, it collects the set of DRA driver names from the `DeviceClass` and `ResourceClaim` specs of those pods and sets the `karpenter.sh/dra-drivers` annotation on the NodeClaim.

### Initialization Logic

Extend `RequestedResourcesRegistered` (or add a sibling check) in the initialization controller:

```go
// DRADriversRegistered returns true if all expected DRA drivers have published
// at least one complete pool via ResourceSlices for this node.
func DRADriversRegistered(slices []*resourcev1.ResourceSlice, nodeClaim *v1.NodeClaim) (string, bool) {
    draDrivers := parseDRADriversAnnotation(nodeClaim)
    if len(draDrivers) == 0 {
        return "", true
    }

    // Build a map of driver -> set of (pool name, slice count, observed slices)
    // A pool is "complete" when the number of observed ResourceSlices with that
    // pool name equals pool.resourceSliceCount.
    type poolState struct {
        expectedCount int
        observedCount int
    }
    driverPools := map[string]map[string]*poolState{} // driver -> pool name -> state

    for _, slice := range slices {
        driver := slice.Spec.Driver
        poolName := slice.Spec.Pool.Name
        poolSliceCount := int(slice.Spec.Pool.ResourceSliceCount)

        if _, ok := driverPools[driver]; !ok {
            driverPools[driver] = map[string]*poolState{}
        }
        ps, ok := driverPools[driver][poolName]
        if !ok {
            ps = &poolState{expectedCount: poolSliceCount}
            driverPools[driver][poolName] = ps
        }
        ps.observedCount++
    }

    for _, driver := range draDrivers {
        pools, ok := driverPools[driver]
        if !ok {
            return driver, false // no slices published for this driver yet
        }
        // Check if at least one pool is complete
        hasCompletePool := false
        for _, ps := range pools {
            if ps.observedCount >= ps.expectedCount {
                hasCompletePool = true
                break
            }
        }
        if !hasCompletePool {
            return driver, false
        }
    }
    return "", true
}
```

The initialization controller queries ResourceSlices filtered by `spec.nodeName` matching the NodeClaim's node and runs this check alongside the existing extended resource and taint checks.

**Status condition**: When blocked on DRA driver registration, the initialization controller sets:
```
ConditionTypeInitialized = Unknown
Reason: "DRADriverNotRegistered"
Message: "DRA driver \"gpu.nvidia.com\" has not published a complete pool"
```

### Limitations

- **Multiple pools per driver**: A driver may publish multiple pools, but we only require _at least one_ complete pool. If a driver is expected to publish two pools and only publishes one, initialization will proceed. This is a pragmatic tradeoff — pool names are non-deterministic, so we cannot map expected pools to published pools. Most drivers publish a single pool per node.
  - **Future improvement**: Drivers could publish metadata (e.g., annotations or a well-known attribute) that Karpenter uses to validate pool completeness more precisely, or Karpenter could accept driver-defined match expressions.
- **Driver installed but no devices**: If a driver is installed but the instance type has no applicable devices, the driver should still publish an empty ResourceSlice (or a slice with zero devices). If it publishes nothing, initialization will hang until the registration TTL expires and the NodeClaim is garbage collected. This is analogous to a device plugin being installed but never registering its extended resource.

## Resource Tracking During Scheduling

### Current Approach for Standard Resources

Karpenter uses a three-phase strategy for tracking allocatable resources:

1. **Pre-registration** (no node yet): Use `nodeClaim.Status.Allocatable` — the in-memory record of assumed resources for the instance type.
2. **Post-registration, pre-initialization**: Combine node's published resources with the NodeClaim's. If a resource has a zero value (or is undefined) on the node but is present on the NodeClaim, use the NodeClaim's value. This accounts for drivers gradually registering resources.
3. **Post-initialization**: Use `node.Status.Allocatable` directly.

### DRA Resource Tracking Strategy

DRA devices cannot be tracked as scalar quantities on the node. Instead, we use an analogous three-phase strategy based on ResourceSlices:

1. **Pre-registration** (no node, no ResourceSlices): Use **in-memory ResourceSlice templates** from the instance type's known device profile. These are synthetic ResourceSlice-shaped objects that describe the devices the instance type is expected to have. They are generated from the cloud provider's instance type metadata (e.g., "p5.48xlarge has 8 H100 GPUs published by driver `gpu.nvidia.com`").

2. **Post-registration, pre-initialization** (node exists, ResourceSlices partially published): **Merge** the in-memory templates with published ResourceSlices. For each expected driver:
   - If the driver has published at least one complete pool, use the published pool(s) and discard the in-memory template for that driver.
   - If the driver has not yet published a complete pool, continue using the in-memory template for that driver.
   This ensures the scheduler can continue packing pods onto nodes that are still registering their DRA devices, without double-counting resources.

3. **Post-initialization** (all drivers registered): Use published ResourceSlices exclusively.

### In-Memory ResourceSlice Model

The cloud provider must supply a DRA device profile for each instance type. This is a new addition to the `InstanceType` interface:

```go
type InstanceType struct {
    // ... existing fields ...

    // DeviceSliceTemplates describes the DRA devices expected on this instance type.
    // Each entry is a template for a ResourceSlice that the corresponding DRA driver
    // is expected to publish once the node is running. These are used for scheduling
    // simulation before the real ResourceSlices are available.
    DeviceSliceTemplates []DeviceSliceTemplate
}

type DeviceSliceTemplate struct {
    // Driver is the DRA driver name (e.g., "gpu.nvidia.com").
    Driver string
    // Devices is the list of expected devices with their attributes and capacities.
    Devices []resourcev1.Device
}
```

**Key design choice**: The in-memory model uses the same device/attribute schema as real ResourceSlices. This allows the scheduling simulator to use a single code path for evaluating device availability regardless of whether the data source is in-memory or published.

**What the in-memory model cannot capture**:
- Runtime-only attributes (serial numbers, PCI addresses, UUIDs)
- Exact pool names (use synthetic placeholders)
- Device taints (these are dynamic, applied by drivers or `DeviceTaintRule` objects)

The in-memory model should include all attributes necessary for CEL selector evaluation during scheduling. Attributes that are purely informational or runtime-only should be omitted — they won't affect scheduling decisions.

### Transition to Published ResourceSlices

The transition happens per-driver, not atomically across all drivers. This means during the post-registration phase, the scheduler might use in-memory data for one driver and published data for another on the same node. This is acceptable because:

- Drivers are independent; one driver's registration doesn't affect another's devices.
- The in-memory model is conservative — it represents the minimum expected devices, not an optimistic upper bound.
- Once a driver publishes a complete pool, the real data immediately supersedes the template.

**Staleness protection**: When a driver publishes a complete pool, the scheduler must discard the in-memory template for that driver to avoid double-counting. The cluster state should track which drivers have completed registration per node.

## Scheduling Simulation

The scheduling algorithm for DRA — including device matching, CEL selector evaluation, and bin-packing with device-level tracking — is described in a separate design document.

At a high level, the scheduling simulation interacts with the NodeClaim lifecycle in the following ways:

- **Provisioning**: The scheduler uses the resource tracking phases described above (in-memory templates for new nodes, merged view for registering nodes, published slices for initialized nodes) to determine device availability. After scheduling, it populates the `karpenter.sh/dra-drivers` annotation with the set of drivers required by the scheduled pods.

## Disruption

### Draining Nodes with DRA Devices

When a NodeClaim is being drained (due to drift, expiration, etc.):

1. Pods are evicted following the standard drain flow (respecting PDBs, do-not-disrupt, terminationGracePeriod).
2. As each pod terminates, kubelet calls `NodeUnprepareResource` for its devices.
3. The pod is removed from `claim.Status.ReservedFor`.
4. Once `ReservedFor` is empty, the claim is deallocated (for ephemeral claims) or remains allocated but unused (for persistent claims).
5. The `DeviceAllocationController` observes these changes and updates its tracking maps.

No special drain ordering is required for DRA pods in the initial implementation. Standard drain semantics (PDBs, grace periods) apply uniformly.

### Non-Graceful Node Shutdown

When a node fails (goes out-of-service):

1. The node is tainted with `node.kubernetes.io/out-of-service:NoExecute`.
2. The GC controller deletes pods on the node.
3. `NodeUnprepareResource` is **not** called (the node is offline).
4. ResourceClaims are eventually deallocated by the kube-controller-manager.
5. DRA drivers must handle cleanup independently (e.g., polling cloud APIs).

**Karpenter's role**: Karpenter should not wait for DRA device cleanup before deleting the NodeClaim. Once the node is confirmed terminated (via cloud provider), Karpenter can proceed with NodeClaim deletion. The DRA driver is responsible for cleaning up its own state (ResourceSlices, device configurations) asynchronously.

However, Karpenter should be aware that ResourceSlices for a terminated node may linger briefly. The scheduler must filter out ResourceSlices whose `spec.nodeName` references a node that no longer exists or whose NodeClaim is terminating.

## Drift Detection

DRA introduces new drift vectors that Karpenter should detect:

1. **DeviceSliceTemplate drift**: If the cloud provider updates the expected device profile for an instance type (e.g., a new driver version publishes different device attributes), existing nodes running the old profile are drifted. This is analogous to AMI drift.

2. **DeviceClass drift**: If an admin updates a `DeviceClass` that is referenced by pods on a node, the node's device allocations may no longer match the updated class. This is a policy-level drift, not an infrastructure drift. However, existing allocations are immutable — the impact would only be felt by new pods. This is better handled as a NodePool-level policy change rather than node-level drift.

3. **ResourceSlice divergence**: If the published ResourceSlices no longer match the expected device profile (e.g., a device fails and the driver removes it from the slice), this could be considered drift. However, device failures are better handled by device taints (`NoSchedule`/`NoExecute`) rather than node replacement.

For the initial implementation, drift detection should focus on (1) — DeviceSliceTemplate drift from the cloud provider — and leave (2) and (3) as future considerations.

## Open Questions

1. **Driver-level vs. pool-level initialization tracking**: The current design tracks initialization at driver granularity. Is this sufficient for drivers that publish multiple pools with different device types? Should we consider a mechanism for drivers to signal expected pool count? (See [Limitations](#limitations))

2. **In-memory model fidelity**: How much fidelity does the in-memory DeviceSliceTemplate need? Should it include all CEL-evaluable attributes, or only the subset used by known DeviceClasses in the cluster? A minimal model reduces cloud provider complexity but risks scheduling mismatches.
