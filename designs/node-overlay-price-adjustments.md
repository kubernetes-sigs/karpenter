# RFC: NodeOverlay `priceExpression`

## Summary

This RFC proposes adding a `spec.priceExpression` field to NodeOverlay that accepts a CEL (Common Expression Language) expression. The expression receives the cloud provider's offering price as `self.price` and evaluates to a numeric value, giving operators full control over the price calculation in a single, readable formula. It also proposes a graduation path that deprecates the existing `price` and `priceAdjustment` fields in favor of `priceExpression`, removing them at GA.

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

1. **Layered enterprise discounts**: A global -15% enterprise discount and a -5% instance-family-specific discount both need to apply to the same instance type. Rather than maintaining a separate overlay per instance type with a pre-computed combined rate, both factors can be encoded in a single expression: `self.price * 0.85 * 0.95`.

2. **Per-node licensing fees**: A security agent charges a flat $0.069/hr per node regardless of instance size. The fee is added after percentage discounts are applied.

3. **Marketplace software licensing plus discounts**: A commercial workload requires a flat per-node license fee, while the underlying compute price receives an ISP, marketplace, or enterprise discount. For example, `self.price * 0.82 + 0.12` models an 18% compute discount plus a $0.12/hr software license fee per node.

4. **Storage attachment cost plus enterprise discount**: A workload requires an EBS volume cost to be modeled per node in addition to the instance price, while the combined or compute-only cost receives an enterprise discount. Operators can choose the correct business rule explicitly, such as `(self.price + 0.08) * 0.9` when the discount applies to both compute and storage, or `self.price * 0.9 + 0.08` when the EBS cost is not discounted.

5. **Spot vs on-demand gap refinement**: Enterprise discounts may apply to spot pricing while reservation savings apply to on-demand pricing, changing the effective gap between capacity types. Modeled accurately, Karpenter can make more correct capacity-type decisions during provisioning.

## Goals

- Add a `spec.priceExpression` CEL field to NodeOverlay that replaces `spec.price` and `spec.priceAdjustment` as the primary price configuration mechanism.
- Compile expressions once at reconcile time and store the compiled program so evaluation at scheduling time is a single cheap numeric computation.
- Surface CEL syntax and type errors as `ValidationSucceeded=False` conditions on the overlay.
- Permit negative prices as an intentional scheduling incentive, surfacing them via a non-blocking `PriceNonNegative=False` condition.
- Define a graduation path that auto-migrates `price` and `priceAdjustment` to equivalent `priceExpression` values at beta, then removes the legacy fields at GA.

## Non-Goals

