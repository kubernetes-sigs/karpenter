# NodeOverlay `priceExpression`

## Summary

This RFC proposes adding a `spec.priceExpression` field that accepts a CEL (Common Expression Language) expression. The expression receives the instance type's base price as `self.price` and evaluates to a numeric value, giving operators full control over order of operations in a single, readable formula. Negative results are permitted as an intentional scheduling incentive but surface as a `PriceNonNegative=False` warning condition on the overlay.

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

1. **Layered enterprise discounts**: A global enterprise discount overlay applies a -15% base reduction, and an instance type or family specific overlay applies a further -5% on top. Both can be expressed in one expression with explicit ordering.

2. **Per-node licensing fees**: A security agent charges a flat $0.069/hr per node regardless of instance size. The fee is added after percentage discounts are applied.

3. **Marketplace software licensing plus discounts**: A commercial workload requires a flat per-node license fee, while the underlying compute price receives an ISP, marketplace, or enterprise discount. For example, `self.price * 0.82 + 0.12` models an 18% compute discount plus a $0.12/hr software license fee per node.

4. **Storage attachment cost plus enterprise discount**: A workload requires an EBS volume cost to be modeled per node in addition to the instance price, while the combined or compute-only cost receives an enterprise discount. Operators can choose the correct business rule explicitly, such as `(self.price + 0.08) * 0.9` when the discount applies to both compute and storage, or `self.price * 0.9 + 0.08` when the EBS cost is not discounted.

5. **Spot vs on-demand gap refinement**: Enterprise discounts may apply to spot pricing while reservation savings apply to on-demand pricing, changing the effective gap between capacity types. Modeled accurately, Karpenter can make more correct capacity-type decisions during provisioning.

## Proposed Solution: `priceExpression` CEL Field

### Overview

Add a `spec.priceExpression` field that accepts a CEL expression string. The expression exposes a single variable `self` with a `price` field (double) representing the instance type's base price for the current offering. The expression must evaluate to a numeric value, which becomes the new simulated price. Negative results are permitted; see [Negative Prices](#negative-prices).

