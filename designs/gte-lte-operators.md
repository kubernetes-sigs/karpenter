# GTE and LTE Operators for Requirements

## Overview

Karpenter currently supports `Gt` (greater than) and `Lt` (less than) operators for numeric requirements. These are strict inequalities, which creates an awkward user experience. For example, to require a minimum of 2 CPUs, users must specify `Gt 1` instead of the more intuitive `Gte 2`.

## Motivation

Users frequently need to express inclusive bounds on numeric requirements like CPU count, memory, or GPU count. The current off-by-one interface is confusing and error-prone:

| User Intent | Current Syntax | Proposed Syntax |
|-------------|----------------|-----------------|
| At least 2 CPUs | `Gt 1` | `Gte 2` |
| At most 8 CPUs | `Lt 9` | `Lte 8` |

## Proposal

Add two new operators to Karpenter's requirement system:
- `Gte` - Greater than or equal to
- `Lte` - Less than or equal to

### API Changes

```yaml
apiVersion: karpenter.sh/v1
kind: NodePool
spec:
  template:
    spec:
      requirements:
        - key: karpenter.k8s.aws/instance-cpu
          operator: Gte  # New operator
          values: ["2"]
        - key: karpenter.k8s.io/instance-memory
          operator: Lte  # New operator
          values: ["64"]
```

### Implementation

1. Define new operator constants in `pkg/apis/v1/`
2. Add `greaterThanOrEqual` and `lessThanOrEqual` fields to the `Requirement` struct
3. Update `NewRequirementWithFlexibility` to handle `Gte` and `Lte`
4. Update validation to accept the new operators
5. Update intersection and comparison logic

### Backward Compatibility

- Existing `Gt` and `Lt` operators remain unchanged
- New operators are additive; no breaking changes

## Alternatives Considered

1. **Automatic conversion**: Convert `Gte N` to `Gt N-1` internally. Rejected because it loses semantic meaning and complicates debugging.
2. **Documentation only**: Document the off-by-one pattern. Rejected because it doesn't solve the UX problem.
