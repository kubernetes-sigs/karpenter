# RFC: Instance Type and Offerings Separation with Upstream Caching

## Overview

This design proposes separating static instance type data from mutable offerings and moving caching upstream to Karpenter core. Instance type specifications (vCPU, memory, architecture) rarely change, while offerings (pricing, availability, ICE errors) change frequently. The current architecture treats all this data uniformly, causing unnecessary deep copies and cache invalidation.

Fixes: https://github.com/kubernetes-sigs/karpenter/issues/2605

## User Stories

1. As a cluster operator, I want instance type lookups to be fast so that provisioning decisions happen quickly without unnecessary CPU overhead.
2. As a Karpenter developer, I want a single caching layer so that cache invalidation logic is centralized and easier to maintain.
3. As a cloud provider implementer, I want a simple interface for signaling data staleness so that I don't need to implement complex caching logic in my provider.
4. As a Karpenter developer, I want offerings cached separately from instance type info so that ICE errors don't invalidate the entire instance type cache.

## Problem Statement

The current architecture has several issues:

1. **Offerings regenerated on every call** - Even when nothing changed, offerings are rebuilt
2. **Deep copy overhead** - The overlay decorator must deep copy because offerings are mutable and embedded in instance types
3. **Mixed change frequencies** - Instance type specs change rarely, offerings change frequently (pricing updates, ICE errors with 3min TTL)
4. **Redundant caching** - Provider caches instance types, but offerings are regenerated anyway
5. **Cache invalidation complexity** - Hard to know when to invalidate because data is mixed

## Proposal

Separate instance types from offerings and move caching upstream to Karpenter core.

### CloudProvider Interface Changes

```go
type CloudProvider interface {
    // Existing methods...
    
    // DEPRECATED: Will be removed in future version
    GetInstanceTypes(context.Context, *v1.NodePool) ([]*InstanceType, error)
    
    // NEW: Static instance type specifications (rarely changes)
    GetInstanceTypeSpecs(context.Context, *v1.NodePool) ([]*InstanceTypeSpec, error)
    GetInstanceTypeSpecsGeneration(context.Context, *v1.NodePool) (uint64, error)
    
    // NEW: Dynamic offerings data (changes frequently)
    GetOfferings(context.Context, *v1.NodePool) (*OfferingsMap, error)
    GetOfferingsGeneration(context.Context, *v1.NodePool) (uint64, error)
}
```

### New Types

```go
// InstanceTypeSpec contains static instance type specifications.
// +k8s:deepcopy-gen=true
type InstanceTypeSpec struct {
    Name         string
    Requirements scheduling.Requirements
    Capacity     corev1.ResourceList
    Overhead     *InstanceTypeOverhead
}

// OfferingsMap contains point-in-time offerings data for all instance types.
type OfferingsMap struct {
    Offerings  map[string]Offerings
    Generation uint64
}
```

## Design Options

### Option 1: Full Separation - Recommended

Separate `GetInstanceTypeSpecs` and `GetOfferings` as distinct interface methods with independent caching.

Pros:
- Clean separation of concerns
- Independent cache invalidation (ICE errors only invalidate offerings)
- Optimal memory usage (specs cached long-term, offerings short-term)

Cons:
- Larger interface change (+4 methods)
- Requires all providers to implement new methods

### Option 2: Generation-Only

Keep `GetInstanceTypes` but add `GetInstanceTypesGeneration` for cache invalidation.

Pros:
- Smaller interface change (+1 method)
- Simpler migration path

Cons:
- Doesn't address the fundamental data model issue
- ICE errors still invalidate entire cache

### Option 3: Callback-Based Updates

Core Karpenter owns the cache and exposes callbacks (e.g., `SetOffering`, `MarkOfferingUnavailable`) that providers use to push updates. This enables granular updates but introduces inverted control flow, potential race conditions, and cache state that can diverge from provider state.

Pros:
- Most granular updates possible
- No polling overhead

Cons:
- Harder to reason about and test
- Race conditions possible

## Implementation Details

### InstanceTypeCache Decorator

A new decorator in core Karpenter caches instance type specs and offerings separately. When `GetInstanceTypes` is called:

1. Check generations for both specs and offerings
2. Refresh only the stale cache (specs or offerings, not both)
3. Combine specs + offerings into `[]*InstanceType`
4. Apply NodeOverlays if enabled
5. Return shared reference (no deep copy needed)

The decorator checks the `InstanceTypeCaching` feature gate and falls back to the provider's `GetInstanceTypes` if disabled. For backward compatibility, it also checks if the provider implements the new interface methods before using them.

### Generation Sources (AWS Provider Example)

| Generation | Changes When |
|------------|--------------|
| `InstanceTypeSpecsGeneration` | EC2 DescribeInstanceTypes (12h), zone offerings (12h), NodeClass changes |
| `OfferingsGeneration` | Pricing updates (12h), ICE errors (real-time, 3min TTL), capacity reservations, NodeClass changes |

## Migration Path

### Phase 1: Add Behind Feature Gate

- Add new methods to CloudProvider interface with default implementations
- Add `InstanceTypeCache` decorator (disabled by default)
- AWS provider implements new methods natively

### Phase 2: Deprecate and Remove

- Mark `GetInstanceTypes` as deprecated
- Update upstream to use new types
- Remove old interface in future major version

## Next Steps

- [ ] Define `InstanceTypeSpec` and `OfferingsMap` types in core Karpenter
- [ ] Add new methods to `CloudProvider` interface with default implementations
- [ ] Implement `InstanceTypeCache` decorator
- [ ] Update AWS provider to implement new methods
- [ ] Add metrics for cache hit/miss rates
- [ ] Update KWOK provider
