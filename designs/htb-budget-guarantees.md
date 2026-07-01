# HTB Based Disruption Budget Model

## Context

Disruption budgets today do not mean what users think they mean. The configuration syntax suggests independent, per reason control, but the runtime behavior is a single shared counter that makes per reason budgets decorative.

### Budgets do not read the way they behave

Disruption budgets are configured **per NodePool**, but within each NodePool, the disrupting counter is **shared across all reasons**. `BuildDisruptionBudgetMapping` counts ALL `MarkedForDeletion()` nodes as disrupting regardless of why they were marked. If Drift consumes 10 of 15 allowed disruptions, Consolidation sees only 5 remaining even if its configured budget is 5%. Drift's consumption directly reduces Consolidation's effective budget.

This creates two problems for users trying to reason about their budgets:

1. **Per reason budgets are not independent.** A user writing `{nodes: "10%", reasons: [Drifted]}` and `{nodes: "5%", reasons: [Underutilized]}` reads this as two separate pools. In reality, they share a single disrupting counter. The per reason budget only filters which `allowedDisruptions` value applies. The deduction is always global. The 5% consolidation budget is meaningless when drift is consuming 10%.

2. **Budgets do not add up to a whole.** "10% for drift" and "5% for consolidation" looks like it partitions 15% of disruption capacity across two reasons. Instead, both budgets draw from the same counter. The catch all budget (no reasons specified) acts as the true cap, but its relationship to the per reason budgets is not obvious. The YAML looks like a partition but behaves like a shared pool with soft hints.

### What HTB gives us

The Hierarchical Token Bucket model makes the YAML mean what it says:

- **The catch all budget becomes the parent**: it defines the total disruption capacity for the NodePool.
- **Per reason budgets become children**: each owns its slice of disruption capacity independently.
- **Budgets add up the way users expect**: "10% for drift" plus "5% for consolidation" totals 15%. Any difference between that sum and the parent is a shared excess pool available to any reason.
- **The hierarchy can extend to multiple scopes**: per reason within a NodePool, and per NodePool within a cluster-wide cap.

The result is a budget model that is more powerful but also easier to understand: what you write is what you get.

## HTB Background

HTB is a Linux traffic control algorithm (`tc-htb`) used for bandwidth management. Each class in the hierarchy has:

- **`rate`**: guaranteed bandwidth. This class always gets at least this much, regardless of what siblings are doing.
- **`ceil`**: maximum bandwidth including borrowed capacity. A class can burst above its rate by borrowing unused bandwidth from siblings, up to this ceiling.
- **parent**: enforces the global cap across all children.

When a class is not using its full `rate`, the unused portion flows up to the parent and becomes available to sibling classes. A class consuming above its `rate` is borrowing from the shared surplus. The parent's `rate` is the hard ceiling for all children combined.

HTB is **work conserving up to each class's `ceil`**. No capacity sits idle when there is demand for it. If a class has no traffic, its entire `rate` is available to siblings that want to borrow. However, a borrowing class will never exceed its own `ceil`, even if more surplus is available. This means capacity is fully utilized across classes that have work to do, while each class's maximum consumption is still bounded.

## Mapping HTB to Disruption Budgets

| HTB concept | Disruption budget equivalent |
|------------|------------------------------|
| Class | Disruption reason (Drifted, Underutilized) |
| `rate` | `reserved[reason]`: guaranteed minimum disruptions for this reason |
| `ceil` | `global_cap`: maximum disruptions a reason can reach by borrowing |
| Parent `rate` | `global_cap`: total allowed disruptions across all reasons |
| Surplus from idle sibling | Excess pool: unused reservations available to other reasons |

## Proposed Model

Each reason becomes an HTB class with its own `rate` (the budget the user configured) and a `ceil` equal to the global cap. When a reason is not using its full budget, the unused portion is available to other reasons. This means budgets read as independent slices but unused capacity is not wasted.

### Definitions

```
global_cap (parent rate) = total allowed disruptions (from catch all budget)
rate[reason]             = guaranteed minimum for this reason (from reason specific budget)
ceil[reason]             = global_cap (all reasons can burst up to global cap)
used[reason]             = nodes currently disrupting for this reason
excess_pool              = global_cap - sum(rate[all_reasons])
```

### Budget Computation

For a given reason R:

```
own_remaining  = max(rate[R] - used[R], 0)
used_from_pool = max(total_used - sum(min(used[reason], rate[reason]) for all reasons), 0)
free_pool      = max(excess_pool - used_from_pool, 0)

available[R]   = own_remaining + free_pool
```

