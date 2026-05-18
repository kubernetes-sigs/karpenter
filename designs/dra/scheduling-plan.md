# DRA Scheduling — Implementation Plan

## Table of Contents

- [Overview](#overview)
- [Allocation Algorithm](#allocation-algorithm)
  - [Phase 1: Gather](#phase-1-gather)
  - [Phase 2: Request Validation](#phase-2-request-validation)
  - [Phase 3: DFS Allocation](#phase-3-dfs-allocation)
  - [Phase 4: Results](#phase-4-results)
- [State Management](#state-management)
  - [Top-Level Allocator State](#top-level-allocator-state)
  - [Per-Request Allocator State](#per-request-allocator-state)
  - [Allocation Commit Protocol](#allocation-commit-protocol)
  - [Attribute Bindings](#attribute-bindings)
- [Integration with Karpenter's Scheduler](#integration-with-karpenter-scheduler)
  - [Scheduler Initialization](#scheduler-initialization)
  - [Existing Node Evaluation](#existing-node-evaluation)
  - [In-Flight NodeClaim Evaluation](#in-flight-nodeclaim-evaluation)
  - [New NodeClaim Evaluation](#new-nodeclaim-evaluation)
  - [NodeClaim Constraint Propagation](#nodeclaim-constraint-propagation)
  - [Finalization and Annotation Population](#finalization-and-annotation-population)
- [2-Phase vs Superposed: Algorithm Selection](#2-phase-vs-superposed-algorithm-selection)
  - [Single-Pass Hybrid Approach](#single-pass-hybrid-approach)
  - [Incremental Requirement Tracking](#incremental-requirement-tracking)
- [Resolved Design Decisions](#resolved-design-decisions)
- [Open Design Decisions](#open-design-decisions)

---

## Overview

This plan describes the implementation of DRA structured parameters support in Karpenter's scheduling simulation. The allocator is responsible for determining whether a pod's `ResourceClaim` requests can be satisfied by a given NodeClaim, and if so, which devices should be allocated and how the NodeClaim's constraints should be tightened.

The implementation follows the upstream Kubernetes DRA allocator's structure (DFS with backtracking over a device decision tree) but diverges in how it handles in-flight NodeClaims, where the instance type is not yet determined and device availability varies across candidate instance types.

### Key References

| Reference | Path |
|-----------|------|
| Upstream stable allocator | `/Users/jmdeal/projects/kubernetes/staging/src/k8s.io/dynamic-resource-allocation/structured/internal/stable/` |
| Attribute bindings impl | `/Users/jmdeal/projects/karpenter/pkg/scheduling/dynamicresources/attributebindings.go` |
| Attribute bindings tests | `/Users/jmdeal/projects/karpenter/pkg/scheduling/dynamicresources/suite_test.go` |
| Karpenter scheduler | `pkg/controllers/provisioning/scheduling/scheduler.go` |
| NodeClaim scheduling | `pkg/controllers/provisioning/scheduling/nodeclaim.go` |
| Design doc | `designs/dra/scheduling.md` |
| NodeClaim lifecycle doc | `designs/dra/nodeclaim-lifecycle.md` |

### Scope Exclusions

The following DRA features are out of scope for the initial implementation:
- Admin access (`adminAccess: true`)
- Partitionable devices (shared counters)
- Consumable capacity (`allowMultipleAllocations`)
- Device taints
- Non-node-local in-flight devices (cross-NodeClaim allocation)
- Multi-solution optimization (finding the least-constraining allocation)

---

## Allocation Algorithm

The allocator processes a pod's ResourceClaims against a NodeClaim in four phases. These phases mirror the upstream allocator but are adapted for Karpenter's superposition model where a NodeClaim may represent many possible instance types.

### Phase 1: Gather

**Goal**: Collect the set of device pools accessible from the NodeClaim.

**Input**: A `NodeClaim` (via the `NodeClaim` interface defined in the design doc).

**Steps**:

1. Check the top-level `Allocator.poolCache` for a cached entry keyed by `NodeClaimID`. If a cached entry exists, it represents the superset of pools from the previous allocation — use it as the starting point and re-filter against the NodeClaim's current requirements (which may have been tightened by prior pod placements). This avoids re-scanning all cluster ResourceSlices each time, since the set can only shrink.

2. If no cache entry exists, build the pool set from scratch:
   - **In-cluster pools**: Derived from the pre-filtered `ResourceSlice` set provided to the `Allocator` at construction time. This set has already had slices from deleting/excluded nodes removed by the caller. For existing (initialized) nodes, these are the real published slices. For pre-initialized nodes, these are a merge of in-memory templates and any published slices (per the lifecycle doc's three-phase tracking strategy).
   - **In-flight pools** (for in-flight/new NodeClaims): Retrieve per-instance-type `ResourceSlice` sets from `NodeClaim.ResourceSlices()`. These represent the devices that *would* be available for each candidate instance type.

3. Validate each pool:
   - A pool is **complete** when the number of observed `ResourceSlice` objects matches `pool.resourceSliceCount`.
   - Incomplete pools are marked but not discarded — they may become complete as slices are published.
   - Pools with duplicate device names within a slice are marked `Invalid`. These pools are carried forward — `All` mode requests will fail validation if any gathered pool is invalid (see [Phase 2, Step 2d](#step-2-request-validation)).

4. Filter pools by node affinity:
   - Each slice's `nodeSelector` / `nodeName` / `allNodes` field must be compatible with the NodeClaim's current requirements.
   - For in-flight NodeClaims, a pool is included if it is compatible with *any* remaining candidate instance type. Pool-to-instance-type compatibility is tracked so Phase 3 can prune correctly.

**Note on caching**: The pool cache is **not updated** at the end of gather. The gathered-and-filtered pools are used for the current allocation attempt only. The cache is updated on **commit** (see [Allocation Commit Protocol](#allocation-commit-protocol)), where it is set to the pool set gathered at the *start* of the allocation (pre-filter). This way, the next allocation starts from a superset and re-filters, saving a full scan while still accounting for new constraints from the committed pod.

**Implementation notes**:
- Pool gathering logic should be extracted from the upstream `GatherPools()` function (`pools_stable.go:54-136`), simplified by removing partitionable device and per-device node selection codepaths.
- The `Pool` struct should track which instance types each pool is accessible from, enabling per-instance-type filtering in Phase 3.

```go
type Pool struct {
    ID        PoolID
    Slices    []*resourcev1.ResourceSlice
    Devices   []DeviceWithID
    Incomplete bool
    Invalid    bool
    // For in-flight NodeClaims: which instance types can see this pool.
    // nil means all instance types (in-cluster pool).
    InstanceTypes sets.Set[InstanceTypeID]
}
```

### Phase 2: Request Validation

**Goal**: Transform each `ResourceClaim` into a validated `ClaimData` containing parsed requests, resolved constraints, and pre-computed device sets for `All` mode.

**Entry point**: `ValidateClaimRequest(ctx, kubeClient, claim, claimIndex, pools, templateDevicesByIT, celCache, bindingFallback) → (*ClaimData, error)`

**Output**: `ClaimData` containing `Requests []RequestData` (one per device request) and `Constraints []Constraint` (one per claim-level constraint).

#### Step 1: Constraint Parsing

Each entry in `claim.Spec.Devices.Constraints` is converted into an internal `Constraint` implementation:

| Constraint Kind | Supported | Internal Type |
|-----------------|-----------|---------------|
| `MatchAttribute` | Yes | `MatchAttributeConstraint` |
| All others | No | Error: *"unsupported constraint type"* |

`MatchAttributeConstraint` tracks whether all devices allocated for the constrained requests share a common attribute value. The upstream implementation (`allocator_stable.go:606-689`) provides the reference — `add()` pins the attribute value on the first device and rejects mismatches on subsequent devices; `remove()` decrements the counter for backtracking.

Constraints may be scoped to a subset of requests via the `Requests` field (stored as `RequestNames sets.Set[string]`). If empty, the constraint applies to all requests in the claim. The optional `bindingFallback` is attached to every `MatchAttributeConstraint`.

#### Step 2: Request Validation

Only `Exactly` requests are supported. A request with `Exactly == nil` (i.e. `FirstAvailable` / sub-requests) is rejected with *"only Exactly requests are supported"*.

For each `Exactly` request, `validateExactRequest` performs:

**2a. DeviceClass Resolution**: The `DeviceClassName` is resolved via the API server. If the class does not exist, validation fails.

**2b. Selector Merging**: Selectors from the DeviceClass and the request are concatenated (class selectors first, then request selectors). All selectors must use CEL; any non-CEL selector is rejected with *"unsupported selector type"*.

**2c. CEL Compilation**: Every merged CEL expression is compiled via the shared `dracel.Cache`. Compilation failures are surfaced immediately (*"failed to compile"*) rather than deferred to evaluation time.

**2d. Allocation Mode Handling**:

| Mode | `NumDevices` | `AllDevices` | `AllTemplateDevicesByIT` |
|------|-------------|-------------|--------------------------|
| `ExactCount` | `req.Count` | `nil` | `nil` |
| `All` | `0` | Pre-computed eligible in-cluster devices | Pre-computed eligible template devices per instance type |

**All mode pre-computation:**

1. **In-cluster devices** (`collectAllModePoolDevices`): Iterates all pools. If any pool is `Invalid` (duplicate device names) or `Incomplete` (missing slices), validation fails immediately — `All` mode requires a complete and consistent view of all pools, so a single bad pool is a hard error. Devices in valid pools are filtered through the merged selectors via `DeviceMatchesSelectors` (AND semantics — all selectors must match).

2. **Template devices** (per instance type): Each instance type's template devices are filtered through the merged selectors. An instance type is **kept** in the map if it has at least one matching template device OR there are in-cluster matches (the instance type is still viable; it just contributes zero template devices). An instance type is **pruned** only when both its template matches and in-cluster matches are empty.

#### Step 3: Total Device Limit Enforcement

The Kubernetes API caps allocation results at `AllocationResultsMaxSize` (32) devices per claim. Validation enforces this in three stages:

**3a. Base Total Check**:
```
baseTotalDevices = sum(req.NumDevices) + sum(len(req.AllDevices))
```
If `baseTotalDevices > AllocationResultsMaxSize`, validation fails with *"exceeding maximum"*. This catches pure `ExactCount` overcounts, `All` mode in-cluster overcounts, and mixed claims where the combined total exceeds the limit.

**3b. Per-Instance-Type Pruning**: For each instance type present in any `All` mode request's `AllTemplateDevicesByIT`:
```
templateCount = sum(len(req.AllTemplateDevicesByIT[itID])) across all requests
if baseTotalDevices + templateCount > AllocationResultsMaxSize:
    delete itID from all requests' AllTemplateDevicesByIT
```
This removes instance types that would push the total over the limit while keeping those that fit.

**3c. Complete Pruning Detection**: If template devices were provided AND `All` mode requests initially had instance types in their maps, but all were pruned in step 3b, validation fails with *"all instance types pruned"*. No instance type can satisfy this claim without exceeding the device limit.

#### Error Conditions Summary

| Condition | Error Message Contains |
|-----------|----------------------|
| `FirstAvailable` request | `"only Exactly requests"` |
| Missing DeviceClass | `"not found"` |
| Non-CEL selector in class or request | `"unsupported selector type"` |
| Invalid CEL expression | `"failed to compile"` |
| Unsupported constraint type | `"unsupported constraint type"` |
| Invalid pool in All mode | `"invalid"` |
| Incomplete pool in All mode | `"incomplete"` |
| Total devices exceed limit | `"exceeding maximum"` |
| All instance types pruned | `"all instance types pruned"` |

### Phase 3: DFS Allocation

**Goal**: Find a valid assignment of devices to requests such that all constraints are satisfied.

This is the core of the allocator — a depth-first search over the device decision tree with backtracking. The DFS is executed **per instance type**: first, in-cluster devices are allocated in a single shared traversal. Then, for each candidate instance type, a separate traversal attempts to satisfy the outstanding requests (those not fulfilled by in-cluster devices) using that instance type's in-flight devices.

#### Decision Tree Structure

The tree has the following layers, from root to leaf:

```
Root
 └─ Claim 0
     └─ Request 0
         └─ Device slot 0 → try device A, device B, device C, ...
             └─ Device slot 1 → try device D, device E, ...
                 └─ ...
     └─ Request 1
         └─ ...
 └─ Claim 1
     └─ ...
 └─ ✓ Leaf (all requests satisfied → allocation found)
```

Each node in the tree represents a choice: which device to assign to this slot. The DFS tries each candidate, checks constraints, and either recurses deeper or backtracks.

#### Two-Stage Traversal

The DFS operates in two stages:

**Stage 1 — In-cluster allocation**: Run a single DFS over in-cluster devices (published ResourceSlices accessible to the node). This is independent of instance type. Devices allocated here are shared across all candidate instance types. If all requests are satisfied in this stage, the allocation is complete.

**Stage 2 — Per-instance-type allocation**: For each candidate instance type, run a separate DFS over that instance type's in-flight devices to satisfy the outstanding requests (those not fulfilled in Stage 1). Each instance type traversal inherits the constraint state from Stage 1 (the `MatchAttribute` pins, incremental requirements, etc.) but operates independently — a failure for one instance type does not affect others. Instance types that cannot satisfy the outstanding requests are pruned from the result.

This separation avoids the complexity of tracking semantically equivalent devices across instance types within a single traversal.

**Single-instance-type special case**: When there is exactly one candidate instance type (pre-initialized nodes, or in-flight NodeClaims constrained to one type), the two-stage split is skipped. Instead, in-cluster and in-flight devices are combined into a **single DFS traversal**, with in-cluster devices ordered first. This allows inter-request constraints spanning both device sources to be evaluated naturally and avoids the artificial stage boundary.

#### Device Candidate Sources

For each request slot, the candidate devices are drawn from:
1. **In-cluster pools**: Devices from published ResourceSlices visible to the node. For existing (initialized) nodes, this is the sole source of devices. For pre-initialized nodes (existing but not yet initialized), published slices are in the in-cluster pool; only the **outstanding** in-memory templates (for drivers that have not yet published a complete pool) appear in the in-flight map under the node's single known instance type.
2. **In-flight pools**: Devices from per-instance-type ResourceSlice templates (from `NodeClaim.ResourceSlices()`). For in-flight/new NodeClaims, each candidate instance type has its own set. For pre-initialized existing nodes, the single known instance type has its outstanding templates here.

#### Per-Device Evaluation

For each candidate device at a given tree node, the following checks are performed in order:

1. **Already in use?** Check three sources: `allocatedDevices` (committed in-cluster allocations), `inFlightAllocatedDevices[nodeClaimID][instanceTypeID]` (committed in-flight allocations), and `allocatingDevices` (devices being allocated in the current DFS path). If the device appears in any of these, skip it.

2. **Selector match?** Evaluate the device's attributes against the combined selectors from the DeviceClass and the request. CEL expressions are compiled once and cached in the shared `celCache` (mutex-protected, following the upstream approach). Results are cached per `(deviceID, requestIndex)` pair to avoid recomputation across backtracking iterations.

3. **Constraint satisfaction?** Call `constraint.add()` for each of the claim's constraints. If any constraint returns false (e.g., a `MatchAttribute` constraint finds the device has a different attribute value than the pinned value), backtrack. For in-flight devices where the constraint references a runtime-only attribute (value unknown at simulation time), attribute bindings are consulted as part of the constraint evaluation — see [Attribute Bindings](#attribute-bindings) for details.

4. **Requirement compatibility?** If the device is from an in-cluster (non-node-local) pool, check whether the device's topology requirements (e.g., zone) are compatible with the NodeClaim's current requirements. This is done by intersecting the device's implied requirements with the NodeClaim's:
   - If compatible: **narrow** the NodeClaim requirements for the remainder of the DFS subtree.
   - If incompatible: skip this device.
   - On backtrack: **revert** the narrowed requirements to the state before this device was allocated.

   > **Simplification (per design doc note)**: Rather than tracking the full NodeClaim requirement set at each tree node, track only the *incremental* requirements added by each allocated device. Since all devices are guaranteed compatible with the base NodeClaim (validated during gather), we only need to check that each new device's requirements are compatible with the *accumulated incremental requirements* from devices allocated earlier in the DFS. This avoids copying the full requirement set at each node.

#### Allocation and Backtracking

When a device passes all checks:
- Record the allocation: `(claimIndex, requestIndex, deviceIndex) → deviceID`
- Update the constraint state via `constraint.add()`
- If tracking incremental requirements, push the new requirement delta
- Recurse to the next device slot (or next request/claim if all slots filled)

When backtracking:
- Undo the constraint state via `constraint.remove()`
- Pop the incremental requirement delta
- Try the next candidate device

#### Early Termination

Early termination operates at two scopes:

- **Per-instance-type**: Each Stage 2 traversal terminates independently.
  - **Success**: When all outstanding requests are satisfied, return immediately with the first valid solution for this instance type.
  - **Failure**: When all candidates for a slot are exhausted, backtrack. If backtracking reaches the root, this instance type is pruned.
- **Global**: Context cancellation (5 second timeout) applies to the entire `Allocate()` call. Check `ctx.Done()` periodically (e.g., between instance type iterations) and abort all remaining work.

### Phase 4: Results

**Goal**: Package the allocation result for the scheduler.

**Output**: An `AllocationResult` containing:

The `Allocate()` method returns an `(AllocationResult, error)` tuple. A non-nil error indicates the allocation failed entirely (e.g., all instance types pruned, invalid claims, or context cancellation). The caller must check the error — an empty `InstanceTypes` slice alone does not indicate failure, since existing initialized NodeClaims may not provide instance type IDs.

1. **`InstanceTypes []InstanceTypeID`**: The set of instance types whose Stage 2 traversal succeeded (i.e., all outstanding requests were satisfied by that instance type's in-flight devices). For existing initialized nodes, this field may be empty even on success (there is no instance type set to prune). For in-flight NodeClaims, this must be a non-empty subset of the NodeClaim's current candidate instance types on success.

2. **`Requirements scheduling.Requirements`**: The accumulated incremental requirements from all allocated non-node-local devices (from both Stage 1 and Stage 2). These constrain the NodeClaim's topology (e.g., zone pinning from a zonal device).

3. **`Allocation`**: An opaque handle containing the allocation details needed to update allocator state on commit:
   - For in-cluster devices: the set of `DeviceID`s to mark as consumed.
   - For in-flight devices: the per-instance-type `DeviceID` sets to mark as consumed.
   - The NodeClaim ID the allocation is associated with.

---

## State Management

### Top-Level Allocator State

The `Allocator` struct is scoped to a single scheduling loop and is shared across all per-pod allocation requests. It is **read-only** during `Allocate()` calls and **mutated only** during initialization and `Commit()`.

```go
type Allocator struct {
    // The pre-filtered set of in-cluster ResourceSlices, provided at construction
    // time. The caller is responsible for excluding slices from deleting/excluded
    // nodes before passing them in. The allocator treats this as the complete
    // universe of in-cluster slices and does not query cluster state directly.
    inClusterSlices []*resourcev1.ResourceSlice

    // In-cluster devices that have been allocated. Initialized from the
    // deviceallocation controller's tracking state, then appended as devices
    // are committed during the scheduling simulation.
    allocatedDevices sets.Set[DeviceID]

    // Per-NodeClaim, per-InstanceType in-flight device allocations. Built up as
    // pods are committed to in-flight NodeClaims.
    inFlightAllocatedDevices map[NodeClaimID]map[InstanceTypeID]sets.Set[DeviceID]

    // The attribute binding adjacency graph. Built once at allocator construction
    // from the cloud provider's instance type metadata.
    attributeBindings AttributeBindings

    // Cached pool sets per NodeClaim. Stores the pre-filter pool superset from the
    // most recent allocation. On subsequent allocations, this set is re-filtered
    // against the NodeClaim's current (tightened) requirements, avoiding a full
    // scan of inClusterSlices.
    poolCache map[NodeClaimID][]Pool

    // Shared CEL compilation cache. The cel.Cache implementation is internally
    // synchronized (RWMutex), allowing compiled expressions to be reused across
    // parallel Allocate() calls without an external lock.
    celCache cel.Cache
}
```

**Initialization**:
- `inClusterSlices`: Provided by the caller at construction time. The caller (scheduler initialization) is responsible for collecting all accessible ResourceSlices and filtering out slices owned by deleting/excluded nodes. See [Scheduler Initialization](#scheduler-initialization) for details.
- `allocatedDevices`: Sourced from the `deviceallocation` controller, which already tracks allocated devices across the cluster. This avoids re-scanning all `ResourceClaim` objects. The set must be filtered to exclude devices allocated by **deleting pods** — this includes all pods on deleting nodes and disruption candidates, as well as pods that are individually deleting (observed `deletionTimestamp`). Devices allocated exclusively by pods in this set are excluded from `allocatedDevices`, making them available for reallocation in the simulation.
- `inFlightAllocatedDevices`: Starts empty. Populated as pods are committed to in-flight NodeClaims.
- `attributeBindings`: Call `BuildAttributeBindings(instanceTypesByNodePool)` with the cloud provider's instance type data, grouped by NodePool.
- `poolCache`: Starts empty.
- `celCache`: Initialized empty, populated lazily, shared across all `Allocate()` calls.

**Thread safety**: The scheduler evaluates a pod against multiple NodeClaims in parallel. All parallel `Allocate()` calls are **read-only** with respect to top-level state. The `celCache` uses RWMutex protection for concurrent reads with exclusive writes. Mutation of `allocatedDevices`, `inFlightAllocatedDevices`, and `poolCache` occurs only in `Commit()`, which is called sequentially after a placement decision is finalized.

### Per-Request Allocator State

The child `allocator` struct is created per `Allocate()` call. It embeds a pointer to the top-level `Allocator` for read access and holds mutable state for the current DFS.

```go
type allocator struct {
    *Allocator  // read-only access to shared state

    ctx       context.Context
    nodeClaim NodeClaim
    claims    []*resourcev1.ResourceClaim
    pools     []Pool

    // Per-claim constraint sets.
    constraints [][]constraint

    // Per-request metadata (device count, class, selectors, predetermined devices).
    requestData map[requestIndices]requestData

    // Devices being allocated in the current DFS path.
    // Maps deviceID → set of claim indices using it.
    allocatingDevices map[DeviceID]sets.Set[int]

    // Cache: does device X match request Y's selectors?
    deviceMatchesRequest map[matchKey]bool

    // The incremental requirements accumulated from non-node-local device allocations.
    // Pushed/popped during DFS.
    incrementalRequirements []scheduling.Requirements

    // The allocation result, populated when a solution is found.
    result []internalAllocationResult
}
```

### Allocation Commit Protocol

The scheduler calls `Allocator.Commit()` when a pod's placement on a NodeClaim is finalized. This is the point where speculative allocation becomes durable (for the scheduling loop).

```go
func (a *Allocator) Commit(ctx context.Context, nodeClaim NodeClaim, allocations []Allocation) {
    for _, alloc := range allocations {
        alloc.Commit()
    }
}
```

Each `Allocation` implementation updates the top-level allocator:

- **`inClusterAllocation.Commit()`**: Adds `deviceID` to `allocator.allocatedDevices`.
- **`inFlightAllocation.Commit()`**: Adds `deviceID` to `allocator.inFlightAllocatedDevices[nodeClaimID][instanceTypeID]`.

**Pool cache update on commit**: After committing allocations, update `poolCache[nodeClaimID]` to the pool set that was gathered at the **start** of this allocation (pre-filter, before applying the current NodeClaim's constraints). This preserves the superset — the next allocation will re-filter this cached set against the NodeClaim's newly tightened requirements, which saves the cost of a full ResourceSlice scan while still correctly reflecting new constraints added by the committed pod.

### Attribute Bindings

Attribute bindings model constraints where multiple devices on an instance will share an attribute value that is unknown at simulation time. Cloud providers declare these bindings to indicate that certain devices will satisfy a `MatchAttribute` constraint once the node is running, even though the concrete attribute value cannot be evaluated during scheduling.

**Example**: A GPU driver and NIC driver both expose a PCIE root attribute. A ResourceClaim requires that the allocated GPU and NIC share the same PCIE root (via a `MatchAttribute` constraint). The in-memory device templates cannot include the actual PCIE root ID (it's runtime-only), but the cloud provider's attribute bindings declare that specific GPU-NIC pairs on a given instance type will share this attribute.

**Construction**: `BuildAttributeBindings()` processes the cloud provider's `InstanceType.DynamicResources.AttributeBindings` declarations:
1. For each binding (a set of devices sharing an attribute), create symmetric pairs in the adjacency map.
2. Compute transitive closure per `(attribute, nodePool, instanceType)` triple using BFS. If A↔B and B↔C are declared separately, A↔C is inferred.
3. Self-loops are removed.

**Integration with `MatchAttribute` constraint evaluation**: Attribute bindings serve as a fallback for `MatchAttribute` constraints when the attribute value is absent from the in-memory device template. The concrete and binding paths are mutually exclusive per attribute: either all constrained devices have a concrete value (direct comparison) or none do (binding fallback). Mixing is not supported — a constraint attribute is either fully present in device templates or fully runtime-only. The evaluation rules are:

- If devices have a concrete value for the constrained attribute: compare directly using standard upstream logic. Attribute bindings are **not** consulted — they do not override or supplement direct comparison.
- If the attribute value is absent from devices (runtime-only attribute): consult `attributeBindings.Bound(nodePool, instanceTypeID, attribute, deviceA, deviceB)`. If bound, the constraint is treated as satisfied; if not, the constraint fails.
- A single ResourceClaim may have multiple `MatchAttribute` constraints on different attributes. Each constraint is evaluated independently — some may use direct comparison (for attributes present in templates) while others use attribute bindings (for runtime-only attributes). All constraints must pass for the device to be accepted.

**Key invariant**: Attribute bindings are scoped to `(attribute, nodePool, instanceType)`. Bindings do not cross these boundaries — a zone binding in pool-a does not imply a zone binding in pool-b.

---

## Integration with Karpenter's Scheduler

### Scheduler Initialization

**File**: `pkg/controllers/provisioning/scheduling/scheduler.go`

At scheduler construction (`NewScheduler`), build the DRA allocator:

1. **Collect and filter in-cluster ResourceSlices**: Gather all `ResourceSlice` objects from the cluster. Filter out slices owned by nodes that are not in the stateNode set passed to the scheduler (i.e., deleting nodes, disruption candidates). Non-node-owned slices (no node owner reference) are always included. This filtering uses `metadata.ownerReferences` to determine node ownership — `spec.nodeName` indicates accessibility, not ownership.
2. Obtain the set of allocated devices from the `deviceallocation` controller's tracking state, filtered for deleting pods. Deleting pods should include all pods on deleting nodes and disruption candidates.
3. Build `AttributeBindings` from instance type metadata grouped by NodePool.
4. Construct the `Allocator` with the filtered slice set, allocated device set, empty `inFlightAllocatedDevices`, empty `poolCache`, and empty `celCache`.

The allocator is stored on the `Scheduler` struct and passed through to NodeClaim evaluation.

### Existing Node Evaluation

**Integration point**: `scheduler.addToExistingNode()` → `NodeClaim.CanAdd()`

For existing (initialized) nodes:
1. The NodeClaim wraps a real node with a known instance type.
2. `NodeClaim.ResourceSlices()` returns an empty map — all published slices are already in the allocator's in-cluster pool set.
3. Call `Allocator.Allocate(ctx, nodeClaim, pod.ResourceClaims)`.
4. If allocation succeeds, the pod can be placed. The `AllocationResult.Allocation` is held until `Add()` is called.
5. On `Add()`, call `Allocator.Commit()` to mark the devices as consumed.

For pre-initialized nodes (existing but not yet fully initialized):
1. The NodeClaim wraps a real node with a known instance type, but some drivers have not yet published complete pools.
2. Published slices for registered drivers are already in the in-cluster pool set.
3. `NodeClaim.ResourceSlices()` returns only the **outstanding** in-memory templates — i.e., templates for drivers that have not yet published a complete pool. These appear under the single known instance type in the map.

4. **Single-instance-type optimization**: When there is exactly one candidate instance type (as is the case for pre-initialized nodes), the allocator skips the two-stage split and instead combines in-cluster and in-flight devices into a **single DFS traversal**. This avoids the artificial separation between stages and allows inter-request constraints that span in-cluster and in-flight devices to be evaluated naturally within one search. This optimization also applies to in-flight NodeClaims that have been constrained down to a single instance type by prior pod placements.

### In-Flight NodeClaim Evaluation

**Integration point**: `scheduler.addToInflightNode()` → `NodeClaim.CanAdd()`

For in-flight NodeClaims (created earlier in this scheduling loop):
1. The NodeClaim has multiple candidate instance types, each with its own ResourceSlice set.
2. `NodeClaim.ResourceSlices()` returns a map from `InstanceTypeID` to the slices that instance type would publish.
3. Call `Allocator.Allocate(ctx, nodeClaim, pod.ResourceClaims)`.
4. The allocator runs the 2-stage algorithm: Stage 1 allocates in-cluster devices, Stage 2 iterates each instance type for outstanding requests.
5. `AllocationResult.InstanceTypes` is the subset of instance types that support the allocation. The scheduler intersects this with the NodeClaim's current instance type set — if the intersection is empty, placement fails.
6. `AllocationResult.Requirements` contains any topology constraints from allocated devices (e.g., zone pinning). The scheduler merges these into the NodeClaim's requirements.
7. On `Add()`, commit the allocation.

### New NodeClaim Evaluation

**Integration point**: `scheduler.addToNewNodeClaim()` → `NodeClaim.CanAdd()`

Identical to in-flight evaluation. The only difference is the starting state: the NodeClaim begins with the full set of instance types from the NodePool template, unconstrained by prior pod placements.

### NodeClaim Constraint Propagation

When a pod with DRA requirements is added to a NodeClaim, the NodeClaim must be constrained in two ways, applied in order:

1. **Requirement tightening**: `AllocationResult.Requirements` adds topology constraints (e.g., `topology.kubernetes.io/zone: us-west-2a`). These are merged into the NodeClaim's requirements via `Requirements.Add()`, which performs intersection-based narrowing.

2. **Instance type re-evaluation**: After requirements are tightened, the full instance type filtering pipeline must be re-run — not just DRA-specific pruning. Tightened requirements may eliminate offerings for instance types that were previously compatible (e.g., an instance type that was available in us-west-2a and us-west-2b is now only viable if it has an us-west-2a offering). This is consistent with how Karpenter handles topology constraint tightening today.

   Additionally, `AllocationResult.InstanceTypes` provides a DRA-specific filter: only instance types whose Stage 2 traversal succeeded are retained. This is applied alongside the existing compatibility, fit, and offering filters in `filterInstanceTypesByRequirements()`.

Both of these happen inside `NodeClaim.Add()`, after the existing resource and topology constraint updates.

### Finalization and Annotation Population

**Integration point**: `scheduler.FinalizeScheduling()` → `NodeClaim.FinalizeScheduling()`

When the scheduling loop completes and NodeClaims are finalized:

1. Collect the set of DRA driver names from all ResourceClaims of pods scheduled to the NodeClaim.
2. Set the `karpenter.sh/dra-drivers` annotation on the NodeClaim (per the lifecycle doc).
3. The finalized NodeClaim carries both the standard resource requests and the DRA driver annotation for the initialization controller to gate on.

---

## 2-Phase vs Superposed: Algorithm Selection

The initial implementation uses the **2-phase** algorithm as described in the design doc, implemented as the two-stage traversal described in [Phase 3](#phase-3-dfs-allocation).

### Why 2-Phase First

1. **Deterministic in-cluster allocations**: By resolving in-cluster device allocations first (independently of instance type), the same devices are consumed regardless of which instance type is ultimately selected. This prevents pessimistic over-reservation where each instance type path claims different in-cluster devices.

2. **Performance**: The DFS depth is shallower because in-cluster and in-flight allocations are evaluated in separate passes rather than as a cross-product.

3. **Correctness gap is narrow**: The only scenario where 2-phase misses a valid allocation is when inter-request constraints span non-node-local and node-local devices *and* the constraint can only be satisfied by pairing a specific in-cluster device with a specific in-flight device. This is unlikely in practice with current drivers.

### Single-Pass Hybrid Approach

The design doc asks whether a hybrid approach can be achieved in a single pass. The answer is **yes**, by ordering device evaluation as follows:

1. For each request, present candidate devices in this order: **in-cluster devices first, then in-flight devices**.
2. The DFS naturally prefers in-cluster devices (tried first at each node in the tree).
3. If the DFS exhausts all candidates without finding a solution, backtracking naturally explores mixed allocations (some in-cluster, some in-flight) because earlier in-cluster choices are unwound and in-flight alternatives are tried.

This achieves 2-phase behavior in the common case (in-cluster devices are preferred) while falling back to superposed exploration on failure — all in a single DFS traversal. The key insight is that the DFS search order *is* the prioritization mechanism.

**Tradeoff**: The fallback path has superposed-level complexity (full cross-product search). However, it only triggers when the 2-phase ordering fails, which should be rare.

**Note**: This is documented as a potential future optimization. The initial implementation uses the simpler two-stage traversal (Stage 1: in-cluster only, Stage 2: per-instance-type in-flight only) to avoid the complexity of mixed traversals.

### Incremental Requirement Tracking

The design doc asks whether requirement tracking during DFS can be simplified by tracking only incremental requirements rather than the full NodeClaim requirement set.

**Yes.** The key observation is:

- All pools gathered in Phase 1 are already validated as compatible with the base NodeClaim requirements.
- Each allocated device may add requirements (e.g., zone constraints from non-node-local devices).
- We only need to verify that each new device's requirements are compatible with the **union of all prior incremental requirements** — not the full NodeClaim requirement set.

Implementation:
- Maintain a `scheduling.Requirements` accumulator initialized to empty.
- When allocating a non-node-local device, compute its implied requirements and call `accumulator.Compatible(deviceRequirements)`.
- If compatible, call `accumulator.Add(deviceRequirements)` to narrow it.
- On backtrack, restore the accumulator to its pre-allocation state (snapshot before `Add`).

This avoids copying and diffing the full NodeClaim requirements at every tree node.

---

## Resolved Design Decisions

1. **DFS timeout**: 5 second per-pod timeout, with the overall scheduling loop timeout as a hard upper bound.

2. **CEL cache scope**: Shared on the top-level `Allocator` with RWMutex protection, following the upstream approach.

3. **Pool cache strategy**: Incrementally narrowed. On commit, the cache stores the pre-filter pool superset. On subsequent allocations, this superset is re-filtered against tightened requirements. This saves a full ResourceSlice scan while correctly reflecting new constraints.

4. **All-mode device handling**: Pre-compute the predetermined device set per instance type during validation and store in `requestData`.

5. **Unsupported constraint types**: Fail the allocation for the claim, following upstream behavior. Unknown constraint types indicate API version skew.

6. **Attribute binding integration with MatchAttribute**: Attribute bindings are a fallback only — they are consulted when the constrained attribute is absent from the in-memory template. When concrete values are present, direct comparison is used exclusively. The concrete and binding paths are mutually exclusive per attribute: either all constrained devices have a concrete value or none do. A single claim may have multiple `MatchAttribute` constraints where some use direct comparison and others use bindings, but the same attribute will never use both mechanisms simultaneously.

7. **Single-instance-type optimization**: When there is exactly one candidate instance type, combine in-cluster and in-flight devices into a single DFS traversal instead of the two-stage split. This applies to pre-initialized existing nodes and in-flight NodeClaims constrained to one instance type.

8. **Deleting nodes and ResourceSlices**: ResourceSlices owned by deleting nodes are excluded from the allocator's in-cluster slice set at construction time (not inside the allocator). Ownership is determined via `metadata.ownerReferences`. Node-owned slices are excluded if the owner is not in the stateNode set; non-node-owned slices are always included. This works for both provisioning (`nodes.Active()` excludes deleting nodes) and disruption simulation (`SimulateScheduling()` further excludes candidates). The allocator receives a pre-filtered slice set and has no cluster state dependency. This is distinct from deleting-pod device filtering — deleting-node filtering removes *capacity*; deleting-pod filtering removes *allocations*. Deleting pods include all pods on deleting nodes and disruption candidates.

---

## Open Design Decisions

1. **Error reporting**: When allocation fails for all instance types, the error should indicate *why* — selector mismatch, constraint violation, or insufficient devices. However, with potentially hundreds of instance types, reporting per-instance-type failure reasons has high cardinality. This needs further design exploration before inclusion.

