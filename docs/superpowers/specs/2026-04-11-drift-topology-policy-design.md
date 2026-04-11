# Drift Topology Policy Design

**Date:** 2026-04-11
**Status:** Approved

## Problem

Karpenter's drift controller disrupts nodes in globally oldest-first order, with no awareness of topology domains (e.g. availability zones). In clusters with zone-sensitive workloads, this can cause simultaneous disruptions across all zones, reducing availability during rolling node replacements.

The goal is to support three disruption behaviors, selectable per NodePool:

- **(a) Default** — globally oldest-first (existing behavior, no config required)
- **(b) Sequential by domain** — disrupt one topology domain at a time, one node at a time within that domain
- **(c) Sequential by domain with concurrency** — disrupt one topology domain at a time, up to N nodes concurrently within that domain

## API

Two new types added to `pkg/apis/v1/nodepool.go`:

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

`DriftPolicy *DriftPolicy` is added to `Disruption` as an optional field with `hash:"ignore"`.

### Example configurations

```yaml
# Mode (a): default — omit driftPolicy entirely

# Mode (b): one zone at a time, one node at a time
disruption:
  driftPolicy:
    topologyKey: topology.kubernetes.io/zone

# Mode (c): one zone at a time, up to 3 nodes concurrently
disruption:
  driftPolicy:
    topologyKey: topology.kubernetes.io/zone
    maxConcurrentPerDomain: "3"
```

### Interaction with flat Budget

`DriftPolicy` is an orthogonal constraint layered on top of the existing `Budget` system. Both apply independently:

- `Budget.Nodes` caps total in-flight nodes across the NodePool (all disruption reasons)
- `DriftPolicy.MaxConcurrentPerDomain` caps in-flight nodes within the active topology domain (Drift only)

The more restrictive constraint governs at each step.

## Algorithm

### Entry point

`applyDriftPolicies` is called at the top of `Drift.ComputeCommands`, before the empty/non-empty candidate split:

```
sort candidates by drift age (oldest first)
↓
applyDriftPolicies(candidates)         ← NEW
↓
split empty/non-empty (unchanged)
↓
iterate empty-first, check flat budget, simulate scheduling (unchanged)
```

### `applyDriftPolicies` steps

**Step 1 — Early exit**

If no candidate's NodePool has a `DriftPolicy.TopologyKey` set, return candidates unchanged. No cluster state read occurs in the common (no-policy) case.

**Step 2 — Build domain index**

Read all managed cluster nodes once to build:

```
index: (nodePoolName, domain) → { total int, inFlight int }
```

- `total` = all managed nodes with the topology label in that domain
- `inFlight` = nodes with `MarkedForDeletion` set

Only `Managed()` is required (not `Initialized()`) to avoid undercounting nodes that are being disrupted before initialization completes.

**Step 3 — Pre-compute per-NodePool active domain**

For each NodePool with a `DriftPolicy`, determine:

**Active domain selection:**
```
inFlightDomains = sorted(domains where inFlight > 0)
if len(inFlightDomains) > 0:
    activeDomain = inFlightDomains[0]   // alphabetically first in-flight domain
else:
    candidateDomains = sorted(unique labeled domains across drifted candidates)
    activeDomain = candidateDomains[0]  // alphabetically first domain with work to do
```

Alphabetical ordering is used throughout for determinism. The active domain does not change based on drift age or node count, so it remains stable as nodes are replaced mid-roll.

**Budget gate:**

Compute `maxAllowed` from `MaxConcurrentPerDomain` using the domain's total node count as the percentage denominator (same rounding as `Budget.Nodes`).

If `inFlightInDomain >= maxAllowed`, mark this NodePool as budget-exhausted. The controller will retry on the next reconcile cycle.

**Step 4 — Filter sorted candidates**

Filter the already-sorted candidate slice, preserving order:

```
for each candidate (in drift-age order):
    if NodePool has no DriftPolicy         → keep
    if NodePool budget exhausted           → drop (gate)
    if candidate has no topology label     → keep (fall-through to flat budget)
    if candidate.domain == activeDomain    → keep
    else                                   → drop
```

