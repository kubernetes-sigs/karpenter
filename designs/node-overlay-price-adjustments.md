# RFC: NodeOverlay `priceExpression`

## Summary

This RFC adds a CEL (Common Expression Language) field, `spec.priceExpression`, to NodeOverlay. Expressions receive the cloud provider's offering price as `price` and return the simulated price. In alpha, the field is additive: `spec.price` and `spec.priceAdjustment` remain unchanged, and an overlay may set only one of the three fields. Their deprecation at beta and removal at GA are handled by the graduation criteria.

## Motivation

### Problem Statement

NodeOverlays let users model costs that cloud provider APIs do not expose, such as enterprise discounts, software licenses, and carbon offsets. However, `priceAdjustment` represents only one percentage or fixed adjustment. It cannot combine a discount, a flat fee, and a surcharge in the required order.

Consider an organization that needs to model:

- A 10% global enterprise discount (EDP)
- A $0.08/hr storage attachment cost
- A 3% regional surcharge

Today, users must precompute the combined price. For example, modeling a percentage discount plus a fixed storage cost requires a fixed price for every affected offering. Provider prices vary by region and instance type, while discounts and storage costs may also vary by region. Users may therefore need separate overlays for each region and instance type instead of one regional expression that applies the local formula to every matching base price.

This workaround causes:

- **Many more overlays**: Every unique combination of region, instance type, and pricing terms may require a precomputed price overlay.
- **Maintenance burden**: Changing a discount or storage cost requires recalculating every affected overlay. Missed updates silently produce inaccurate scheduling decisions.
- **Performance impact**: More overlays increase API server list/watch traffic, informer cache size, and reconciliation work.