- Exposing additional expression variables beyond `self.price` (e.g. instance family, zone, capacity type). The environment is intentionally minimal.
- Supporting stacking or merging of multiple price overlays. See [Alternatives Considered: Stacking Overlays](#stacking-overlays).
- Auto-generating NodeOverlay resources from cost data sources (e.g. AWS Cost Explorer, billing APIs).
- Providing a migration CLI or tooling beyond the admission webhook auto-rewrite.

## Proposed Solution

### API

Add a `spec.priceExpression` field that accepts a CEL expression string. The expression exposes a single variable `self` with a `price` field (double) representing the instance type's base price for the current offering. The expression must evaluate to a numeric value, which becomes the new simulated price.

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

`priceExpression` can also set a price directly by using a bare numeric literal:

```yaml
priceExpression: "0.50"   # sets the offering price to exactly $0.50/hr
```

Specifying `priceExpression` alongside `price` or `priceAdjustment` is a validation error enforced at admission time.

### Mathematical Order of Operations

The operator fully controls order of operations via the expression. For the motivating example with a $1.00 base price:

| Expression | Calculation | Result |
|------------|-------------|--------|
| `(self.price * 0.9 + 0.05) * 1.03` | (1.00 × 0.90 + 0.05) × 1.03 | $0.9785 |
| `self.price * 0.9 * 1.03 + 0.05` | 1.00 × 0.90 × 1.03 + 0.05 | $0.9770 |
| `self.price * (1 - 0.10 + 0.03) + 0.05` | 1.00 × 0.93 + 0.05 | $0.9800 |

The operator chooses which form accurately models their cost structure. This is the core advantage over a fixed ordered-list approach: there is no ambiguity about what "apply in order" means across multipliers and additive fees.

### Expression Environment

| Variable | Type | Description |
|----------|------|-------------|
| `self.price` | `double` | The cloud provider's offering price. Always the raw provider price; not affected by `spec.price` overrides on other overlays. |

The CEL environment is deliberately minimal. No other variables are exposed. The expression must return a `double`, `int`, or `uint`.

### Resolution Rules

For a given instance type offering, let $M$ be the set of all matching NodeOverlays sorted by weight descending (alphabetical by name to break ties):

1. **Base price**: The cloud provider's offering price is always the input to any price computation. `spec.price` from one overlay is never visible to `priceExpression` on another overlay.

2. **`priceExpression`**: If the highest-weight overlay in $M$ specifies `priceExpression`, it is evaluated with `self.price` set to the cloud provider's offering price. The result becomes the new simulated price. Lower-weight overlays are not applied.

3. **`spec.price` / `spec.priceAdjustment`** (deprecated at beta, removed at GA): If the highest-weight overlay in $M$ specifies one of these legacy fields, it is applied using the existing semantics. Lower-weight overlays are not applied. Both fields are superseded by `priceExpression` and will be removed at GA.

### Design Details

**Compilation model**

CEL expressions are compiled once when the NodeOverlay controller reconciles, not on every scheduling decision. The compiled `cel.Program` is stored alongside the price update in the instance type store. This makes evaluation at scheduling time cheap (a single map lookup and numeric computation) with no repeated parsing overhead.

**Controller changes**

1. On reconcile, call `cel.Compile(overlay.Spec.PriceExpression)` for each overlay with a `priceExpression`.
2. If compilation fails, set `ValidationSucceeded=False` with reason `RuntimeValidation`.
3. Before storing the overlay, evaluate the expression against every matched offering. If any matched offering fails to evaluate, set `ValidationSucceeded=False` with reason `ExpressionEvaluationError` and do not store the overlay. This prevents a high-weight invalid expression from silencing a lower-weight valid overlay.
4. Store the compiled `cel.Program` in the `priceUpdate` struct alongside the overlay update string.
5. At scheduling time, evaluate the stored program against the cloud provider's offering price to produce the adjusted price.
6. Track the overlay name and adjusted price in the instance type store so Karpenter can annotate launched NodeClaims with the price overlay that affected the selected offering. Reserved offerings are distinguished by reservation ID in addition to instance type, zone, and capacity type.

**Validation**

- **Syntax**: Validated at admission time via `RuntimeValidate`. Any CEL parse or type-check error surfaces as a validation error on the overlay resource.
- **Return type**: The expression must return a numeric type (`double`, `int`, or `uint`). Other return types are rejected at compile time.
- **Negative price**: Permitted. When an expression produces a negative result, a `log.Info` warning is emitted and the overlay surfaces `PriceNonNegative=False`. The overlay remains Ready. See [Negative Prices](#negative-prices).
- **Runtime evaluation errors**: Set `ValidationSucceeded=False` and `PriceAdjusted=False` with reason `ExpressionEvaluationError`. The expression is not stored or applied. Lower-weight valid overlays can still apply to the affected offerings.
- **Mutual exclusion**: Specifying `priceExpression` alongside `price` or `priceAdjustment` is a validation error enforced at admission time.

**Negative Prices** <a name="negative-prices"></a>

The existing `price` and `priceAdjustment` fields clamp their result to `0` via `AdjustedPrice()` in `cloudprovider/types.go`. Only `priceExpression` can produce a negative value.

When the NodeOverlay controller evaluates a CEL expression and the result is negative:
1. The price is applied as-is (not clamped). A negative price causes those offerings to sort ahead of all positive-priced offerings in `OrderByPrice`, acting as a hard scheduling preference.
2. `log.Info` emits a warning with the overlay name.
3. The overlay sets `PriceNonNegative=False` with reason `NegativePrice`. This condition is informational only and does not affect `Ready`.

Operators who want a scheduling incentive without distorting disruption cost math should prefer a very small positive price (e.g. `0.001`) over a negative value.

**Status Conditions** <a name="status-conditions"></a>

| Condition | Ready dependency | Description |
|-----------|------------------|-------------|
| `ValidationSucceeded` | Yes | Runtime validation, conflict detection, and expression evaluation succeeded. |
| `PriceAdjusted` | Yes | The overlay has no price configuration, or its price configuration matched and adjusted at least one instance type offering. |
| `PriceNonNegative` | No | All evaluated `priceExpression` results were non-negative. Informational only. |

`ValidationSucceeded` reasons:

| Reason | Description |
|--------|-------------|
| `RuntimeValidation` | CEL expression failed to compile (syntax or type error). |
| `Conflict` | Two overlays of the same weight target the same offering. |
| `ExpressionEvaluationError` | Expression compiled but failed to evaluate against one or more matched offerings. |

`PriceAdjusted` reasons:

| Reason | Description |
|--------|-------------|
| `NoMatchingInstanceTypes` | Price configuration did not match any instance type offerings. |
| `ExpressionEvaluationError` | Expression compiled but failed to evaluate against one or more matched offerings. |

`PriceNonNegative` reasons:

| Reason | Description |
|--------|-------------|
| `NegativePrice` | The expression produced a negative price for one or more matched offerings. |

`ValidationSucceeded=False` or `PriceAdjusted=False` sets `Ready=False`. `PriceNonNegative=False` does not affect `Ready`.

The conditions are updated during the NodeOverlay controller's reconcile loop, which runs at least every 6 hours and on any NodeOverlay, NodePool, or NodeClass change.

**NodeClaim observability** <a name="nodeclaim-observability"></a>

When Karpenter launches a NodeClaim, it annotates the NodeClaim with overlay information from the instance type store:

| Annotation | Description |
|------------|-------------|
| `karpenter.sh/price-overlay-applied` | Name of the price overlay that adjusted the launched offering. |
| `karpenter.sh/price-overlay-adjusted-price` | Adjusted price used for the launched offering. |
| `karpenter.sh/capacity-overlay-applied` | Name of the capacity overlay that adjusted the launched instance type. |

These annotations are best-effort and written after the cloud provider returns concrete labels (instance type, zone, capacity type, reservation ID). Omitted when no overlay applies.

## Graduation Criteria

### Alpha (`v1alpha1`) — No breaking changes

- `priceExpression` is added to the NodeOverlay API alongside the existing `price` and `priceAdjustment` fields.
- All three fields are accepted. Specifying more than one is a validation error (mutually exclusive).
- CEL expressions are compiled at reconcile time and evaluated at scheduling time.
- Status conditions (`ValidationSucceeded`, `PriceAdjusted`, `PriceNonNegative`) implemented and accurate.
- NodeClaim annotations (`price-overlay-applied`, `price-overlay-adjusted-price`) populated at launch time.
- `price` and `priceAdjustment` continue to behave exactly as today. No existing overlays require changes.
- Unit and integration tests covering `priceExpression` evaluation, validation errors, negative prices, and NodeClaim annotation.

### Beta (`v1beta1`) — Deprecate and auto-migrate legacy fields

- `price` and `priceAdjustment` are marked deprecated in the API documentation and CRD schema descriptions.
- An admission webhook auto-rewrites incoming resources that use the legacy fields:
  - `spec.price: "0.50"` → `spec.priceExpression: "0.50"`
  - `spec.priceAdjustment: "-10%"` → `spec.priceExpression: "self.price * 0.90"`
  - `spec.priceAdjustment: "+5%"` → `spec.priceExpression: "self.price * 1.05"`
  - `spec.priceAdjustment: "-0.05"` → `spec.priceExpression: "self.price - 0.05"`
  - `spec.priceAdjustment: "+0.05"` → `spec.priceExpression: "self.price + 0.05"`
- After rewriting, the resource is stored with only `priceExpression` set. Operators reading their resources back will observe the rewritten form.
- A controller event and `log.Info` warning are emitted for each overlay that was auto-migrated, directing the operator to update their manifests.
- Existing overlays written before the beta upgrade are migrated on their next reconcile.
- At least two releases in alpha with no breaking API changes.
- Positive user feedback from alpha adoption.

### GA (`v1`) — Legacy fields removed

- `price` and `priceAdjustment` are removed from the NodeOverlay CRD spec. Resources specifying these fields are rejected at admission with a clear error message directing the operator to `priceExpression`.
- Only `priceExpression` is supported for price configuration.
- At least two releases in beta with no breaking API changes.
- No outstanding critical bugs or performance regressions.
- E2E test coverage across at least two cloud provider implementations.

## Alternatives Considered

### Stacking Multiple Overlays <a name="stacking-overlays"></a>

The most frequently requested alternative is allowing multiple price overlays to apply to the same instance type offering in a defined order, so that each overlay contributes one dimension of the final price. For example, a global EDP discount overlay and a per-node licensing fee overlay would both match `m5.xlarge`, and the controller would apply them in sequence.

We explicitly rejected stacking for the following reasons:

**It breaks the existing resolution model.** Every other NodeOverlay field—capacity, requirements—uses a highest-weight-wins model. One overlay wins and is reported in the status; the others are recorded as conflicts. Stacking price adjustments would be the only field with a different composition model, making the overall API harder to understand and document.

**Order of operations is inherently business-specific.** A per-node licensing fee, a cloud provider discount, and a tax surcharge can be validly applied in multiple orders depending on the contract. For example:

- `(self.price * 0.9 + 0.05) * 1.03` — AZ surcharge applied after license fee
- `self.price * 0.9 * 1.03 + 0.05` — AZ surcharge on compute only, license fee added after

These produce different prices, and neither order is universally correct. A stacking model must pick one order (e.g. highest-weight-first), but that order may be wrong for many users. The controller cannot know the right answer; only the operator can. Embedding the order of operations inside a CEL expression keeps the business logic explicit and visible in the resource rather than hidden in controller behavior.

**Conflict semantics break down with stacking.** Karpenter's conflict detection flags two overlays of equal weight that target the same offering as a `Conflict` error. With stacking, the question becomes: should two same-weight overlays conflict (existing behavior) or compose additively (stacking behavior)? Either answer is surprising in some scenario. The `weight` field's meaning shifts from "this overlay wins" to something ambiguous between "this overlay takes priority" and "this overlay is applied at this layer."

**Status and observability become ambiguous.** When a NodeClaim is launched, Karpenter annotates it with the overlay name that adjusted the offering. With stacking, the price is the result of N overlays. The annotation would need to list all N overlays, and the `PriceAdjusted` condition would need to account for partial application—what does it mean if two of three stacked overlays applied successfully but one failed? Attributing the final price becomes opaque to both operators and Karpenter itself.

**Debugging is harder.** When an operator observes an unexpected price on a NodeClaim annotation, they need to reconstruct which sequence of overlays produced it and in what order. With a single expression, the formula is self-documenting in the resource that owns the price model.

**CEL already solves the composition problem more cleanly.** The motivation for stacking is that operators want to compose multiple independent cost factors without duplicating overlays. A single CEL expression achieves the same result with explicit, readable arithmetic and no changes to the resolution model.

### Structured Operation List

An earlier design considered a structured list field, e.g.:

```yaml
spec:
  priceAdjustments:
    - op: multiply
      value: "0.9"
    - op: add
      value: "0.05"
    - op: multiply
      value: "1.03"
```

**Advantages**

- Each adjustment is individually named and introspectable.
- Individual adjustments could be validated by type (percentage, absolute, multiplier) rather than relying on CEL type-checking.

**Disadvantages**

- "Apply in order" still encodes the order of operations in the list, which is exactly what operators need to control explicitly. The structured form offers no ordering advantage over a CEL expression.
- Harder to express non-trivial formulas (e.g. applying a surcharge only to the compute portion before adding a flat fee).
- Requires a new custom operation language when CEL is already a well-known, tested standard used elsewhere in Kubernetes.
- More API surface to define, validate, and maintain for equivalent expressive power.

The CEL expression approach was chosen because it gives operators full control over order of operations in a single readable string, uses a well-known language, and requires no new operation vocabulary.

## Pros and Cons

**Pros**
- Operator has complete, explicit control over order of operations—no ambiguity about how multipliers and additive fees interact.
- All adjustments for a cost model live in one expression, with no implicit coupling across overlay resources.
- Consistent with CEL usage elsewhere in Kubernetes (admission webhooks, `kubeReserved`/`systemReserved` in the AWS provider).
- Expressions are compiled once at reconcile time; evaluation at scheduling time is a single cheap numeric computation.
- Simpler API surface than a structured list: one string field, standard language semantics.
- Graduation path removes `price` and `priceAdjustment`, shrinking the API surface over time rather than growing it.

**Cons**
- CEL is less approachable than structured fields for operators unfamiliar with expression languages. A typo produces a compile error rather than a field-level validation message.
- Harder to introspect programmatically (e.g. "what discounts apply to this instance type?") than a structured list of named adjustments.
- Cannot compose adjustments across independently owned overlays—teams that want separate overlays for separate cost dimensions still need to merge them into a single expression (or use the weight-based highest-wins model at coarser granularity).
- Expressions that are syntactically valid but semantically wrong (e.g. `self.price * 0.0`) will produce correct-but-unexpected prices with no warning.
- The auto-migration of `priceAdjustment` strings at beta requires parsing the adjustment format (`-10%`, `+0.05`, etc.) and emitting a CEL equivalent—the translation is mechanical but adds controller complexity.
