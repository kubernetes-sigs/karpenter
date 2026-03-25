# Size Class Lock Threshold Usage Guide

## Overview

The Size Class Lock Threshold feature allows you to control when Karpenter locks a NodeClaim to a specific size class during provisioning. This helps prevent nodes from growing too large and improves disruption control.

## How It Works

When a NodeClaim reaches the specified pod count threshold, Karpenter "locks" it to its current size class. After locking:
- The NodeClaim can only accept pods that fit within its locked size class
- New pods that would require a larger instance type will trigger creation of a new NodeClaim
- This prevents nodes from continuously growing and makes disruption more predictable

## Configuration

The size class lock threshold is configured per-NodePool using an annotation:

```yaml
apiVersion: karpenter.sh/v1
kind: NodePool
metadata:
  name: default
  annotations:
    karpenter.sh/size-class-lock-threshold: "10"  # Lock after 10 pods
spec:
  template:
    spec:
      nodeClassRef:
        group: karpenter.kwok.sh
        kind: KwokNodeClass
        name: default
```

### Annotation Key

`karpenter.sh/size-class-lock-threshold`

### Valid Values

- **Positive integer** (e.g., `"5"`, `"10"`, `"20"`): Lock the size class after this many pods are scheduled
- **Not set or `"0"`**: Feature disabled (default behavior)
- **Negative values**: Feature disabled

## Examples

### Example 1: Lock after 5 pods

```yaml
apiVersion: karpenter.sh/v1
kind: NodePool
metadata:
  name: small-workloads
  annotations:
    karpenter.sh/size-class-lock-threshold: "5"
spec:
  # ... rest of spec
```

This configuration will:
1. Allow the first 5 pods to be scheduled on a NodeClaim
2. After 5 pods, lock the NodeClaim to its current size class
3. Additional pods that fit in the same size class can still be added
4. Pods requiring a larger size class will trigger a new NodeClaim

### Example 2: Different thresholds for different workloads

```yaml
---
apiVersion: karpenter.sh/v1
kind: NodePool
metadata:
  name: batch-jobs
  annotations:
    karpenter.sh/size-class-lock-threshold: "20"  # Allow more pods before locking
spec:
  # ... spec for batch workloads
---
apiVersion: karpenter.sh/v1
kind: NodePool
metadata:
  name: web-services
  annotations:
    karpenter.sh/size-class-lock-threshold: "5"  # Lock earlier for predictability
spec:
  # ... spec for web services
```

### Example 3: Disable the feature

```yaml
apiVersion: karpenter.sh/v1
kind: NodePool
metadata:
  name: default
  # No annotation = feature disabled
spec:
  # ... rest of spec
```

Or explicitly:

```yaml
apiVersion: karpenter.sh/v1
kind: NodePool
metadata:
  name: default
  annotations:
    karpenter.sh/size-class-lock-threshold: "0"  # Explicitly disabled
spec:
  # ... rest of spec
```

## Applying to Existing Clusters

To enable size class locking on an existing NodePool:

```bash
kubectl annotate nodepool default karpenter.sh/size-class-lock-threshold=10
```

To disable:

```bash
kubectl annotate nodepool default karpenter.sh/size-class-lock-threshold-
```

## Choosing the Right Threshold

Consider these factors when choosing a threshold value:

1. **Workload characteristics**: 
   - Uniform pod sizes → Higher threshold (10-20)
   - Variable pod sizes → Lower threshold (3-5)

2. **Disruption tolerance**:
   - Need predictable disruption → Lower threshold
   - Optimize for bin-packing → Higher threshold

3. **Cost optimization**:
   - Lower thresholds may create more nodes but with more predictable sizes
   - Higher thresholds allow better bin-packing but less predictable node sizes

## Monitoring

Check if size class locking is active on your NodeClaims:

```bash
kubectl get nodeclaims -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.metadata.annotations.karpenter\.sh/locked-size-class}{"\n"}{end}'
```

NodeClaims with a locked size class will show the size class number (1-5).

## Troubleshooting

### Pods not scheduling

If pods aren't scheduling and you have size class locking enabled:

1. Check if existing NodeClaims are locked:
   ```bash
   kubectl get nodeclaims -o yaml | grep -A 2 "locked-size-class"
   ```

2. Check if the pod would fit in the locked size class:
   - Review pod CPU/memory requests
   - Compare against the locked NodeClaim's capacity

3. Consider:
   - Increasing the threshold value
   - Disabling the feature for that NodePool
   - Creating a separate NodePool without size class locking

### Too many small nodes

If you're seeing many small nodes instead of consolidation:

1. The threshold might be too low
2. Consider increasing the threshold value
3. Review your pod resource requests

## Related Features

- **Pod Deletion Cost Management**: Works alongside size class locking to optimize disruption
- **Consolidation**: Size class locking affects how consolidation evaluates replacement options