In HTB terms: a class first consumes from its guaranteed `rate`. Once the `rate` is exhausted, it borrows from the parent's excess pool (surplus above all children's combined `rate`). It can borrow up to its `ceil` (which equals the global cap). The parent ensures total consumption never exceeds `global_cap`.

### Requirement: global_cap >= sum(rate)

If `global_cap == sum(rate)`: no excess pool, each reason is strictly limited to its `rate`. Pure isolation.

If `global_cap > sum(rate)`: the difference is the excess pool. Reasons can burst above their `rate` by borrowing from it.

If `global_cap < sum(rate)`: invalid configuration. The sum of guarantees exceeds the global cap. This should be rejected at validation time.

### Walkthrough: No Excess Pool

**Setup**: 100-node pool. Global cap 15 nodes. Drift rate 10. Consolidation rate 5. Excess pool = 0.

Each reason is hard limited to its rate:

| Scenario | Drift used | Consol used | Drift available | Consol available |
|----------|-----------|-------------|-----------------|------------------|
| Idle | 0 | 0 | 10 | 5 |
| Drift active | 8 | 0 | 2 | 5 |
| Drift at rate | 10 | 0 | 0 | 5 |
| Both active | 10 | 3 | 0 | 2 |
| Both at rate | 10 | 5 | 0 | 0 |

The budget behaves the way the YAML reads: Drift gets 10, Consolidation gets 5, and neither interferes with the other.

### Walkthrough: With Excess Pool

**Setup**: 100-node pool. Global cap 20 nodes. Drift rate 10. Consolidation rate 5. Excess pool = 5.

| Scenario | Drift used | Consol used | Pool used | Drift available | Consol available |
|----------|-----------|-------------|-----------|-----------------|------------------|
| Idle | 0 | 0 | 0 | 10 + 5 = 15 | 5 + 5 = 10 |
| Drift active | 8 | 0 | 0 | 2 + 5 = 7 | 5 + 5 = 10 |
| Drift at rate | 10 | 0 | 0 | 0 + 5 = 5 | 5 + 5 = 10 |
| Drift borrowing | 13 | 0 | 3 | 0 + 2 = 2 | 5 + 2 = 7 |
| Drift at global cap | 15 | 0 | 5 | 0 + 0 = 0 | 5 + 0 = 5 |
| Both borrowing | 13 | 5 | 3 | 0 + 2 = 2 | 0 + 2 = 2 |

Key observations:
- Drift can exceed its 10-node budget by borrowing from the excess pool when other reasons are not using it.
- Even when Drift borrows all 5 excess nodes, Consolidation still has its configured 5. The budget reads correctly.
- Unused capacity is not wasted. If one reason is idle, others can use the slack. But each reason's configured budget is always respected.

## Mapping to the Existing Budget API

Today's NodePool budget spec already expresses the HTB hierarchy:

```yaml
spec:
  disruption:
    budgets:
    - nodes: "15%"                           # no reasons = parent rate (global cap)
    - nodes: "10%", reasons: [Drifted]       # child class rate
    - nodes: "5%",  reasons: [Underutilized] # child class rate
```

The mapping:
- **Catch all budget** (no reasons) becomes the **parent `rate`** (global cap). All children's `ceil` equals this.
- **Reason specific budgets** become **child class `rate`** values (guaranteed minimums).
- **Excess pool** = parent rate - sum(child rates).

If no catch all budget exists: `global_cap = sum(rate)`, no excess pool. Pure isolation.

If no reason specific budgets exist (only a catch all): behavior is unchanged from today. All methods share a single global pool with no per reason guarantees.

## HTB Opens the Door to MultiLevel Budget Hierarchies

Today, budgets are configured per NodePool with no way to express cross NodePool constraints. Each NodePool is an island. There is no concept of "at most 20 nodes disrupting across the entire cluster" or "this group of NodePools shares a disruption budget." Users working with multiple NodePools have to manually coordinate budgets and hope the math works out.

HTB naturally supports multiple levels of hierarchy. The same parent/child model that gives per reason guarantees within a NodePool can extend upward:

```
Cluster (root)
  rate: 50 nodes              # cluster-wide disruption cap
  │
  ├── NodePool "critical"
  │     rate: 10 nodes         # guaranteed minimum for this pool
  │     ceil: 20 nodes         # can borrow up to 20 from cluster surplus
  │     │
  │     ├── Drifted:       rate 6
  │     └── Underutilized: rate 4
  │
  ├── NodePool "batch"
  │     rate: 30 nodes         # batch workloads get more disruption capacity
  │     ceil: 50 nodes         # can use full cluster budget if others are idle
  │     │
  │     ├── Drifted:       rate 20
  │     └── Underutilized: rate 10
  │
  └── Excess pool: 50 - 10 - 30 = 10 shared nodes
```

