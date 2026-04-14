# Drift Topology Policy

## Problem

Karpenter's drift controller selects candidates in globally oldest-first order, with no awareness of topology domains such as availability zones. When a fleet-wide drift event occurs — for example, a new AMI becoming available across all nodes — Karpenter will simultaneously replace nodes across all topology domains.

For stateful distributed systems that replicate data across availability zones, simultaneous multi-zone disruption creates two compounding risks.

**Quorum loss.** Consider a Kafka cluster with replication factor 3, spread evenly across 3 availability zones (az-a, az-b, az-c), with one replica per zone per partition. If Karpenter simultaneously replaces a node in az-a and a node in az-b, the replicas for any partition spanning those zones are both temporarily unavailable. With only one of three replicas healthy, the partition loses quorum and becomes unavailable to producers and consumers. Cassandra faces the same risk: with a replication factor of 3 and one replica per zone, two simultaneous cross-zone replacements reduce read and write quorum to a single node.

**Rebalance and compaction storms.** Every Kafka broker replacement triggers a rebalance: consumer groups pause processing, partition leaders are redistributed, and the cluster absorbs a surge of CPU and network I/O. When replacements occur across multiple zones simultaneously, these rebalances stack — a pattern known as a *rebalance storm* — which can cascade: consumer lag grows, downstream microservices starve, those services restart, triggering further rebalances. Cassandra faces an analogous problem: each node add/remove event triggers a compaction wave as SSTables are consolidated on affected nodes. Concurrent cross-zone replacements produce multiple simultaneous compaction storms, saturating CPU and disk across the cluster.

### Why existing controls are insufficient

`Budget.Nodes` limits the total number of nodes simultaneously in-flight across a NodePool, but it has no awareness of which topology domain those nodes belong to. Setting `Budget.Nodes: "1"` prevents simultaneous cross-zone disruptions and is the current workaround for operators of zone-sensitive stateful workloads. However, this forces the entire fleet into single-file global replacement — one node at a time, globally — regardless of whether parallelism within a zone would be safe. For large fleets, a full drift cycle with `Budget.Nodes: "1"` can take approximately 48 hours, significantly slowing release cadence.

There is no existing mechanism in Karpenter to express: *replace nodes zone by zone, with controlled concurrency within each zone.*

## User Stories

**Kafka operator on a large fleet:**
> As an operator running Kafka with cross-zone replication, I want drift to replace nodes one availability zone at a time, so that partition replicas in at least 2 out of 3 zones are always healthy and consumer groups are never simultaneously disrupted across zones.

**Cassandra operator:**
> As an operator running Cassandra, I want drift to process one availability zone at a time so that token range streaming and compaction complete in one zone before starting the next, preventing simultaneous compaction storms from saturating cluster resources.

**Operator of a large fleet:**
> As an operator with hundreds of nodes per zone, I want to allow concurrent node replacements within the active zone while still processing zones sequentially, so that a full fleet drift cycle completes in hours rather than days.

### Behavior overview

**Without DriftPolicy** — Karpenter replaces the two oldest drifted nodes regardless of zone. If those nodes happen to be in az-a and az-b, both rebalance/compaction events fire simultaneously and partition replicas in two zones are degraded at the same time:

```
az-a: [ ⚙ replacing ] [ stable ]    rebalance storm ──┐
az-b: [ ⚙ replacing ] [ stable ]    rebalance storm ──┤─→ cascading failures
az-c: [   stable    ] [ stable ]                       │
                                                       ↓
                              Kafka partition P1: 1/3 replicas healthy → quorum at risk
```

Workaround `Budget.Nodes: "1"` is safe but topology-blind — one node globally at a time, ~48h for full fleet.

**With DriftPolicy** — Karpenter processes az-a completely before advancing to az-b. Multiple nodes within az-a can be replaced concurrently; az-b and az-c remain untouched until az-a is done:

```
az-a: [ ⚙ replacing ] [ ⚙ replacing ]   one rebalance wave ── completes ──┐
az-b: [   stable    ] [   stable    ]                                       ├→ sequential, isolated
az-c: [   stable    ] [   stable    ]                                       │
                                                                            ↓
                              Kafka partition P1: 2/3 replicas healthy → quorum maintained
```

## Proposed API

A new `DriftPolicy` struct added as an optional field on `Disruption`:

```go
// DriftPolicy configures topology-aware disruption ordering for drifted nodes.
type DriftPolicy struct {
    // TopologyKey is the node label used to group nodes into topology domains.
    // Karpenter will disrupt one domain at a time, in alphabetical order of domain name.
    // +kubebuilder:validation:Required
    TopologyKey string `json:"topologyKey"`

    // MaxConcurrentPerDomain is the maximum number of nodes that can be
    // simultaneously in-flight within the active topology domain.
    // Supports absolute values ("2") and percentages ("50%").
    // Defaults to "1" (fully sequential within a domain).
    // +kubebuilder:validation:Pattern:="^((100|[0-9]{1,2})%|[0-9]+)$"
    // +kubebuilder:default:="1"
    // +optional
    MaxConcurrentPerDomain string `json:"maxConcurrentPerDomain,omitempty"`
}
```

### Example configurations

```yaml
# Default: omit driftPolicy entirely — globally oldest-first, existing behavior unchanged

# One zone at a time, one node at a time within the zone
disruption:
  driftPolicy:
    topologyKey: topology.kubernetes.io/zone

# One zone at a time, up to 3 nodes concurrently within the active zone
disruption:
  driftPolicy:
    topologyKey: topology.kubernetes.io/zone
    maxConcurrentPerDomain: "3"

# One zone at a time, up to 50% of that zone's nodes concurrently
disruption:
  driftPolicy:
    topologyKey: topology.kubernetes.io/zone
    maxConcurrentPerDomain: "50%"
```