This limitation was raised in [kubernetes-sigs/karpenter#2616](https://github.com/kubernetes-sigs/karpenter/issues/2616).

With `priceExpression`, one overlay can apply a formula to every matching base price. Different regional terms may still require separate overlays, but users no longer need a fixed-price overlay for every instance type in that region.

### Use Cases

1. **Combined discounts**: Apply two discounts in one overlay: `price * 0.85 * 0.95`.

2. **License fees**: Apply a discount before adding a flat per-node fee: `price * 0.82 + 0.12`.

3. **Storage costs**: Choose whether a discount applies to compute and storage, `(price + 0.08) * 0.9`, or only compute, `price * 0.9 + 0.08`.

4. **Capacity-type pricing**: Model different effective discounts for spot and on-demand offerings to improve capacity-type selection.

## Goals

- Add a `spec.priceExpression` CEL field alongside `spec.price` and `spec.priceAdjustment` without changing the existing fields' behavior.
- Compile expressions during reconciliation and reuse the compiled program during scheduling.
- Surface CEL syntax and type errors as `ValidationSucceeded=False` conditions on the overlay.
- Permit negative prices as an intentional scheduling incentive, surfacing them via a non-blocking `PriceNonNegative=False` condition.
- Define a graduation path that deprecates `price` and `priceAdjustment` at beta, then removes them through versioned API conversion at GA.

## Non-Goals

- Exposing variables beyond `price`, such as instance family, zone, or capacity type.
- Supporting layering or merging of overlapping NodeOverlays in this proposal. See [Future Consideration: Layering and Overlay Overlap](#stacking-overlays).
- Auto-generating NodeOverlay resources from cost data sources (e.g. AWS Cost Explorer, billing APIs).
- Providing migration tooling in alpha.

## Proposed Solution

### API

`spec.priceExpression` accepts a CEL expression. The `price` variable is the offering's base price as a `double`. The expression must return a number, which becomes the simulated price.

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
  priceExpression: "(price * 0.9 + 0.05) * 1.03"
```

This applies a 10% discount, adds a $0.05 fee, and then applies a 3% surcharge. Parentheses make the order explicit.

An expression can also set a fixed price:

```yaml
priceExpression: "0.50"   # sets the offering price to exactly $0.50/hr
```

An overlay may set at most one of `priceExpression`, `price`, or `priceAdjustment`. Any combination of two or more fields is rejected at admission.

### Mathematical Order of Operations

For a $1.00 base price, different formulas produce different results:

| Expression | Calculation | Result |
|------------|-------------|--------|
| `(price * 0.9 + 0.05) * 1.03` | (1.00 × 0.90 + 0.05) × 1.03 | $0.9785 |
| `price * 0.9 * 1.03 + 0.05` | 1.00 × 0.90 × 1.03 + 0.05 | $0.9770 |
| `price * (1 - 0.10 + 0.03) + 0.05` | 1.00 × 0.93 + 0.05 | $0.9800 |

The expression makes the intended order explicit.

### Expression Environment

| Variable | Type | Description |
|----------|------|-------------|
| `price` | `double` | Raw cloud provider offering price; unaffected by other overlays. |

No other variables are exposed. The expression must return a `double`, `int`, or `uint`.

### Resolution Rules

For an offering, process matching overlays that configure price by descending weight:

1. **Base price**: Every calculation starts from the raw cloud provider price. An overlay never receives a price produced by another overlay.

2. **`priceExpression`**: If the highest-weight overlay specifies an expression, evaluate it against the base price. Its result becomes the simulated price; lower-weight overlays do not apply.

3. **`price` / `priceAdjustment`**: Otherwise, apply the highest-weight overlay using existing semantics. Lower-weight overlays do not apply.

Equal-weight overlays that update the same offering conflict; they are not tie-broken or composed.

### Design Details

**Compilation model**

The controller compiles expressions during reconciliation and stores each `cel.Program` with its price update. This keeps parsing and type-checking off the scheduling path.

**Controller changes**

1. Compile each expression during reconciliation. On failure, set `ValidationSucceeded=False` with reason `RuntimeValidation`.
2. Evaluate the program against every matched offering before storing it, surfacing evaluation errors before scheduling. On failure, set `ValidationSucceeded=False` with reason `ExpressionEvaluationError` and skip the overlay. This allows a valid lower-weight overlay to apply.
3. Normalize `price`, `priceAdjustment`, and `priceExpression` into the same internal price evaluator without rewriting the submitted spec. Preserve existing clamping and adjustment semantics for legacy fields.
4. Store the evaluator with the price update and apply it to the provider price during scheduling.
5. Track the overlay name by NodePool, instance type, zone, capacity type, and reservation ID so the launched NodeClaim can identify the price overlay that applied.

**Validation**

- **Syntax**: `RuntimeValidate` checks parsing and types during reconciliation. Errors set `ValidationSucceeded=False`.
- **Return type**: The expression must return a numeric type (`double`, `int`, or `uint`). Other return types are rejected at compile time.
- **Negative price**: Permitted, but sets the informational condition `PriceNonNegative=False`. See [Negative Prices](#negative-prices).
- **Evaluation errors**: Set `ValidationSucceeded=False` and `PriceAdjusted=False` with reason `ExpressionEvaluationError`. The expression is not applied.
- **Mutual exclusion**: CRD validation rejects any overlay that sets two or more of `priceExpression`, `price`, and `priceAdjustment`.

**Negative Prices** <a name="negative-prices"></a>

`price` and `priceAdjustment` clamp results to `0` through `AdjustedPrice()` in `cloudprovider/types.go`. `priceExpression` permits negative values.

When an expression returns a negative value, the controller applies it without clamping, logs the overlay name, and sets `PriceNonNegative=False` with reason `NegativePrice`. The condition does not affect `Ready`. Because `OrderByPrice` sorts negative prices first, they act as a strong scheduling preference.

Operators who want a scheduling incentive without distorting disruption cost math should prefer a very small positive price (e.g. `0.001`) over a negative value.

**Status Conditions** <a name="status-conditions"></a>

| Condition | Ready dependency | Description |
|-----------|------------------|-------------|
| `ValidationSucceeded` | Yes | Runtime validation, conflict detection, and expression evaluation succeeded. |
| `PriceAdjusted` | Yes | The overlay has no price configuration, or its price configuration matched and adjusted at least one instance type offering. |
| `PriceNonNegative` | No | All evaluated `priceExpression` results were non-negative. Informational only. |

| Condition | Reason | Description |
|-----------|--------|-------------|
| `ValidationSucceeded` | `RuntimeValidation` | Expression failed to compile. |
| `ValidationSucceeded` | `Conflict` | Equal-weight overlays update the same offering. |
| `ValidationSucceeded` | `ExpressionEvaluationError` | Expression failed for a matched offering. |
| `PriceAdjusted` | `NoMatchingInstanceTypes` | No offering matched the price configuration. |
| `PriceAdjusted` | `ExpressionEvaluationError` | Expression failed for a matched offering. |
| `PriceNonNegative` | `NegativePrice` | Expression returned a negative price. |

`ValidationSucceeded=False` or `PriceAdjusted=False` sets `Ready=False`. `PriceNonNegative=False` does not affect `Ready`.

The conditions are updated during the NodeOverlay controller's reconcile loop, which runs at least every 6 hours and on any NodeOverlay, NodePool, or NodeClass change.

**NodeClaim observability** <a name="nodeclaim-observability"></a>

When Karpenter launches a NodeClaim, it uses the resolved offering labels to annotate the NodeClaim with the matching price overlay from the instance type store:

| Annotation | Description |
|------------|-------------|
| `karpenter.sh/price-overlay-applied` | Name of the price overlay that adjusted the launched offering. |

This annotation is best-effort and written after the cloud provider returns concrete labels (instance type, zone, capacity type, and reservation ID). It is omitted when no price overlay applies.

## Graduation Criteria

### Alpha (`v1alpha1`) — No breaking changes

- Add `priceExpression` alongside `price` and `priceAdjustment`; an overlay may set at most one of the three fields.
- CEL expressions are compiled at reconcile time and evaluated at scheduling time.
- Normalize all three fields into one internal evaluator while preserving their semantics and the submitted spec.
- Implement the proposed status conditions and NodeClaim annotation.
- `price` and `priceAdjustment` continue to behave exactly as today. No existing overlays require changes.
- Add unit and integration coverage for evaluation, validation, negative prices, and the annotation.

The [Kubernetes deprecation policy](https://kubernetes.io/docs/reference/using-api/deprecation-policy/) requires API elements to be removed through a new API version and objects to round-trip between served versions without losing information. The graduation plan follows that model rather than rewriting user-authored specs during reconciliation.

### Beta (`v1beta1`) — Deprecate legacy fields

- Mark `price` and `priceAdjustment` as deprecated.
- Continue accepting them while the controller normalizes all three price fields internally.
- Emit an event and log message directing users to `priceExpression`, but leave the submitted spec unchanged.
- At least two releases in alpha with no breaking API changes.
- Positive user feedback from alpha adoption.

### GA (`v1`) — Legacy fields removed

- Remove `price` and `priceAdjustment` from the `v1` schema.
- Use [CRD version conversion](https://kubernetes.io/docs/tasks/extend-kubernetes/custom-resources/custom-resource-definition-versioning/) to translate legacy values into equivalent expressions, preserving round-trip compatibility while deprecated API versions are served.
- [Migrate stored objects](https://kubernetes.io/docs/tasks/manage-kubernetes-objects/storage-version-migration/) to the `v1` storage version before deprecated versions stop being served.
- At least two releases in beta with no breaking API changes.
- No outstanding critical bugs or performance regressions.
- E2E test coverage across at least two cloud provider implementations.

## Alternatives Considered

### Future Consideration: Layering and Overlay Overlap <a name="stacking-overlays"></a>

The [RFC discussion](https://github.com/kubernetes-sigs/karpenter/pull/3004#discussion_r3500062419), building on [kubernetes-sigs/karpenter#2616](https://github.com/kubernetes-sigs/karpenter/issues/2616), proposed chaining expressions so each overlay receives the output of a lower-weight overlay. This would compose independently selected adjustments without enumerating every selector intersection.

Chaining does not match current resolution: the highest-weight price update wins, lower-weight updates are ignored, and equal-weight updates conflict. Layering should therefore be a general NodeOverlay overlap-resolution design, not a special rule for `priceExpression`. Such a design must define winners, layers, conflicts, ordering, status, and observability across overlay fields.

This RFC defers layering because of that complexity. We should revisit it if further user demand justifies a general solution for independently managed, overlapping overlays.

The main complications are:

- **Resolution semantics**: Price selects one winner, while capacity can merge distinct resource keys. Sequential transformations would add another composition rule.
- **Order of operations**: No sequence of discounts, fees, and surcharges is universally correct. Layering must define how weight controls order; a single expression makes the rule explicit.
- **Conflicts**: Layering must define whether equal-weight overlays conflict or compose, and whether weight means priority or application order.
- **Observability**: Conditions and annotations must represent every contributing overlay, application order, and partial failure.

CEL composes arithmetic within one overlay without changing resolution. It does not provide the ownership and reuse benefits of independent layers.

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

The list makes each adjustment introspectable and allows operation-specific validation. However, it adds a custom operation language, still relies on list order, and makes formulas such as discounting compute but not a flat fee harder to express. CEL provides more flexibility with less API surface.
