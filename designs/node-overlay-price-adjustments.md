# NodeOverlay `priceExpression`

## Summary

This RFC proposes replacing the existing single `spec.priceAdjustment` field with a `spec.priceExpression` field that accepts a CEL (Common Expression Language) expression. The expression receives the instance type's base price as `self.price` and must evaluate to a non-negative numeric value, giving operators full control over order of operations while collapsing all adjustments into a single, readable formula.

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

1. **Layered enterprise discounts**: A global EDP overlay applies a -15% base reduction, and a Graviton-specific overlay applies a further -5% on top. Both can be expressed in one expression with explicit ordering.

2. **Per-node licensing fees**: A security agent charges a flat $0.069/hr per node regardless of instance size. The fee is added after percentage discounts are applied.

3. **Regional surcharges**: An overlay adds +3% for instances in a specific availability zone for compliance cost modeling. The expression makes the order of operations explicit.

4. **Spot vs on-demand gap refinement**: An on-demand reservation discount narrows the price gap with spot. Modeled accurately, Karpenter can make more correct capacity-type decisions during provisioning.

## Proposed Solution: `priceExpression` CEL Field

### Overview

Replace `spec.priceAdjustment` with a `spec.priceExpression` field that accepts a CEL expression string. The expression exposes a single variable `self` with a `price` field (double) representing the instance type's base price for the current offering. The expression must evaluate to a non-negative numeric value, which becomes the new simulated price.

This approach was suggested by @jmdeal in [kubernetes-sigs/karpenter#3004](https://github.com/kubernetes-sigs/karpenter/pull/3004) as a cleaner alternative to maintaining a structured list of adjustments.

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

The existing `priceAdjustment` and `price` fields are retained for backward compatibility. Specifying `priceExpression` alongside either is a validation error.

### Expression Environment

| Variable | Type | Description |
|----------|------|-------------|
| `self.price` | `double` | The offering's base price (cloud provider price, or the value set by a `spec.price` override on a higher-weight overlay) |

The CEL environment is deliberately minimal. No other variables are exposed. The expression must return a `double`, `int`, or `uint`. Returning a negative value is a validation error at admission time and also guarded at evaluation time.

### Resolution Rules

For a given instance type offering, let $M$ be the set of all matching NodeOverlays sorted by weight descending (alphabetical by name to break ties):

1. **Base price**: The cloud provider's price is the initial value. If any overlay in $M$ specifies `spec.price`, the highest-weight such overlay's value becomes the base.

2. **`priceExpression`**: If the highest-weight overlay in $M$ specifies `priceExpression`, it is evaluated with `self.price` set to the base price. The result becomes the new simulated price. Lower-weight overlays are not applied (same highest-weight-wins semantics as today).

3. **Legacy `priceAdjustment`**: Overlays using the legacy field retain current override semantics—only the highest-weight match applies.

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
- **Negative price**: Expressions that evaluate to a negative value are rejected at evaluation time. The offering retains its previous price, and a warning event is emitted on the overlay.
- **Mutual exclusion**: Specifying `priceExpression` alongside `price` or `priceAdjustment` on the same resource is a validation error.

**Backward Compatibility**

The existing `priceAdjustment` and `price` fields are unchanged. All existing NodeOverlay resources continue to behave exactly as today. Operators opt in to `priceExpression` explicitly.

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

### Migration from `priceAdjustment`

Existing single-adjustment overlays migrate trivially:

| Before | After |
|--------|-------|
| `priceAdjustment: "-10%"` | `priceExpression: "self.price * 0.9"` |
| `priceAdjustment: "+0.05"` | `priceExpression: "self.price + 0.05"` |
| `price: "0.75"` | `price: "0.75"` (unchanged) |