### Interaction with Budget

`DriftPolicy` and `Budget.Nodes` are orthogonal constraints that apply independently. Both must be satisfied for a disruption to proceed:

- `Budget.Nodes` caps total in-flight nodes across the NodePool (all disruption reasons)
- `DriftPolicy.MaxConcurrentPerDomain` caps in-flight nodes within the active topology domain (drift only)

The more restrictive constraint governs at each step.

## 🔑 Design Choices

### Why not extend `Budget` to support topology?

`Budget` is reason-agnostic — it constrains total in-flight disruptions across consolidation, expiry, and drift simultaneously. Topology-aware ordering is meaningful specifically for drift, where the trigger is a fleet-wide state change (such as a new AMI) that causes many nodes to drift at once, creating a rolling-wave replacement pattern. Consolidation and expiry are reactive to real-time cluster state and do not produce the same coordinated multi-node disruption risk.

Adding topology awareness to `Budget` would either apply it incorrectly to all disruption reasons, or require reason-specific fields inside `Budget` — a significantly larger and more invasive API change. Scoping the feature to a `DriftPolicy` field keeps the change minimal and the semantics unambiguous.

### Why not a separate CRD or external controller?

Karpenter's candidate selection and in-flight tracking are internal controller state with no stable external API surface. An external controller could only influence behavior indirectly (e.g. by manipulating node labels), which would be fragile and version-coupled to Karpenter internals. Placing `DriftPolicy` on `NodePool` keeps the contract clean: the NodePool spec fully describes how its nodes are managed, and the drift controller enforces the policy in a single, testable code path.

### Why alphabetical domain selection?

Alphabetical ordering is deterministic and stable across controller restarts. If Karpenter restarts mid-roll, alphabetical selection ensures it resumes the same active domain rather than jumping to a new one, which could leave a partially-replaced zone in a degraded state for longer.

Alternative approaches were considered:

- **Round-robin:** requires durable state that survives restarts.
- **Load-based (fewest in-flight first):** unstable under the rebalance/compaction load we are specifically trying to isolate; could cause thrashing.
- **Most-nodes-first:** optimizes throughput but not stability; no correctness advantage.

Alphabetical ordering is also easy for operators to reason about: given a list of zone names, they can predict exactly which zone will be active first.

### Why drift-only scope?

`DriftPolicy` is explicitly scoped to drift so that its semantics are unambiguous and it does not interact with consolidation or expiry budgets. The field name signals this scope. If a similar need emerges for other disruption reasons, it can be addressed with a separately-designed policy field.

## Algorithm

### Entry point

`applyDriftPolicies` is called at the top of `Drift.ComputeCommands`, before the empty/non-empty candidate split:

```
sort candidates by drift age (oldest first)
↓
applyDriftPolicies(candidates)         ← new
↓
split empty/non-empty (unchanged)
↓
iterate empty-first, check flat budget, simulate scheduling (unchanged)
```

### Steps

**Step 1 — Early exit.** If no candidate's NodePool has `DriftPolicy.TopologyKey` set, return candidates unchanged. No cluster state read occurs in the common (no-policy) case.

**Step 2 — Build domain index.** Read all managed cluster nodes once to build:

```
index: (nodePoolName, domain) → { total int, inFlight int }
```

`inFlight` = nodes with `MarkedForDeletion` set. Only `Managed()` nodes are counted (not `Initialized()`) to avoid undercounting nodes being disrupted before initialization completes.

**Step 3 — Determine active domain per NodePool.**

```
if any domain has inFlight > 0:
    activeDomain = alphabetically first domain with inFlight > 0
else:
    activeDomain = alphabetically first domain with drifted candidates
```

Compute `maxAllowed` from `MaxConcurrentPerDomain` using the domain's total node count as the percentage denominator (same rounding as `Budget.Nodes`). If `inFlight >= maxAllowed`, mark the NodePool as budget-exhausted; the controller retries on the next reconcile.

**Step 4 — Filter candidates.**

```
for each candidate (in drift-age order):
    if NodePool has no DriftPolicy         → keep
    if NodePool budget exhausted           → drop
    if candidate has no topology label     → keep (fall-through to flat budget)
    if candidate.domain == activeDomain    → keep
    else                                   → drop
```

No re-sorting — elements are only removed, never reordered, preserving empty-node priority and drift-age ordering.

### Preserved invariants

- **Empty-first priority** — `applyDriftPolicies` only removes candidates; the empty/non-empty split runs on the filtered slice unchanged.
- **Flat budget unchanged** — `Budget.Nodes` and `disruptionBudgetMapping` are not modified.
- **Deterministic output** — the map over NodePools is used only for pre-computation; the final filter runs on the already-sorted input slice.

## Unlabeled Nodes

Nodes missing the configured `topologyKey` label bypass the domain filter and fall through to the flat NodePool budget. They are disrupted normally, as if no `DriftPolicy` is set. This is the safest behavior: unlabeled nodes are not silently blocked, and operators can address missing labels without changing the NodePool spec.

## Validation

- `TopologyKey` must be a valid qualified Kubernetes label name (via `k8s.io/apimachinery/pkg/util/validation`)
- `MaxConcurrentPerDomain` must match `^((100|[0-9]{1,2})%|[0-9]+)$` if set (same pattern as `Budget.Nodes`)

`TopologyKey` carries `+kubebuilder:validation:Required` within the `DriftPolicy` struct, so it is always required when `driftPolicy` is present — no additional CEL rule needed.
