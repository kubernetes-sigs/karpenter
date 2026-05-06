# NodeOverlay Stacking Resolution

## Summary

This RFC proposes adding a `spec.resolutionPolicy` field to the NodeOverlay API to give operators explicit control over how multiple matching overlays are resolved when computing instance type prices. The two policies are `Override` (current behavior: highest weight wins) and `Stack` (new: weight-ordered cumulative application). `Override` remains the default for backward compatibility.

## Motivation

### Problem Statement

NodeOverlays were introduced to let users inject real-world pricing information that cloud provider APIs don't surface—Savings Plans, enterprise discounts, per-node software licensing fees, and carbon-offset costs. The current resolution model applies only the highest-weight overlay when multiple overlays match an instance type, which creates a combinatorial problem for users with multiple independent cost dimensions.

Consider an organization that needs to model:

- A 10% global enterprise discount (EDP)
- A $0.05/hr per-node fee for a licensed security agent
- A 5% regional adjustment for a specific availability zone

Today they cannot express these as independent overlays. Because only the highest-weight overlay is applied, they must either collapse all three factors into a single overlay per instance type—requiring N overlays for N instance types—or accept inaccurate pricing and therefore sub-optimal scheduling decisions.

This directly limits the utility of NodeOverlays as a cost-modeling tool and was raised in [kubernetes-sigs/karpenter#2616](https://github.com/kubernetes-sigs/karpenter/issues/2616).

### Use Cases

1. **Layered enterprise discounts**: A global EDP overlay applies a -15% base reduction, and a separate Graviton-specific overlay applies a further -5%. Both should contribute to the final simulated price.

2. **Per-node licensing fees**: A security agent charges a flat $0.069/hr per node regardless of instance size. This fee should stack on top of any existing discount overlays rather than compete with them.

3. **Regional surcharges**: An overlay adds +3% for instances in a specific availability zone for compliance cost modeling. This should compose with capacity-type discounts without displacing them.

4. **Spot vs on-demand gap refinement**: An on-demand reservation discount narrows the price gap with spot. Modeled accurately, Karpenter can make more correct capacity-type decisions during provisioning.

## Proposal

Introduce `spec.resolutionPolicy` on NodeOverlay with two values:

- **`Override`** (default): The highest-weight matching overlay that sets `priceAdjustment` wins; all others are ignored. This is the current behavior.
- **`Stack`**: This overlay's `priceAdjustment` is applied cumulatively on top of any higher-weight overlays, in descending weight order.

**Scope**: `resolutionPolicy` governs resolution of `spec.priceAdjustment` only. It has no effect on `spec.price` or `spec.capacity`, which retain their existing resolution semantics:

- `spec.price` always uses highest-weight-wins, since it is an absolute replacement value rather than a modifier.
- `spec.capacity` always uses merge-by-weight. Extending `resolutionPolicy` to cover these fields is left for future work once real-world usage patterns for `priceAdjustment` stacking are established.

### API

```yaml
apiVersion: karpenter.sh/v1alpha1
kind: NodeOverlay
metadata:
  name: edp-discount
spec:
  weight: 100
  resolutionPolicy: Stack   # Override | Stack; defaults to Override
  requirements:
    - key: karpenter.sh/capacity-type
      operator: In
      values: ["on-demand"]
  priceAdjustment: "-10%"
---
apiVersion: karpenter.sh/v1alpha1
kind: NodeOverlay
metadata:
  name: agent-fee
spec:
  weight: 50
  resolutionPolicy: Stack
  requirements:
    - key: kubernetes.io/os
      operator: In
      values: ["linux"]
  priceAdjustment: "+0.05"
---
apiVersion: karpenter.sh/v1alpha1
kind: NodeOverlay
metadata:
  name: az-surcharge
spec:
  weight: 10
  resolutionPolicy: Stack
  requirements:
    - key: topology.kubernetes.io/zone
      operator: In
      values: ["us-east-1a"]
  priceAdjustment: "+3%"
```

### Resolution Rules

For a given instance type offering, let $M$ be the set of all matching NodeOverlays sorted by weight descending (alphabetical by name to break ties):

1. **Base price**: The cloud provider's price is the initial value. If any overlay in $M$ specifies `spec.price`, the highest-weight such overlay's value becomes the new base.

2. **Policy uniformity**: All overlays in $M$ that specify `priceAdjustment` must share the same `resolutionPolicy`. Mixing `Override` and `Stack` overlays that match the same instance type is invalid and is reported via the `Ready` status condition on each conflicting overlay. Resolution falls back to highest-weight-wins (fail-open) when a conflict is detected.

3. **Override**: Only the highest-weight overlay's `priceAdjustment` is applied. All others are ignored. This is the current behavior.

4. **Stack**: All overlays' `priceAdjustment` values are applied in descending weight order, each operating on the result of the previous step.

### Mathematical Order of Operations

| Overlay | Policy | Weight | Adjustment | Calculation | Result |
|---------|--------|--------|------------|-------------|--------|
| Provider | — | — | Base price | $1.000 | $1.000 |
| `base-override` | Override | 70 | −20% | $1.000 × 0.80 | $0.800 |
| `edp-discount` | Stack | 60 | −10% | $0.800 × 0.90 | $0.720 |
| `agent-fee` | Stack | 50 | +$0.05 | $0.720 + $0.05 | $0.770 |
| `az-surcharge` | Stack | 10 | +3% | $0.770 × 1.03 | $0.793 |

If no `Override` overlay is present, `Stack` overlays apply directly to the provider base price.

## Design Details

### Controller Changes

The resolution logic in the NodeOverlay controller currently selects a single winner per instance type. It will be updated as follows:

1. Fetch all active NodeOverlay resources.
2. For each instance type, collect all matching overlays sorted by weight descending, name ascending.
3. Identify the highest-weight overlay with `spec.price`; use it to reset the base if present.
4. Check that all matching overlays with `priceAdjustment` share the same `resolutionPolicy`. If not, mark conflicting overlays not-Ready and fall back to highest-weight-wins.
5. If policy is `Override`, apply only the highest-weight overlay's `priceAdjustment`. If policy is `Stack`, iterate all matching overlays in descending weight order, applying each `priceAdjustment` to the running price.
6. Cache the resolved `SimulatedPrice` per InstanceType. Invalidate only when a NodeOverlay is created, updated, or deleted, or when the cloud provider returns updated instance type data.

The scheduling simulation consumes pre-resolved InstanceType prices and requires no changes.

### Validation

- **`resolutionPolicy`**: Must be `Override` or `Stack`. Defaults to `Override` when omitted, preserving backward compatibility.
- **Negative price handling**: If the resolved price for an instance type is ≤ 0 after applying all adjustments, Karpenter will emit an error event and remove that instance type from consideration for scheduling (ice it) until the overlay configuration is corrected. Negative prices are never allowed; there is no clamping or silent fallback.
- **`resolutionPolicy` scope**: Only affects `spec.priceAdjustment`. Setting `resolutionPolicy: Stack` on an overlay that only sets `spec.price` or `spec.capacity` has no effect and a warning will be surfaced via the `Ready` status condition.
- **Policy mixing**: If overlays matching the same instance type have conflicting `resolutionPolicy` values (some `Override`, some `Stack`), all conflicting overlays will have their `Ready` status condition set to false with a descriptive message. Resolution falls back to highest-weight-wins for that instance type until the conflict is resolved.
- **Conflict reporting**: The existing `Ready` status condition on NodeOverlay resources continues to report equal-weight conflicts. This behavior is unchanged.

### Backward Compatibility

`resolutionPolicy` defaults to `Override`, so all existing NodeOverlay resources behave exactly as they do today without any changes. Operators opt in to stacking explicitly per-overlay.

## Alternatives Considered

### Implicit Stacking (No API Change)

Change `priceAdjustment` resolution to always be cumulative without a new field. All matching overlays stack automatically.

**Pros**: No API changes; simpler for the common stacking case.

**Cons**: Silent behavior change for any cluster with multiple overlays matching the same instance type. Operators cannot express "this overlay should suppress all others" without a new escape hatch. An explicit field is safer at the v1alpha1 stage and is more consistent with Kubernetes API conventions of declarative, self-describing configuration.

### Feature Gate

Introduce a `NodeOverlayStacking` feature gate that globally switches all `priceAdjustment` resolution to cumulative.

**Pros**: Easy to roll back.

**Cons**: All-or-nothing; operators cannot choose different resolution semantics for different groups of overlays within the same cluster.

### NodePool-scoped Policy

Allow the NodePool spec to declare a stacking policy governing overlay resolution for nodes in that pool.

**Pros**: Scoped rollout per pool.

**Cons**: NodeOverlays are deliberately pool-agnostic. Placing the policy on NodePool contradicts the design intent and forces each pool to re-specify the same policy, introducing configuration drift.