At each level, the same properties hold:
- Each class gets its guaranteed `rate` regardless of sibling activity.
- Unused capacity flows up and is available to siblings.
- No class exceeds its `ceil`.
- The parent enforces the total cap.

This is not possible with the current flat budget model. Today, a NodePool's budget is computed in isolation. There is no parent to coordinate across pools, no way to express "critical gets at least 10 even when batch is busy," and no way to cap total cluster-wide disruptions.

With HTB, these interactions fall out naturally from the hierarchy. The per NodePool budget becomes a mid-level class in the tree. The cluster-wide budget becomes the root. Per reason budgets are the leaves. Each level provides the same guarantee: your `rate` is yours, surplus is shared, and `ceil` is your maximum.

This does not need to be implemented all at once. The immediate value is per reason guarantees within a NodePool (the leaf level). Cluster-wide budgets (the root level) can be added later by introducing a new API object and wiring it as the HTB parent of existing NodePool budgets.

## Implementation Requirements

### 1. Track per reason disrupting counts

Currently, `BuildDisruptionBudgetMapping` counts all `MarkedForDeletion()` nodes as disrupting without tracking reason. HTB requires knowing how many nodes are disrupting **per reason**.

The disruption reason is already written to the NodeClaim at `queue.go:268` (`DisruptionReasonAnnotationKey`). It is not currently tracked in the in-memory state (`StateNode`).

Changes needed:
- Add `markedForDeletionReason v1.DisruptionReason` to `StateNode`
- Pass reason to `Cluster.MarkForDeletion()` from `StartCommand` (which already has `cmd.Reason()`)
- In `BuildDisruptionBudgetMapping`, count `used[reason]` separately from `used[total]`

### 2. Compute per reason available budget

Replace the current budget formula:

```
// Current: global only
available = allowedDisruptions - disrupting_all
```

With HTB computation:

```
// HTB: per reason with borrowing from excess pool
own_remaining = max(rate[reason] - used[reason], 0)
free_pool     = max(excess_pool - pool_used, 0)
available     = own_remaining + free_pool
```

### 3. Standalone DisruptionBudget CRD

Rather than reinterpreting the existing inline NodePool budget fields, introduce a new `DisruptionBudget` custom resource. NodePools would reference the CR via a new field (e.g. `disruptionBudgetRef`). This has several advantages:

- **Clean break from current semantics.** The inline budgets keep their existing behavior. Users opt into HTB by creating a DisruptionBudget and pointing their NodePool at it. No feature flag or silent semantic change needed.
- **Cluster wide scope becomes natural.** A DisruptionBudget with no NodePool selector applies globally as the root of the HTB tree. A DisruptionBudget targeting specific NodePools sits in the middle. Per reason budgets within the CR are the leaves.
- **Multi level hierarchy without API gymnastics.** The HTB tree (cluster > NodePool > reason) maps directly to the CR hierarchy rather than being inferred from a flat list of inline budget entries.
- **Independent lifecycle.** Budget configuration can be managed, audited, and versioned separately from NodePool configuration. Platform teams can own the budget CRs while application teams own NodePools.

### 4. Validation

Add validation to reject configurations where `sum(rate) > global_cap`. The sum of per reason budgets cannot exceed the global cap because the parent cannot allocate more capacity than it has.

## Backward Compatibility

With a standalone DisruptionBudget CRD, backward compatibility is straightforward. The existing inline NodePool budgets keep their current behavior. HTB semantics only apply when a NodePool references a DisruptionBudget CR. Users opt in explicitly by creating the CR and wiring the reference. No silent semantic changes, no feature flags needed.

## Open Questions

1. **Excess pool allocation fairness.** The excess pool is first-come-first-served. In the single-threaded controller, whichever method runs first in a cycle gets first access to the pool. Is this acceptable, or should the excess pool be allocated proportionally?

2. **Multiple reasons per budget.** A budget can list multiple reasons: `{nodes: "10%", reasons: [Drifted, Underutilized]}`. How does this map to HTB? Options: treat as a shared rate for those reasons (a single HTB class serving multiple reasons), or require single reason budgets for HTB to apply.

3. **Per reason ceil.** In this proposal, all reasons share `ceil = global_cap`. HTB supports per class ceilings. For example, Consolidation could have `ceil = 8` instead of 15, limiting how much it can borrow even when excess is available. Is per reason ceil useful, or is the global cap sufficient?
