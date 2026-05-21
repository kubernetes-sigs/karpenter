# DRA — Cloud Provider Interface Extensions

## Overview

This document defines the extensions to the `cloudprovider` package required to support DRA structured parameters in Karpenter. The core Karpenter scheduler and NodeClaim lifecycle controllers are cloud-provider agnostic — they depend on interfaces and types defined in `pkg/cloudprovider/`. DRA support requires the cloud provider to supply device-level metadata for each instance type so the allocator can simulate device allocation during scheduling.

No changes to the `CloudProvider` interface itself are required. All extensions are to the `InstanceType` struct and its supporting types.

## New Types

The following types are added to `pkg/cloudprovider/`:

### ResourceSliceTemplate

The cloud provider type for in-memory device templates. This type is intentionally minimal — it only describes node-local devices (the only type supported for in-flight templates in the initial implementation). Node-affinity fields (`NodeName`, `NodeSelector`, `AllNodes`) are omitted since all cloud-provider-supplied templates are implicitly node-local.

```go
type ResourceSliceTemplate struct {
    // Driver is the DRA driver name (e.g., "gpu.nvidia.com").
    Driver unique.Handle[string]
    // Pool identifies the device pool this slice belongs to.
    Pool ResourcePool
    // Devices is the list of expected devices in this slice.
    Devices []Device
}

type ResourcePool struct {
    Name unique.Handle[string]
}

type Device struct {
    // Name uniquely identifies this device within its pool.
    Name unique.Handle[string]
    // Attributes are the device's properties, used for CEL selector evaluation
    // and MatchAttribute constraint checking.
    Attributes map[resourcev1.QualifiedName]resourcev1.DeviceAttribute
}
```

### Allocator Abstraction Layer

The allocator operates against both real (in-cluster) `resourcev1.ResourceSlice` objects and cloud-provider-supplied `ResourceSliceTemplate` objects. To avoid copying all in-cluster slices into a common struct on every scheduling loop, the allocator package defines an interface that both sources implement:

```go
// In the allocator package (not cloudprovider):
type ResourceSlice interface {
    Driver() unique.Handle[string]
    Pool() ResourcePool
    Devices() []Device
    // Potential returns true if this slice represents potential (not yet published)
    // devices — i.e., devices from a cloud provider template that would exist if a
    // given instance type were launched. Returns false for in-cluster slices that
    // have been published to the API server.
    //
    // The allocator uses this distinction for:
    //   - Tracking allocations: potential devices are tracked per-NodeClaim and
    //     per-InstanceType; published devices are tracked globally via AllocationTracker.
    //   - Requirement narrowing: published (non-potential) non-node-local devices
    //     may carry topology requirements that constrain the NodeClaim. Potential
    //     devices are always node-local and do not constrain topology.
    Potential() bool
    // NodeSelector returns the node selector if the slice uses label-based affinity, or nil.
    // Used during pool gathering to filter slices by NodeClaim compatibility.
    NodeSelector() *corev1.NodeSelector
    // AllNodes returns true if the slice's devices are accessible from all nodes.
    AllNodes() bool
    // Generation returns the pool generation this slice belongs to. Newer generations
    // supersede older ones during pool gathering. Template slices return 0.
    Generation() int64
    // ResourceSliceCount returns the total number of slices expected in this pool at
    // the current generation. Used to determine pool completeness. Template slices return 1.
    ResourceSliceCount() int64
}
```

This allows the allocator to work with both sources through a common interface without copying. The `cloudprovider.ResourceSliceTemplate` implements this interface with `Potential() → true`, `NodeSelector() → nil`, `AllNodes() → false`, `Generation() → 0`, `ResourceSliceCount() → 1`. An adapter for `resourcev1.ResourceSlice` implements it with `Potential() → false` and derives the remaining methods from the API object's fields.

### AttributeBinding

Declares that a set of devices on an instance type will share a common (but unknown at simulation time) value for a given attribute. This enables the allocator to satisfy `MatchAttribute` constraints for runtime-only attributes that cannot be included in the in-memory device templates.

```go
type AttributeBinding struct {
    // Devices is the set of devices that share the attribute value. Must contain
    // at least 2 devices; bindings with fewer are ignored.
    Devices []DeviceID
    // Attribute is the fully qualified attribute name that the devices share.
    Attribute resourcev1.QualifiedName
}
```

### DynamicResources

Groups all DRA-related metadata for an instance type.

```go
type DynamicResources struct {
    // ResourceSlices describes the DRA devices expected on this instance type.
    // Each entry is an in-memory template for a ResourceSlice that the corresponding
    // DRA driver is expected to publish once the node is running.
    //
    // Templates are used for scheduling simulation before real ResourceSlices are
    // available (pre-registration phase) and for in-flight NodeClaims where the
    // instance type has not been selected yet.
    //
    // Guidelines for cloud provider implementations:
    //   - Use the same device/attribute schema as real ResourceSlices so the allocator
    //     can use a single evaluation code path.
    //   - Include all attributes necessary for CEL selector evaluation during scheduling.
    //   - Omit runtime-only attributes (serial numbers, PCI addresses, UUIDs) that are
    //     not known until the node is running. These do not affect scheduling decisions.
    //   - Omit device taints (these are dynamic, applied by drivers or DeviceTaintRule objects).
    //   - Pool names may be synthetic placeholders — they do not need to match the actual
    //     pool names published by the driver.
    //   - All templates are implicitly node-local per the scheduling design.
    ResourceSliceTemplates []*ResourceSliceTemplate

    // AttributeBindings declares sets of devices that will share a common attribute
    // value at runtime, even though the concrete value is not known at simulation time.
    //
    // These are used to extend MatchAttribute constraint evaluation for in-flight devices.
    // When a MatchAttribute constraint references an attribute absent from the device
    // template (runtime-only), the allocator consults attribute bindings to determine
    // whether the constraint would be satisfied.
    //
    // Example: A GPU and NIC on the same instance type share a PCIE root ID. The cloud
    // provider declares an AttributeBinding with both device IDs and the PCIE root
    // attribute name. The allocator treats the MatchAttribute constraint as satisfied
    // for this pair without needing the concrete PCIE root value.
    //
    // Bindings are transitive: if A-B and B-C are declared in separate entries, the
    // allocator infers A-C. Transitivity is computed per (attribute, nodePool, instanceType)
    // triple and does not cross these boundaries.
    AttributeBindings []*AttributeBinding
}
```