This approach was suggested by @jmdeal in [kubernetes-sigs/karpenter#3004](https://github.com/kubernetes-sigs/karpenter/pull/3004) as a cleaner alternative to maintaining a structured list of price operations.

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
  priceExpression: "(self.price * 0.9 + 0.05) * 1.03"
```

The three-factor example from the motivation section (−10% EDP, +$0.05 agent fee, +3% AZ surcharge) is expressed as a single formula. The operator controls order of operations directly via parenthesization.

The existing `price` field is retained for explicit price overrides. Specifying `priceExpression` alongside `price` is a validation error.

`priceExpression` can also set a price directly by using a bare numeric literal that ignores `self.price`:

```yaml
priceExpression: "0.50"   # equivalent to price: "0.50"
```

This makes `spec.price` largely redundant — `priceExpression` subsumes it. `spec.price` is retained because it is more self-documenting for the simple override case and is validated by a regex at admission time rather than by CEL type-checking.

### Expression Environment

| Variable | Type | Description |
|----------|------|-------------|
| `self.price` | `double` | The cloud provider's offering price as a double. This is always the raw provider price; it is not affected by `spec.price` overrides on other overlays. |

The CEL environment is deliberately minimal. No other variables are exposed. The expression must return a `double`, `int`, or `uint`. Returning a negative value is permitted; see [Negative Prices](#negative-prices) below.

### Resolution Rules

For a given instance type offering, let $M$ be the set of all matching NodeOverlays sorted by weight descending (alphabetical by name to break ties):

1. **Base price**: The cloud provider's offering price is always the input to any price computation. `spec.price` from one overlay is never visible to `priceExpression` on another overlay — the two fields are independent and compete under highest-weight-wins.

2. **`priceExpression`**: If the highest-weight overlay in $M$ specifies `priceExpression`, it is evaluated with `self.price` set to the cloud provider's offering price. The result becomes the new simulated price. Lower-weight overlays are not applied (same highest-weight-wins semantics as today).

3. **`spec.price`**: If the highest-weight overlay in $M$ specifies `spec.price` (and does not specify `priceExpression`), that value becomes the simulated price directly. Lower-weight overlays are not applied.

### Why Not Merge Multiple Matching Overlays?

An earlier design considered allowing multiple matching price overlays to merge, then resolving conflicts by weight and applying the resulting set of adjustments in order.

**Advantages**

- Users could define one global base discount overlay, then stack narrower overlays for instance-family-specific discounts instead of repeating the global discount in every instance-family expression.
- Independently owned cost dimensions could remain in separate resources. For example, a platform team could own the enterprise discount while an application team owns a per-node licensing fee.
- Shared adjustments could be updated once and automatically affect every more-specific overlay that composes with them.

**Disadvantages**

- The rest of NodeOverlay follows a simple highest-weight-wins model: when multiple overlays target the same field, the controller picks one winner and reports conflicts rather than composing partial state across resources. Price adjustment merging would be the only behavior with a different resolution model.
- Merge resolution would require users and maintainers to reason about:

  - which fields merge and which fields conflict,
  - whether ordering comes from weight, declaration order, operation type, or another explicit field,
  - how additive fees interact with percentage discounts,
  - whether a higher-weight overlay replaces or composes with a lower-weight overlay, and
  - how to explain status, events, and validation errors when the final price comes from several resources.

**Decision**

We moved away from multiple-overlay merge resolution because it makes NodeOverlay resolution significantly more complex while solving only this one feature's composition problem. Those semantics are especially hard for cost modeling because the order of operations is business-specific. A per-node licensing fee, an EBS cost, an ISP discount, and an enterprise discount can all be validly ordered in different ways depending on the contract. Encoding that behavior through multiple overlay merge rules would make the API look declarative while hiding the most important part of the calculation in controller-specific conflict resolution. A single CEL expression keeps the existing overlay selection semantics intact and makes the final arithmetic explicit in the resource that owns the price model.

### Mathematical Order of Operations

The operator fully controls order of operations via the expression. For the motivating example with a $1.00 base price:

| Expression | Calculation | Result |
|------------|-------------|--------|
| `(self.price * 0.9 + 0.05) * 1.03` | (1.00 × 0.90 + 0.05) × 1.03 | $0.9785 |
| `self.price * 0.9 * 1.03 + 0.05` | 1.00 × 0.90 × 1.03 + 0.05 | $0.9770 |
| `self.price * (1 - 0.10 + 0.03) + 0.05` | 1.00 × 0.93 + 0.05 | $0.9800 |

The operator chooses which form accurately models their cost structure. This is the core advantage over a fixed ordered-list approach: there is no ambiguity about what "apply in order" means across multipliers and additive fees.

### Design Details

**Compilation model**

CEL expressions are compiled once when the NodeOverlay controller reconciles, not on every scheduling decision. The compiled `cel.Program` is stored alongside the price update in the instance type store. This makes evaluation at scheduling time cheap (a single map lookup and numeric computation) with no repeated parsing overhead.

**Controller changes**

1. On reconcile, call `cel.Compile(overlay.Spec.PriceExpression)` for each overlay with a `priceExpression`.
2. If compilation fails, mark the overlay not-Ready with a descriptive message (same path as existing runtime validation failures).
3. Store the compiled `cel.Program` in the `priceUpdate` struct alongside the overlay update string.
4. At scheduling time, evaluate `program.Eval(map["self"]["price"] = basePrice)` to produce the adjusted price.

**Validation**

- **`priceExpression` syntax**: Validated at admission time via `RuntimeValidate`. Any CEL parse or type-check error surfaces as a validation error on the overlay resource before it is applied.
- **Return type**: The expression must return a numeric type (`double`, `int`, or `uint`). Expressions returning booleans, strings, or other types are rejected at compile time.
- **Negative price**: Expressions that evaluate to a negative value are **permitted**. Negative prices are intentionally allowed so operators can use them as a scheduling incentive (Karpenter's `OrderByPrice` sorts cheaper offerings first, so a negative price causes those offerings to be strongly preferred). When a CEL expression produces a negative result for any offering, a `log.Info` warning is emitted and the overlay's `PriceNonNegative` status condition is set to `False`. See [Status Conditions](#status-conditions).
- **Mutual exclusion**: Specifying `priceExpression` alongside `price` on the same resource is a validation error.

**Negative Prices** <a name="negative-prices"></a>

The existing `price` and `priceAdjustment` fields clamp their result to `0` via `AdjustedPrice()` in `cloudprovider/types.go`, so they cannot produce negative prices. Only `priceExpression` can produce a negative value.

When the NodeOverlay controller evaluates a CEL expression at reconcile time and the result is negative:
1. The price is applied as-is (not clamped). A negative price causes those offerings to sort ahead of all positive-priced offerings in `OrderByPrice`, acting as a hard scheduling preference.
2. `log.Log.Info` emits a warning with the expression string and the result value.
3. The overlay's `PriceNonNegative` status condition is set to `False` (reason: `NegativePrice`).

Operators who want a scheduling incentive without distorting disruption cost math should prefer a very small positive price (e.g. `0.001`) over a negative value.

**Status Conditions** <a name="status-conditions"></a>

Two new status conditions are added to NodeOverlay alongside the existing `ValidationSucceeded`:

| Condition | True | False |
|-----------|------|-------|
| `PriceApplied` | The overlay's price configuration (`price`, `priceAdjustment`, or `priceExpression`) matched at least one offering on at least one NodePool. | The price spec is set but no matching offerings were found across all NodePools. Reason: `NoMatchingInstanceTypes`. Overlays with no price spec are always `True`. |
| `PriceNonNegative` | No CEL expression on this overlay produced a negative price for any evaluated offering. | At least one CEL expression evaluation produced a negative price. Reason: `NegativePrice`. Overlays without `priceExpression` are always `True`. |

Both conditions are set during the NodeOverlay controller's reconcile loop, which runs at least every 6 hours and on any NodeOverlay, NodePool, or NodeClass change.

**Backward Compatibility**

The existing `price` field is unchanged. Existing NodeOverlay resources that use `price` continue to behave exactly as today. Operators opt in to `priceExpression` explicitly.

### Pros and Cons

**Pros**
- Operator has complete, explicit control over order of operations—no ambiguity about how multipliers and additive fees interact.
- All adjustments for a cost model live in one expression, with no implicit coupling across overlay resources.
- Consistent with CEL usage elsewhere in Kubernetes (admission webhooks, `kubeReserved`/`systemReserved` in the AWS provider).
- Expressions are compiled once at reconcile time; evaluation at scheduling time is a single cheap numeric computation.
- Simpler API surface than a structured list: one string field, standard language semantics.

**Cons**
- CEL is less approachable than structured fields for operators unfamiliar with expression languages. A typo produces a compile error rather than a field-level validation message.
- Harder to introspect programmatically (e.g. "what discounts apply to this instance type?") than a structured list of named adjustments.
- Cannot compose adjustments across independently owned overlays—teams that want separate overlays for separate cost dimensions still need to merge them into a single expression (or use the weight-based highest-wins model at coarser granularity).
- Expressions that are syntactically valid but semantically wrong (e.g. `self.price * 0.0`) will produce correct-but-unexpected prices with no warning.
- Expressions that produce negative prices are permitted but distort Karpenter's disruption cost math (which uses the simulated price as a cost estimate). The `PriceNonNegative=False` status condition surfaces this, but the disruption behavior is still affected.
