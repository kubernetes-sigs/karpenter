# NodeOverlay `priceAdjustments`

## Summary

This RFC proposes two approaches for enabling cumulative price adjustment stacking across matching NodeOverlays, solving the combinatorial problem that arises when multiple independent cost dimensions (enterprise discounts, licensing fees, regional surcharges) need to be modeled independently but applied together.

**Option 1 (preferred)** introduces a `spec.priceAdjustments` field—an ordered list of adjustments applied sequentially within a single overlay. This is the recommended approach: it requires no cross-overlay coordination, has a simple and explicit ordering model, and avoids the policy-mixing error states inherent in Option 2.

**Option 2** introduces a `spec.resolutionPolicy` field that controls how multiple overlays with `priceAdjustment` are resolved when more than one matches an instance type.

## Motivation

### Problem Statement

NodeOverlays were introduced to let users inject real-world pricing information that cloud provider APIs don't surface—Savings Plans, enterprise discounts, per-node software licensing fees, and carbon-offset costs. The current resolution model applies only the highest-weight overlay when multiple overlays match an instance type, which creates a combinatorial problem for users with multiple independent cost dimensions.

Consider an organization that needs to model:

- A 10% global enterprise discount (EDP)
- A $0.05/hr per-node fee for a licensed security agent
- A 5% regional adjustment for a specific availability zone

Today they cannot express these as independent overlays. Because only the highest-weight overlay is applied, they must either collapse all three factors into a single overlay per instance type—requiring N overlays for N instance types—or accept inaccurate pricing and therefore sub-optimal scheduling decisions.

The workaround of pre-computing each combined price and creating one NodeOverlay per unique price point does technically produce correct prices, but it creates severe operational problems:

- **Overlay sprawl**: A cloud provider region may expose hundreds of instance types across multiple capacity types and zones. Expressing even two independent cost dimensions requires a NodeOverlay for every distinct (instance type, combined price) combination. A modest setup—say 200 instance types × 3 cost dimensions—can demand hundreds of NodeOverlay resources just to model pricing accurately.
- **Maintenance burden**: When a single cost dimension changes (e.g. an EDP discount is renegotiated), every overlay that encodes that dimension must be updated atomically. A missed update silently produces incorrect prices for affected instance types.
- **Performance impact**: Each NodeOverlay resource is a watched object in the Kubernetes API. A large number of overlays increases list-watch pressure on the API server, bloats the controller's informer cache, and raises the cost of every reconcile loop that recomputes per-instance-type prices.