No re-sorting after filtering — elements are only removed, never reordered.

### Why this preserves existing invariants

- **Empty-first priority** — `applyDriftPolicies` only removes candidates, never reorders or truncates by index. The empty/non-empty split runs on the filtered slice, so empty nodes always get priority within the active domain.
- **Flat budget unchanged** — `Budget.Nodes` and `disruptionBudgetMapping` are not modified. `DriftPolicy` adds a gate before the existing budget check, not a replacement for it.
- **Deterministic output** — the map over NodePools is used only for pre-computation (building lookup maps); the final filter runs on the already-sorted input slice, so output order is deterministic regardless of map iteration order.
- **No `MaxInt32` hack** — `GetAllowedDisruptionsByReason` is not modified. Topology budgets are enforced entirely within the Drift method.

### Unlabeled nodes

Nodes missing the configured `topologyKey` label bypass the domain filter and fall through to the flat NodePool budget. They are treated as if no `DriftPolicy` is set for them. This is the safest behavior: unlabeled nodes are disrupted normally rather than silently blocked.

## Validation

Added to `pkg/apis/v1/nodepool_validation.go`:

- `TopologyKey` must be a valid qualified Kubernetes label name (via `k8s.io/apimachinery/pkg/util/validation`)
- `MaxConcurrentPerDomain` must match `^((100|[0-9]{1,2})%|[0-9]+)$` if set (same as `Budget.Nodes`)

No CEL rule is needed to require `topologyKey` when `maxConcurrentPerDomain` is set: since `TopologyKey` carries `+kubebuilder:validation:Required` within the `DriftPolicy` struct, it is always required whenever `driftPolicy` is present in the spec.

**Code convention:** All fields on `DriftPolicy` must carry `//nolint:kubeapilinter` before their doc comment, consistent with every other field in `nodepool.go`.

## Files changed

| File | Change |
|------|--------|
| `pkg/apis/v1/nodepool.go` | Add `DriftPolicy` struct; add `DriftPolicy *DriftPolicy` to `Disruption` |
| `pkg/apis/v1/nodepool_validation.go` | Add `validateDriftPolicy()` |
| `pkg/apis/v1/nodepool_budgets_test.go` | Validation tests for `DriftPolicy` |
| `pkg/controllers/disruption/drift.go` | Add `applyDriftPolicies`, `activeDomain`, domain index helpers |
| `pkg/controllers/disruption/drift_test.go` | Unit tests (see below) |
| `pkg/apis/crds/karpenter.sh_nodepools.yaml` | Regenerated |
| `kwok/charts/crds/karpenter.sh_nodepools.yaml` | Regenerated |

## Test cases

| Test | Assertion |
|------|-----------|
| No `DriftPolicy` — existing behavior | Globally oldest node disrupted |
| Mode (b): alphabetically first domain chosen | zone-a disrupted before zone-b |
| Mode (b): stays in active domain while in-flight nodes exist | Second reconcile targets same domain as first |
| Mode (b): advances to next domain when first is complete | zone-b becomes active after zone-a finishes |
| Mode (c): per-domain concurrency cap enforced | `maxConcurrentPerDomain: "2"`, 2 in-flight → no new disruption |
| Mode (c): per-domain concurrency allows parallel within domain | `maxConcurrentPerDomain: "2"`, 1 in-flight → disruption issued |
| Unlabeled nodes fall through | Unlabeled candidate eligible when active domain is zone-a |
| Both gates: per-domain exhausted + flat budget `"0"` | No disruption |
| Percentage `maxConcurrentPerDomain` | `"50%"` of 4 nodes = 2 allowed; 2 in-flight → blocked |
| Empty nodes prioritized within active domain | Empty node disrupted before non-empty in same domain |

All tests use explicit drift timestamps (e.g. `-2h/-1h/-30m`) to ensure deterministic zone selection.