## InstanceType Extension

Add the `DynamicResources` field to the existing `InstanceType` struct:

```go
type InstanceType struct {
    Name         string
    Requirements scheduling.Requirements
    Offerings    Offerings
    Capacity     corev1.ResourceList
    // DynamicResources contains DRA device metadata for this instance type.
    // Cloud providers that do not support DRA may leave this as the zero value.
    DynamicResources DynamicResources
    Overhead               *InstanceTypeOverhead
    // ... unexported fields unchanged ...
}
```

Deep copy for `DynamicResources` and its nested types is handled by the existing `controller-gen` deep copy generator (`//go:generate` directive in `types.go`).

## Usage by Karpenter Components

### Scheduling Allocator

The allocator consumes `DynamicResources` in two ways:

1. **ResourceSlice templates** — Accessed via the `NodeClaim` interface's `ResourceSlices()` method. For in-flight NodeClaims, the per-instance-type template map is built from `InstanceType.DynamicResources.ResourceSliceTemplates` for each candidate instance type, wrapped via the allocator's `ResourceSlice` interface. For pre-initialized existing nodes, only templates for drivers that have not yet published a complete pool are included.

2. **Attribute bindings** — `BuildAttributeBindings(instanceTypesByNodePool)` iterates over all instance types grouped by NodePool, collecting `InstanceType.DynamicResources.AttributeBindings` and building the transitive closure graph. This is called once at allocator construction time.

### NodeClaim Initialization

The initialization controller uses `DynamicResources.ResourceSliceTemplates` to determine the set of expected DRA drivers. The driver names are extracted from the `Driver` field of each template. The `karpenter.sh/dra-drivers` annotation is populated with this set during `FinalizeScheduling()`, and the initialization controller gates on all listed drivers publishing at least one complete pool.

## Cloud Provider Implementation Guide

Cloud providers implement DRA support by populating `DynamicResources` on the `InstanceType` objects returned from `GetInstanceTypes()`. The following guidance applies:

### ResourceSlice Construction

For each instance type, enumerate the DRA devices that the instance type's drivers will publish. Group them into `ResourceSliceTemplate` entries by driver:

```go
&cloudprovider.InstanceType{
    Name: "p5.48xlarge",
    // ... standard fields ...
    DynamicResources: cloudprovider.DynamicResources{
        ResourceSliceTemplates: []*cloudprovider.ResourceSliceTemplate{
            {
                Driver: unique.Make("gpu.nvidia.com"),
                Pool:   cloudprovider.ResourcePool{Name: unique.Make("p5-48xlarge-gpus")},
                Devices: []cloudprovider.Device{
                    {
                        Name: unique.Make("gpu-0"),
                        Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
                            "gpu.nvidia.com/model": {StringValue: lo.ToPtr("H100")},
                            "gpu.nvidia.com/memory": {IntValue: lo.ToPtr(int64(80))},
                        },
                    },
                    // ... gpu-1 through gpu-7 ...
                },
            },
        },
    },
}
```

### AttributeBinding Construction

For devices that share runtime-only attributes, declare bindings using `DeviceID` references:

```go
DynamicResources: cloudprovider.DynamicResources{
    ResourceSliceTemplates: []*cloudprovider.ResourceSliceTemplate{ /* ... */ },
    AttributeBindings: []*cloudprovider.AttributeBinding{
        {
            // GPU 0 and NIC 0 share a PCIE root
            Attribute: "topology.nvidia.com/pcie-root",
            Devices: []cloudprovider.DeviceID{
                {Driver: unique.Make("gpu.nvidia.com"), Pool: unique.Make("gpus"), Device: unique.Make("gpu-0")},
                {Driver: unique.Make("nic.aws.com"), Pool: unique.Make("nics"), Device: unique.Make("nic-0")},
            },
        },
        {
            // GPU 1 and NIC 1 share a PCIE root
            Attribute: "topology.nvidia.com/pcie-root",
            Devices: []cloudprovider.DeviceID{
                {Driver: unique.Make("gpu.nvidia.com"), Pool: unique.Make("gpus"), Device: unique.Make("gpu-1")},
                {Driver: unique.Make("nic.aws.com"), Pool: unique.Make("nics"), Device: unique.Make("nic-1")},
            },
        },
    },
}
```

### What to Include vs Omit

| Include | Omit |
|---------|------|
| Device model, family, generation | Serial numbers, UUIDs |
| Memory capacity, compute units | PCI bus addresses |
| Topology attributes used in CEL selectors | Device taints (dynamic) |
| Attributes referenced by known DeviceClasses | Exact pool names (use synthetic) |
| All attributes needed for `MatchAttribute` constraints (where value is known) | Runtime-only attributes (declare via `AttributeBindings` instead) |

### Cloud Providers Without DRA Support

Cloud providers that do not support DRA leave `DynamicResources` as the zero value. The allocator handles this gracefully — an instance type with no `ResourceSliceTemplates` simply has no in-flight devices to offer, and pods with DRA requirements will not be placed on NodeClaims backed by these instance types.