This directly limits the utility of NodeOverlays as a cost-modeling tool and was raised in [kubernetes-sigs/karpenter#2616](https://github.com/kubernetes-sigs/karpenter/issues/2616).

### Use Cases

1. **Layered enterprise discounts**: A global EDP overlay applies a -15% base reduction, and a separate Graviton-specific overlay applies a further -5%. Both should contribute to the final simulated price.

2. **Per-node licensing fees**: A security agent charges a flat $0.069/hr per node regardless of instance size. This fee should stack on top of any existing discount overlays rather than compete with them.

3. **Regional surcharges**: An overlay adds +3% for instances in a specific availability zone for compliance cost modeling. This should compose with capacity-type discounts without displacing them.

4. **Spot vs on-demand gap refinement**: An on-demand reservation discount narrows the price gap with spot. Modeled accurately, Karpenter can make more correct capacity-type decisions during provisioning.

## Option 1: `priceAdjustments` Ordered List ⭐ Preferred

### Overview

Replace the single `spec.priceAdjustment` field with a `spec.priceAdjustments` list. Adjustments are applied to the instance type's price sequentially in list order, with each step operating on the running result of the previous step. This approach requires no cross-overlay coordination and expresses layered cost modeling entirely within a single overlay resource.

### API

```yaml
apiVersion: karpenter.sh/v1alpha1
kind: NodeOverlay
metadata:
  name: on-demand-cost-model
spec:
  weight: 100
  requirements:
    - key: karpenter.sh/capacity-type
      operator: In
      values: ["on-demand"]
  priceAdjustments:
    - "-10%"    # EDP enterprise discount
    - "+0.05"   # licensed security agent fee ($/hr flat)
    - "+3%"     # AZ compliance surcharge
```

The existing `priceAdjustment` (singular) field is retained for backward compatibility. If both are specified, validation will reject the resource. Operators migrating from `priceAdjustment` simply wrap the single value in a list.

### Resolution Rules

For a given instance type offering, let $M$ be the set of all matching NodeOverlays sorted by weight descending (alphabetical by name to break ties):

1. **Base price**: The cloud provider's price is the initial value. If any overlay in $M$ specifies `spec.price`, the highest-weight such overlay's value becomes the new base.

2. **Overlay application**: For each overlay in $M$ (descending weight order), apply its `priceAdjustments` entries in list order, each entry operating on the running price.

3. **Highest-weight-wins for `priceAdjustment` (singular)**: Overlays using the legacy singular field retain current override semantics—only the highest-weight match applies.

### Mathematical Order of Operations

| Overlay | Field | Adjustment | Calculation | Result |
|---------|-------|------------|-------------|--------|
| `on-demand-cost-model` | `priceAdjustments[0]` | −10% | $1.000 × 0.90 | $0.900 |
| `on-demand-cost-model` | `priceAdjustments[1]` | +$0.05 | $0.900 + $0.05 | $0.950 |
| `on-demand-cost-model` | `priceAdjustments[2]` | +3% | $0.950 × 1.03 | $0.979 |

When multiple overlays each carry `priceAdjustments`, they are applied in descending weight order, with each overlay's full list executing before moving to the next overlay.

### Design Details

**Controller Changes**

1. Fetch all active NodeOverlay resources.
2. For each instance type, collect all matching overlays sorted by weight descending, name ascending.
3. Apply `spec.price` from the highest-weight overlay that sets it (resets base price).
4. For each overlay in order, iterate its `priceAdjustments` list, applying each entry to the running price.
5. For overlays using the legacy `priceAdjustment` field, apply only the highest-weight match (current behavior).
6. Cache the resolved `SimulatedPrice` per InstanceType.

**Validation**

- **`priceAdjustments`**: Must be a non-empty list if specified. Each entry must be a valid absolute (`+0.05`, `-0.10`) or percentage (`+3%`, `-10%`) adjustment.
- **Mutual exclusion**: Specifying both `priceAdjustment` and `priceAdjustments` on the same resource is a validation error.
- **Negative price handling**: Negative resolved prices are allowed. If the resolved price for an instance type is ≤ 0 after applying all adjustments, Karpenter will emit a warning event on the NodeOverlay resource and continue scheduling normally. Operators should treat a negative price as a misconfiguration signal and correct their adjustments.

**Backward Compatibility**

The existing `priceAdjustment` (singular) field is unchanged. All existing NodeOverlay resources continue to behave exactly as today. Operators opt in to `priceAdjustments` explicitly.

### Pros and Cons

**Pros**
- No cross-overlay coordination required; all adjustments are self-contained in one resource.
- Ordering is explicit and unambiguous—list index determines application order.
- Simple mental model: one overlay, one ordered list of cost transformations.
- No new policy concept to learn; no mixing/conflict errors across resources.

**Cons**
- All cost dimensions must be managed within a single overlay per selector combination, which may not suit organizations where different teams own different cost components.
- Cannot express "this adjustment only if another adjustment is also active" without combining them into a single overlay.

---

## Option 2: `resolutionPolicy` Field

### Overview

Introduce `spec.resolutionPolicy` on NodeOverlay to control how multiple matching overlays are resolved when computing instance type prices. The two policies are `Override` (current behavior: highest weight wins) and `Stack` (new: weight-ordered cumulative application). `Override` remains the default for backward compatibility.

**Scope**: `resolutionPolicy` governs resolution of `spec.priceAdjustment` only. It has no effect on `spec.price` or `spec.capacity`, which retain their existing resolution semantics:

- `spec.price` always uses highest-weight-wins, since it is an absolute replacement value rather than a modifier.
- `spec.capacity` always uses merge-by-weight.

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

### Design Details

**Controller Changes**

1. Fetch all active NodeOverlay resources.
2. For each instance type, collect all matching overlays sorted by weight descending, name ascending.
3. Identify the highest-weight overlay with `spec.price`; use it to reset the base if present.
4. Check that all matching overlays with `priceAdjustment` share the same `resolutionPolicy`. If not, mark conflicting overlays not-Ready and fall back to highest-weight-wins.
5. If policy is `Override`, apply only the highest-weight overlay's `priceAdjustment`. If policy is `Stack`, iterate all matching overlays in descending weight order, applying each `priceAdjustment` to the running price.
6. Cache the resolved `SimulatedPrice` per InstanceType. Invalidate only when a NodeOverlay is created, updated, or deleted, or when the cloud provider returns updated instance type data.

**Validation**

- **`resolutionPolicy`**: Must be `Override` or `Stack`. Defaults to `Override` when omitted, preserving backward compatibility.
- **Negative price handling**: Negative resolved prices are allowed. If the resolved price for an instance type is ≤ 0 after applying all adjustments, Karpenter will emit a warning event on the NodeOverlay resource and continue scheduling normally. Operators should treat a negative price as a misconfiguration signal and correct their adjustments.
- **`resolutionPolicy` scope**: Only affects `spec.priceAdjustment`. Setting `resolutionPolicy: Stack` on an overlay that only sets `spec.price` or `spec.capacity` has no effect and a warning will be surfaced via the `Ready` status condition.
- **Policy mixing**: If overlays matching the same instance type have conflicting `resolutionPolicy` values (some `Override`, some `Stack`), all conflicting overlays will have their `Ready` status condition set to false with a descriptive message. Resolution falls back to highest-weight-wins for that instance type until the conflict is resolved.
- **Conflict reporting**: The existing `Ready` status condition on NodeOverlay resources continues to report equal-weight conflicts. This behavior is unchanged.

**Backward Compatibility**

`resolutionPolicy` defaults to `Override`, so all existing NodeOverlay resources behave exactly as they do today without any changes. Operators opt in to stacking explicitly per-overlay.

### Pros and Cons

**Pros**
- Different teams can independently manage separate overlays (e.g. finance owns `edp-discount`, security team owns `agent-fee`) without touching each other's resources.
- Overlays can carry independent selectors, allowing fine-grained per-dimension matching.

**Cons**
- Requires all co-matching overlays to agree on `resolutionPolicy`, creating implicit cross-resource coupling.
- Policy mixing is an error state that is hard to detect without observing runtime status conditions.
- Application order depends on weight assignment across independently owned resources, which may produce surprising results when teams adjust weights for unrelated reasons.

---