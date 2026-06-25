# DRA Scheduling

## Table of Contents

- [Overview](#overview)
- [Scope Exclusions](#scope-exclusions)
- [Device Sources](#device-sources)
- [ResourceClaim State Handling](#resourceclaim-state-handling)
- [Pool Management](#pool-management)
  - [Pool Gathering](#pool-gathering)
  - [Pool Filtering](#pool-filtering)
  - [Pool Caching](#pool-caching)
- [Attribute Bindings](#attribute-bindings)
  - [Structure](#structure)
  - [Transitivity](#transitivity)
  - [Construction](#construction)
  - [Attribute Lookup](#attribute-lookup)
- [Request Validation](#request-validation)
- [Constraints](#constraints)
  - [MatchAttribute](#matchattribute)
- [Allocation Algorithm](#allocation-algorithm)
  - [Claim Classification](#claim-classification)
  - [Per-Instance-Type DFS](#per-instance-type-dfs)
  - [Device Eligibility Checks](#device-eligibility-checks)
  - [Allocation Modes](#allocation-modes)
  - [Backtracking](#backtracking)
  - [Timeout](#timeout)
- [State Management](#state-management)
  - [Top-Level Allocator State](#top-level-allocator-state)
  - [Per-Request Allocator State](#per-request-allocator-state)
- [Commit Protocol](#commit-protocol)
  - [Commit Behavior](#commit-behavior)
  - [Instance Type Release](#instance-type-release)
- [Integration with Karpenter's Scheduler](#integration-with-karpenters-scheduler)
  - [Scheduler Initialization](#scheduler-initialization)
  - [NodeClaim Abstraction](#nodeclaim-abstraction)
  - [Existing Node Evaluation](#existing-node-evaluation)
  - [In-Flight NodeClaim Evaluation](#in-flight-nodeclaim-evaluation)
  - [New NodeClaim Evaluation](#new-nodeclaim-evaluation)
  - [Constraint Propagation](#constraint-propagation)
  - [Finalization and Annotation Population](#finalization-and-annotation-population)
- [Resolved Design Decisions](#resolved-design-decisions)
- [Open Design Decisions](#open-design-decisions)

---

## Overview

The DRA (Dynamic Resource Allocation) device allocator is the component responsible for determining whether a set of ResourceClaims can be satisfied for a given NodeClaim. It simulates the upstream Kubernetes DRA scheduler's allocation logic, adapted for Karpenter's unique scheduling model where a NodeClaim represents a superposition of candidate instance types rather than a concrete node.

The allocator runs during Karpenter's scheduling loop. For each pod, the allocator receives all of the pod's ResourceClaims — whether already allocated or pending — determines which candidate instance types can satisfy them, accumulates topology requirements from all sources (already-allocated claims and newly allocated devices), and produces an opaque allocation handle that, when committed, reserves devices and records per-claim metadata for subsequent scheduling decisions within the same loop.

### Problem Statement

Karpenter must make provisioning decisions for pods that require DRA devices before a concrete node exists. This creates two challenges that the upstream scheduler does not face:

1. **Instance type superposition.** A NodeClaim is compatible with multiple instance types, each of which provides a different set of template devices. The allocator must evaluate each instance type independently and prune those that cannot satisfy the claims.

2. **Cross-NodeClaim device contention.** Multiple NodeClaims may compete for the same in-cluster devices. Since only one instance type will ultimately be provisioned per NodeClaim, the allocator must track device reservations at the correct granularity: globally across NodeClaims, but conditionally within a single NodeClaim's instance type candidates.

---

## Scope Exclusions

The following DRA features are out of scope for the initial implementation:
- Admin access (`adminAccess: true`)
- Partitionable devices (shared counters)
- Consumable capacity (`allowMultipleAllocations`)
- Device taints
- Non-node-local in-flight devices (cross-NodeClaim allocation)
- Multi-solution optimization (finding the least-constraining allocation)

---

## Device Sources

Devices come from two sources, prioritized in this order:

### In-Cluster Devices

Published to the Kubernetes API server as `ResourceSlice` objects by DRA drivers. These represent real, existing devices on nodes in the cluster. In-cluster devices are organized into **pools**, where each pool is identified by a `(driver, poolName)` pair and may span multiple ResourceSlice objects.

In-cluster devices may be **node-local** or **cluster-wide** (accessible from all nodes via `AllNodes: true`). Node-local devices come in two forms:
- **`NodeName`-pinned**: the slice sets `spec.nodeName` to a single node. These devices are accessible only from that exact node, so they can satisfy an existing node whose node name matches but can never satisfy an in-flight NodeClaim (which has no concrete node yet). They carry no label topology requirement — the node identity itself is the constraint.
- **`NodeSelector`-scoped**: the slice uses a label `NodeSelector`. These carry topology requirements that constrain which nodes can use them, and may match both existing nodes and in-flight NodeClaims whose requirements are compatible.

`ResourceSlice` objects owned by **uninitialized nodes** (nodes that have not yet reached the `Initialized` status condition) are excluded from pools. Until a node is initialized, its devices are represented by template devices instead, so its published slices are not counted as in-cluster devices (see [NodeClaim Abstraction](#nodeclaim-abstraction) and [Scheduler Initialization](#scheduler-initialization)).

### Template Devices

Provided by the cloud provider as `ResourceSliceTemplate` objects. These represent devices that *will exist* once an instance type is launched but are not yet published to the API server. Template devices are always node-local to the instance they will run on and carry no topology requirements (the instance type itself determines topology).

The allocator prefers in-cluster devices over template devices. This is a natural consequence of the DFS iteration order: in-cluster pool devices are iterated first, and the search commits to the first valid solution found. Preferring in-cluster devices minimizes variance across instance types, since in-cluster allocations are shared while template allocations diverge per instance type.

---

## ResourceClaim State Handling

All of a pod's ResourceClaims are passed to the allocator regardless of their current allocation state. The allocator classifies each claim and handles it internally before proceeding to the DFS.

Claims are processed sequentially. The allocator maintains **effective requirements** — the NodeClaim's base requirements progressively tightened by each already-allocated claim's topology. Each claim's compatibility is checked against these effective requirements, not just the original NodeClaim requirements. This ensures that mutually incompatible claims (e.g., one pinned to zone A and another to zone B) are detected immediately rather than producing a confusing downstream failure.

### Allocated (In-Cluster)

A claim that already has `status.allocation` set on the API server is fully allocated in the cluster. The allocator reads the topology requirements from `status.allocation.nodeSelector` and checks them for compatibility with the current effective requirements. If incompatible, the allocation fails immediately. If compatible, the requirements are merged into the effective requirements — tightening the baseline for subsequent claims and for pool gathering/filtering — and are included in the `AllocationResult`. No device reservation or DFS is needed for this claim; its devices are already committed.

### Allocated (In-Memory)

Multiple pending pods may reference the same ResourceClaim. When a claim that was not previously allocated is first allocated during the scheduling loop, the allocator records per-claim metadata: the associated NodeClaim ID, whether template devices were used, and the accumulated topology requirements. When a subsequent pod references that same claim, the allocator uses this metadata instead of re-running the DFS:

- **Template devices were used.** The claim is node-local to the original NodeClaim. If the current NodeClaim matches the original, the claim is already satisfied — it is skipped with no additional requirements. If the current NodeClaim is different, the allocation fails immediately; template-allocated claims cannot be satisfied from a different node.

- **In-cluster devices only.** The accumulated topology requirements are checked for compatibility with the current effective requirements. If incompatible, the allocation fails. If compatible, the requirements are merged into the effective requirements (same as in-cluster allocated claims) and included in the allocation result. The claim is skipped for DFS.

### Unallocated

A claim with no allocation — neither in-cluster nor in-memory — proceeds through normal validation and DFS. The DFS starts with effective requirements that include the NodeClaim's base requirements plus any topology constraints accumulated from already-allocated claims, so pool filtering and topology compatibility checks reflect the full set of constraints.

---

## Pool Management

### Pool Gathering

Pools are built from in-cluster `ResourceSlice` objects published to the API server. The provided slice set is pre-filtered by the caller to exclude slices owned by deleting/excluded nodes and by uninitialized nodes (see [Scheduler Initialization](#scheduler-initialization)); because *all* of an uninitialized node's slices are excluded, that node is wholly represented by template devices and never contributes a partial pool. The gathering process:

1. Groups slices by `(driver, poolName)`.
2. Tracks the highest generation per pool. When a newer generation is encountered, all previously accumulated slices for that pool are discarded.
3. Determines **completeness** by comparing the total slice count at the current generation against the pool's declared `ResourceSliceCount`. Completeness is a global property computed across all slices (matching and non-matching).
4. Filters slices by node affinity accessibility to the NodeClaim. A slice's devices are accessible when the slice is `AllNodes`, when it pins itself via `spec.nodeName` and that name equals the NodeClaim's node (existing nodes only — in-flight NodeClaims have no node name and never match a `NodeName`-pinned slice), or when its label `NodeSelector` is compatible with the NodeClaim's requirements. Only accessible slices contribute devices; inaccessible slices still participate in generation tracking and completeness checks.
5. Detects **invalid** pools with duplicate device names across slices.

For in-flight NodeClaims, a pool is included if it is compatible with *any* remaining candidate instance type.

```go
type Pool struct {
    Key        PoolKey  // {Driver DriverID, Pool PoolID}
    Slices     []ResourceSlice
    Devices    []DeviceWithID
    Incomplete bool
    Invalid    bool
}
```

Template devices are not tracked in pools — they are provided separately per instance type via the `NodeClaim.ResourceSlices()` interface and iterated after in-cluster pool devices during the DFS.

### Pool Filtering

When topology requirements are tightened during the DFS (a non-node-local device narrows the NodeClaim's requirements), the pool set is re-filtered against the new requirements. This is an incremental operation on the cached pool superset, not a full rebuild. Pools with no matching slices after filtering are dropped. The filtered state is snapshotted before each tightening so it can be restored on backtrack.

### Pool Caching

After a successful allocation is committed, the resulting pool set is cached by NodeClaim ID. The cache stores the pre-filter pool superset from the allocation. On subsequent allocations for the same NodeClaim, this superset is re-filtered against the NodeClaim's current (tightened) requirements, saving the cost of a full ResourceSlice scan while correctly reflecting new constraints added by the committed pod.

---

## Attribute Bindings

Attribute bindings model runtime-only attributes that share a value across devices on an instance, where the concrete value is not known at scheduling time. They are declared by cloud providers as part of instance type metadata.

**Example:** A ResourceClaim requires a GPU and NIC that share a PCI root complex. The PCI root ID is only known after the node launches, but the cloud provider declares that specific GPU-NIC pairs on a given instance type will share this attribute.

### Structure

Bindings are indexed by `(attribute, nodePool, instanceType)` and map each device to the set of devices it is bound with.

**Key invariant**: Attribute bindings are scoped to `(attribute, nodePool, instanceType)`. Bindings do not cross these boundaries — a zone binding in pool-a does not imply a zone binding in pool-b.

### Transitivity

Bindings are transitive. If device A is bound to device B and device B is bound to device C under the same `(attribute, nodePool, instanceType)` triple, then A is also bound to C. Transitivity is computed during construction via BFS closure over the direct binding graph.

### Construction

`BuildAttributeBindings` processes cloud provider instance type metadata:
1. For each declared binding group (a set of devices sharing an attribute), symmetric pairs are created between all devices in the group.
2. After all direct bindings are established, a BFS transitive closure is computed per `(attribute, nodePool, instanceType)` triple. The closure is computed from a snapshot of the original direct bindings to avoid contaminating mid-pass results.

Binding groups with fewer than 2 devices are ignored. Self-loops are removed.

### Attribute Lookup

When looking up an attribute on a device, the allocator first tries the fully qualified attribute name. If not found and the attribute name has a domain prefix matching the device's driver name, it retries with just the ID portion (driver-qualified fallback). This accommodates devices that store attributes without the driver domain prefix.

---

## Request Validation

Before the DFS begins, each ResourceClaim is validated and parsed into internal structures. Only `Exactly` requests are supported; `FirstAvailable` (sub-request) requests are rejected.

**Entry point**: `ValidateClaimRequest(ctx, kubeClient, claim, claimIndex, pools, templateDevicesByIT, celCache, bindingFallback) → (*ClaimData, error)`

**Output**:

```go
type ClaimData struct {
    ID          ResourceClaimID  // unique.Handle[string] from claim name
    Requests    []RequestData
    Constraints []Constraint
}

type RequestData struct {
    Name           string
    Class          *resourcev1.DeviceClass
    NumDevices     int                                    // For ExactCount mode
    AllocationMode resourcev1.DeviceAllocationMode       // ExactCount or All
    AllDevices     []DeviceWithID                         // Pre-computed eligible in-cluster devices (All mode)
    AllTemplateDevicesByIT map[InstanceTypeID][]DeviceWithID  // Pre-computed eligible templates per IT (All mode)
    Selectors      []resourcev1.DeviceSelector           // Combined from class + request
}
```

### Step 1: Constraint Parsing

Each entry in `claim.Spec.Devices.Constraints` is converted into an internal `Constraint` implementation:

| Constraint Kind | Supported | Internal Type |
|-----------------|-----------|---------------|
| `MatchAttribute` | Yes | `MatchAttributeConstraint` |
| All others | No | Error: *"unsupported constraint type"* |

`MatchAttributeConstraint` tracks whether all devices allocated for the constrained requests share a common attribute value. Constraints may be scoped to a subset of requests via the `Requests` field (stored as `RequestNames sets.Set[string]`). If empty, the constraint applies to all requests in the claim.

### Step 2: Request Validation

For each `Exactly` request, `validateExactRequest` performs:

**2a. DeviceClass Resolution**: The `DeviceClassName` is resolved via the API server. If the class does not exist, validation fails.

**2b. Selector Merging**: Selectors from the DeviceClass and the request are concatenated (class selectors first, then request selectors). All selectors must use CEL; any non-CEL selector is rejected.

**2c. CEL Compilation**: Every merged CEL expression is compiled via the shared `dracel.Cache`. Compilation failures are surfaced immediately rather than deferred to evaluation time.

**2d. Allocation Mode Handling**:

| Mode | `NumDevices` | `AllDevices` | `AllTemplateDevicesByIT` |
|------|-------------|-------------|--------------------------|
| `ExactCount` | `req.Count` | `nil` | `nil` |
| `All` | `0` | Pre-computed eligible in-cluster devices | Pre-computed eligible template devices per instance type |

**All mode pre-computation:**

1. **In-cluster devices** (`collectAllModePoolDevices`): Iterates all pools. If any pool is `Invalid` (duplicate device names) or `Incomplete` (missing slices), validation fails immediately — `All` mode requires a complete and consistent view of all pools. Devices in valid pools are filtered through the merged selectors (AND semantics).

2. **Template devices** (per instance type): Each instance type's template devices are filtered through the merged selectors. An instance type is **kept** if it has at least one matching template device OR there are in-cluster matches. An instance type is **pruned** only when both its template matches and in-cluster matches are empty.

### Step 3: Total Device Limit Enforcement

The Kubernetes API caps allocation results at `AllocationResultsMaxSize` (32) devices per claim. Validation enforces this in three stages:

**3a. Base Total Check**:
```
baseTotalDevices = sum(req.NumDevices) + sum(len(req.AllDevices))
```
If `baseTotalDevices > AllocationResultsMaxSize`, validation fails with *"exceeding maximum"*.

**3b. Per-Instance-Type Pruning**: For each instance type present in any `All` mode request's `AllTemplateDevicesByIT`:
```
templateCount = sum(len(req.AllTemplateDevicesByIT[itID])) across all requests
if baseTotalDevices + templateCount > AllocationResultsMaxSize:
    delete itID from all requests' AllTemplateDevicesByIT
```

**3c. Complete Pruning Detection**: If template devices were provided AND all instance types were pruned in step 3b, validation fails with *"all instance types pruned"*.

### Error Conditions Summary

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

---

## Constraints

Constraints are inter-device rules evaluated during the DFS. They are **stateful**: each `Add()` call modifies internal state (e.g., pinning a value), and each `Remove()` call reverses exactly one successful `Add()`. This stack-based design enables backtracking.

### MatchAttribute

The only constraint type currently supported. A `MatchAttribute` constraint requires that all devices allocated for the constrained requests share a common value for a specified attribute.

**Behavior:**
- The first device to satisfy the constraint **pins** the attribute value.
- Subsequent devices must have the same value for that attribute.
- On backtrack, when the pinning device is removed, the constraint resets to unpinned.

**Scoping:** A constraint may be scoped to specific request names within a claim, or apply to all requests if no names are specified.

**Evaluation paths:** A constraint uses one of two mutually exclusive evaluation paths, determined by the first device added:

1. **Concrete path.** The device has the attribute in its template. The attribute value is read directly and compared. Once established via concrete values, devices without the attribute are rejected.

2. **Binding fallback path.** The device lacks the attribute (it is runtime-only). The constraint consults the `AttributeBindings` graph to determine whether devices are bound under the attribute for the current `(nodePool, instanceType)`. Once established via bindings, devices with concrete attribute values are rejected.

This mutual exclusivity prevents mixing concrete comparison with binding-based comparison within the same constraint evaluation, which could produce inconsistent results. A single ResourceClaim may have multiple `MatchAttribute` constraints on different attributes — each is evaluated independently, and some may use direct comparison while others use bindings.

---

## Allocation Algorithm

The allocation algorithm determines whether a pod's ResourceClaims can be satisfied by a given NodeClaim. It operates in three phases: claim classification, per-instance-type evaluation via DFS, and result packaging. All prerequisite data — pools, attribute bindings, validated requests, and constraints — is prepared by the sections above before the algorithm runs.

### Claim Classification

Each ResourceClaim is classified as allocated (in-cluster), allocated (in-memory), or unallocated (see [ResourceClaim State Handling](#resourceclaim-state-handling)). Already-allocated claims progressively tighten the effective requirements (starting from the NodeClaim's base requirements). Each claim's topology is validated against the effective requirements at the time it is processed, so mutually incompatible claims are detected immediately. Only unallocated claims proceed to the DFS. Pool gathering and filtering use the final effective requirements, so already-allocated claims may eliminate pools before the DFS begins.

### Per-Instance-Type DFS

Each candidate instance type is evaluated independently via a full depth-first search. Instance types whose DFS fails are pruned from the candidate set.

Within a single instance type evaluation, the DFS iterates over three nested dimensions:

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

- **Claims** (outer): Each unallocated ResourceClaim in the pod's claim list.
- **Requests** (middle): Each device request within a claim.
- **Slots** (inner): Each device slot within a request (one slot per device to allocate).

For each slot, the algorithm tries candidate devices in iteration order: **in-cluster pool devices first, then template devices for the current instance type**. The first device that passes all checks is tentatively allocated and the search recurses to the next slot. If the subtree fails, the algorithm backtracks and tries the next candidate.

This ordering ensures in-cluster devices are preferred. In the common case, in-cluster devices satisfy requests without touching template devices. When in-cluster devices are insufficient, the DFS naturally falls through to template devices — all in a single traversal per instance type.

**Requirement accumulation across instance types:** Between instance type iterations, `allocatedDevices`, `pools`, and `snapshots` are reset, but `requirements` is **not** reset. Each instance type's contributed topology requirements tighten the baseline for subsequent instance types. This prevents disjoint requirement scenarios (e.g., "instance type A in zone-A OR instance type B in zone-B") that are not representable by a single NodeClaim. An instance type whose DFS would need requirements incompatible with the accumulated set from prior instance types is naturally pruned.

**Binding fallback lifecycle:** The attribute binding fallback is `nil` during initial claim validation (`ValidateClaimRequest`). Before each instance type's DFS, `setBindingFallback()` configures all `MatchAttributeConstraint` instances with the correct `(nodePool, instanceType)` context. Binding paths are not evaluated during validation — only during DFS.

### Device Eligibility Checks

For each candidate device, four checks are performed in order:

1. **Already allocated?** The device is rejected if it is already allocated globally (seed set), already allocated for a different NodeClaim (any instance type on that NodeClaim), or already allocated for the same NodeClaim on the same instance type (by a prior pod). A device allocated for the same NodeClaim on a *different* instance type is allowed, since only one instance type will actually be provisioned.

2. **Selector match?** The device must match all CEL selectors from both the `DeviceClass` and the request. Selectors use AND semantics. Match results are cached per `(device, claim, request)` tuple to avoid redundant CEL evaluation across backtrack iterations.

3. **Constraint satisfaction?** The device must satisfy all inter-device constraints on the claim (see [Constraints](#constraints)). Constraints are stateful; if the device fails a constraint, all previously applied constraints for this device are rolled back.

4. **Topology compatibility?** For non-node-local in-cluster devices with a `NodeSelector`, the device's implied topology requirements must be compatible with the NodeClaim's accumulated requirements. If compatible, the requirements are tightened and the pool set is re-filtered to reflect the narrower topology (see [Pool Filtering](#pool-filtering)). This state is snapshotted so it can be restored on backtrack.

   > **Snapshot-based tracking**: A single `requirements` field holds the current accumulated topology constraints. When a non-node-local device tightens requirements, a `backtrackSnapshot{requirements, pools}` is pushed and both fields are mutated. On backtrack, the snapshot is popped and both are restored. Since all devices in the gathered pools are guaranteed compatible with the base requirements (validated during gather), each new device's requirements need only be compatible with the *accumulated* requirements from devices allocated earlier in the DFS path.

### Allocation Modes

Each device request specifies an allocation mode:

- **ExactCount**: Allocate exactly N devices matching the request's selectors. Devices are drawn from the current pool set and template devices, tried in order until N are found.

- **All**: Allocate *every* matching device. The eligible device set is pre-computed during [Request Validation](#request-validation). Each slot maps to a specific predetermined device. Unlike ExactCount, there is no choice in which device fills each slot; the constraint is that every eligible device must pass allocation checks (not already allocated, constraints satisfied).

### Backtracking

When the DFS fails at any point, it unwinds in exact reverse order:

1. The device allocation record is removed.
2. If topology requirements were tightened, the snapshot is restored (requirements and filtered pool set).
3. Constraint state is rolled back via `Remove()` calls in reverse order.

This ensures the allocator can explore the full search space without leaving stale state.

### Timeout

The DFS is bounded by a 5-second context timeout. If the timeout fires during search, the current branch is abandoned. Context cancellation is checked periodically (e.g., between instance type iterations). This prevents pathological claim/device combinations from blocking the scheduling loop.

### Results

The `Allocate()` method returns an `(AllocationResult, error)` tuple. A non-nil error indicates the allocation failed entirely (e.g., all instance types pruned, invalid claims, or context cancellation).

1. **`InstanceTypes []InstanceTypeID`**: The set of instance types whose DFS succeeded. When there are no unallocated claims (all claims already satisfied), this returns the NodeClaim's full instance type set. For existing initialized nodes, this is the single known instance type. For in-flight NodeClaims with unallocated claims, this must be a non-empty subset of the NodeClaim's current candidate instance types on success.

2. **`Requirements scheduling.Requirements`**: The accumulated topology requirements from all sources — both already-allocated claims and newly allocated non-node-local devices.

3. **`Allocation`**: An opaque handle containing the allocation details needed to update allocator state on commit (device IDs, per-instance-type device sets, NodeClaim association, per-claim metadata).

---

## State Management

### Top-Level Allocator State

The `Allocator` struct is scoped to a single scheduling loop and is shared across all per-pod allocation requests. It is **read-only** during `Allocate()` calls and **mutated only** during initialization, `Commit()`, and `ReleaseInstanceType()`.

```go
type Allocator struct {
    // Encapsulates device allocation state: pre-allocated devices (immutable seed
    // set), inflight cluster allocations (by device and by NodeClaim/InstanceType),
    // and inflight template allocations.
    allocationTracker *AllocationTracker

    // The attribute binding adjacency graph. Built once at allocator construction
    // from the cloud provider's instance type metadata.
    attributeBindings AttributeBindings

    // Used for DeviceClass resolution during claim validation.
    kubeClient client.Client

    // The pre-filtered set of in-cluster ResourceSlices, provided at construction
    // time. The caller is responsible for excluding slices from deleting/excluded
    // nodes and from uninitialized nodes before passing them in. The allocator
    // treats this as the complete universe of in-cluster slices and does not query
    // cluster state directly.
    // Uses the ResourceSlice interface to abstract over API server slices.
    inClusterSlices []ResourceSlice

    // Cached pool sets per NodeClaim. Stores the pre-filter pool superset from the
    // most recent allocation. On subsequent allocations, this set is re-filtered
    // against the NodeClaim's current (tightened) requirements, avoiding a full
    // scan of inClusterSlices.
    poolCache map[NodeClaimID][]*Pool

    // Per-claim allocation metadata for in-memory claim reuse. Records the
    // NodeClaim ID, whether template devices were used, and accumulated topology
    // requirements for each claim allocated during the scheduling loop. Enables
    // subsequent pods referencing the same claim to skip re-running the DFS.
    // Keyed by claim name (interned as unique.Handle[string]).
    claimAllocationMetadata map[ResourceClaimID]*ResourceClaimAllocationMetadata
}
```

**AllocationTracker** encapsulates the device allocation state machine:
- `PreallocatedDevices sets.Set[DeviceID]` — immutable seed set of devices already allocated in the cluster.
- `InflightClusterAllocations map[DeviceID]*InflightAllocationMetadata` — tracks which NodeClaim/InstanceTypes have reserved each in-cluster device. A device allocated for a different NodeClaim is unavailable; a device allocated for the same NodeClaim on a different instance type is allowed (since the NodeClaim will collapse to a single IT).
- `InflightClusterAllocationsByNodeClaim map[NodeClaimID]map[InstanceTypeID]sets.Set[DeviceID]` — inverse index for efficient release when instance types are pruned.
- `InflightTemplateAllocations map[NodeClaimID]map[InstanceTypeID]sets.Set[DeviceID]` — template device allocations are NodeClaim+IT-local.

The `IsAllocated()` method implements the visibility rules described in [Device Eligibility Checks](#device-eligibility-checks) step 1. `Commit()` records device allocations from committed allocations. `ReleaseInstanceTypes()` frees devices when ITs are pruned.

**Initialization**:
- `allocationTracker`: Constructed via `NewAllocationTracker(allocatedDevices...)` with the pre-allocated device IDs sourced from the `deviceallocation` controller. Filtered to exclude devices allocated exclusively by **deleting pods** — this includes all pods on deleting nodes and disruption candidates, as well as pods that are individually deleting (observed `deletionTimestamp`). Devices allocated exclusively by such pods are excluded, making them available for reallocation.
- `attributeBindings`: Call `BuildAttributeBindings(instanceTypesByNodePool)` with the cloud provider's instance type data, grouped by NodePool.
- `kubeClient`: The controller-runtime client, used to resolve `DeviceClass` references during claim validation.
- `inClusterSlices`: Provided by the caller at construction time. See [Scheduler Initialization](#scheduler-initialization).
- `poolCache`: Starts empty. Populated via `Commit()`.
- `claimAllocationMetadata`: Starts empty. Populated via `Commit()` with per-claim metadata.

**Thread safety**: The scheduler evaluates a pod against multiple NodeClaims in parallel. All parallel `Allocate()` calls are **read-only** with respect to top-level state. Mutation of `allocationTracker`, `claimAllocationMetadata`, and `poolCache` occurs only in `Commit()` and `ReleaseInstanceType()`, which are called sequentially after a placement decision is finalized.

### Per-Request Allocator State

The child `allocator` struct is created per `Allocate()` call. It embeds a pointer to the top-level `Allocator` for read access and holds mutable state for the current DFS.

```go
type allocator struct {
    *Allocator  // read-only access to shared state

    ctx      context.Context
    // Created per-Allocate() call to avoid write-contention on the top-level allocator.
    celCache *dracel.Cache

    // The NodeClaim being evaluated.
    nodeClaim NodeClaim
    // The current instance type being evaluated in the DFS.
    itID InstanceTypeID
    // Template devices indexed by instance type, built from NodeClaim.ResourceSlices().
    templateDevicesByIT map[InstanceTypeID][]DeviceWithID
    // Validated claim data: contains requests (with selectors, class, mode, predetermined
    // devices) and constraints. Replaces raw claims, separate constraint/request maps.
    claimData []*ClaimData
    // Cache: does device X match request Y's selectors?
    deviceMatchesRequest map[matchKey]bool

    // Devices allocated in the current DFS path (quick lookup set).
    allocatedDevices sets.Set[DeviceID]
    // Ordered metadata for allocated devices (claim index, device details).
    // Used for result construction and topology requirement accumulation.
    allocatedDevicesMetadata []deviceAllocationMetadata

    // The accumulated topology requirements, progressively tightened as non-node-local
    // devices are allocated. Restored from snapshots on backtrack.
    requirements scheduling.Requirements
    // The current filtered pool set. Narrowed when requirements tighten; restored on backtrack.
    pools []*Pool
    // Stack of {requirements, pools} pairs pushed on requirement tightening, popped on backtrack.
    snapshots []backtrackSnapshot
}
```

The `ClaimData` type produced by validation contains both `Requests []RequestData` and `Constraints []Constraint`, replacing the separate `constraints` and `requestData` maps from the original design. Results are constructed in the `allocate()` method's return path rather than accumulated in a `result` field.

---

## Commit Protocol

Allocation results are not applied to the allocator's shared state until explicitly committed. This two-phase approach allows the caller to inspect the result, decide whether to proceed, and only then modify shared state.

### Commit Behavior

When `Commit()` is called on an allocation result:

`Allocation` is an interface returned in `AllocationResult`. The scheduler calls `Commit()` directly on the allocation handle:

```go
type Allocation interface {
    Commit(context.Context)
}
```

The scheduler invokes `allocationResult.Allocation.Commit(ctx)` after deciding to proceed with placement. Internally, the `Commit()` implementation updates the top-level allocator:

1. **Device reservation.** All device IDs from the allocation are recorded in the `AllocationTracker`, indexed by `(nodeClaimID, instanceTypeID)`. This makes them visible to `IsAllocated()` checks in subsequent allocations.

2. **Pool cache update.** The pool set used during allocation is cached for the NodeClaim, enabling faster pool resolution in subsequent allocations. The cache stores the pre-filter pool superset — the next allocation will re-filter against the NodeClaim's newly tightened requirements.

3. **Per-claim allocation metadata.** For each newly allocated claim, the allocator records: the associated NodeClaim ID, whether template devices were used, and the accumulated topology requirements. This metadata enables in-memory allocated claim handling when subsequent pods reference the same ResourceClaim (see [ResourceClaim State Handling](#resourceclaim-state-handling)).

### Instance Type Release

When the scheduler prunes an instance type from a NodeClaim's candidate set, `ReleaseInstanceType` removes all device allocations for that instance type on that NodeClaim. Once all instance types referencing a device are released, the device becomes available to other NodeClaims.

---

## Integration with Karpenter's Scheduler

### Scheduler Initialization

**File**: `pkg/controllers/provisioning/scheduling/scheduler.go`

At scheduler construction (`NewScheduler`), build the DRA allocator:

1. **Collect and filter in-cluster ResourceSlices**: Gather all `ResourceSlice` objects from the cluster. Filter out slices owned by nodes that are not in the stateNode set passed to the scheduler (i.e., deleting nodes, disruption candidates). Also filter out slices owned by **uninitialized nodes** (nodes that have not reached the `Initialized` status condition) — an uninitialized node's devices are represented by template devices, so including its published slices would double-count them. Non-node-owned slices (no node owner reference) are always included. This filtering uses `metadata.ownerReferences` to determine node ownership — `spec.nodeName` indicates accessibility, not ownership.
2. Obtain the set of allocated devices from the `deviceallocation` controller's tracking state, filtered for deleting pods. The `deviceallocation.Controller` is injected directly into the provisioner (not accessed through an interface). Devices with no consumers (unowned) are treated as releasable. Deleting pods should include all pods on deleting nodes and disruption candidates.
3. Build `AttributeBindings` from instance type metadata grouped by NodePool.
4. Construct the `Allocator` via `NewAllocator(inClusterSlices, allocatedDevices, attributeBindings, kubeClient)`. The `allocationTracker`, `poolCache`, and `claimAllocationMetadata` start empty and are populated via `Commit()` during the scheduling loop.

The allocator is stored on the `Scheduler` struct and passed through to NodeClaim evaluation.

### NodeClaim Abstraction

The allocator operates on a `NodeClaim` interface that abstracts over three lifecycle phases:

- **Existing initialized nodes.** Have a single known instance type. `ResourceSlices()` returns empty (all devices are already published in-cluster).
- **Pre-initialized nodes.** Have a single known instance type but have not yet reached the `Initialized` status condition. Template devices are the source of truth until initialization completes: `ResourceSlices()` returns the instance type's **full** template set under that instance type, and the node's published in-cluster slices are excluded from pool gathering.
- **In-flight NodeClaims.** Have multiple candidate instance types. `ResourceSlices()` returns templates for all candidates.

This abstraction allows the allocator to use identical logic regardless of whether it is evaluating an existing node, a node being set up, or a node that does not yet exist.

### Existing Node Evaluation

**Integration point**: `scheduler.addToExistingNode()` → `NodeClaim.CanAdd()`

For existing (initialized) nodes:
1. The NodeClaim wraps a real node with a known instance type.
2. `NodeClaim.ResourceSlices()` returns an empty map — all published slices are already in the allocator's in-cluster pool set.
3. Call `Allocator.Allocate(ctx, nodeClaim, pod.ResourceClaims)`.
4. If allocation succeeds, the pod can be placed. The `AllocationResult.Allocation` is held until `Add()` is called. **Note**: `AllocationResult.Requirements` are **not** merged into the existing node's requirements — existing node requirements are immutable (already set in stone). The allocator validates compatibility internally; if claims could not be satisfied with the node's requirements, the allocation would have failed.
5. On `Add()`, call `AllocationResult.Allocation.Commit(ctx)` to mark the devices as consumed.

For pre-initialized nodes (existing but not yet at the `Initialized` status condition):
1. The NodeClaim wraps a real node with a known instance type that has not yet reached the `Initialized` status condition.
2. Template devices are the source of truth until the node is initialized. `NodeClaim.ResourceSlices()` returns the instance type's **full** in-memory template set under the single known instance type.
3. The node's published in-cluster `ResourceSlice`s are excluded from pool gathering while it is uninitialized (see [Scheduler Initialization](#scheduler-initialization)), so devices are not double-counted across templates and published slices. Once the node becomes initialized, its published slices become authoritative and `ResourceSlices()` returns empty — the existing initialized-node behavior.

### In-Flight NodeClaim Evaluation

**Integration point**: `scheduler.addToInflightNode()` → `NodeClaim.CanAdd()`

For in-flight NodeClaims (created earlier in this scheduling loop):
1. The NodeClaim has multiple candidate instance types, each with its own ResourceSlice set.
2. `NodeClaim.ResourceSlices()` returns a map from `InstanceTypeID` to the slices that instance type would publish.
3. Call `Allocator.Allocate(ctx, nodeClaim, pod.ResourceClaims)`.
4. The allocator runs the per-instance-type DFS: for each candidate instance type, a single DFS traversal evaluates both in-cluster and template devices.
5. `AllocationResult.InstanceTypes` is the subset of instance types that support the allocation. The scheduler intersects this with the NodeClaim's current instance type set — if the intersection is empty, placement fails.
6. `AllocationResult.Requirements` contains topology constraints from all sources (already-allocated claims and newly allocated devices). The scheduler merges these into the NodeClaim's requirements.
7. On `Add()`, commit the allocation.

### New NodeClaim Evaluation

**Integration point**: `scheduler.addToNewNodeClaim()` → `NodeClaim.CanAdd()`

Identical to in-flight evaluation. The only difference is the starting state: the NodeClaim begins with the full set of instance types from the NodePool template, unconstrained by prior pod placements.

### Constraint Propagation

When a pod with DRA requirements is added to a NodeClaim, the NodeClaim must be constrained in two ways, applied in order:

1. **Requirement tightening**: `AllocationResult.Requirements` adds topology constraints (e.g., `topology.kubernetes.io/zone: us-west-2a`). These are merged into the NodeClaim's requirements via `Requirements.Add()`, which performs intersection-based narrowing.

2. **Instance type re-evaluation**: After requirements are tightened, the full instance type filtering pipeline must be re-run — not just DRA-specific pruning. Tightened requirements may eliminate offerings for instance types that were previously compatible (e.g., an instance type that was available in us-west-2a and us-west-2b is now only viable if it has an us-west-2a offering). This is consistent with how Karpenter handles topology constraint tightening today.

   Additionally, `AllocationResult.InstanceTypes` provides a DRA-specific filter: only instance types whose DFS succeeded are retained. This is applied alongside the existing compatibility, fit, and offering filters in `filterInstanceTypesByRequirements()`.

Both of these happen inside `NodeClaim.Add()`, after the existing resource and topology constraint updates.

### Finalization and Annotation Population

**Integration point**: `scheduler.FinalizeScheduling()` → `NodeClaim.FinalizeScheduling()`

When the scheduling loop completes and NodeClaims are finalized:

1. Collect the set of DRA driver names from all ResourceClaims of pods scheduled to the NodeClaim.
2. Set the `karpenter.sh/dra-drivers` annotation on the NodeClaim (per the lifecycle doc).
3. The finalized NodeClaim carries both the standard resource requests and the DRA driver annotation for the initialization controller to gate on.
