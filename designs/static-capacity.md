# Karpenter - Static Capacity

## Background
Karpenter currently operates as a dynamic cluster autoscaler, automatically adjusting node counts based on pending pod demand. However, several important use cases require maintaining a fixed set of nodes:

1. Performance-critical applications where just-in-time provisioning latency is unacceptable
2. Workloads that need guaranteed capacity regardless of pod demand  
3. Scenarios where fixed node counts are preferred

Currently, users attempt to achieve static capacity through workarounds:
- Running placeholder pods to maintain minimum capacity
- Using separate node management tools alongside Karpenter (MNG/Fargate/Self-managed)
- Configuring complex provisioner requirements to approximate static behavior

## Proposal
Extend the existing NodePool resource to support static provisioning capabilities by adding new fields:

```yaml
spec:
  # Makes NodePool static when specified
  replicas: 5  # Number of nodes to maintain
```

Key aspects:
1. Static NodePools maintain fixed node count regardless of pod demand
2. Will not be considered as consolidation candidate (at least in v1alpha1)
3. Nodes are spread across AZs by default
4. Inherit existing Karpenter features like drift detection and interruption handling
5. Use existing NodeClaim infrastructure with owner references
  

### Modeling & Validation

When `replicas` is specified, the `NodePool` enters a static provisioning mode where certain disruption-related fields become irrelevant or misleading. Specifically:

- `limits` and `weight` (used to influence consolidation and node selection) are not meaningful for static capacity.
- `consolidationPolicy` and `consolidateAfter` (which control Karpenter’s consolidation logic) must not be used when the node count is fixed via `replicas`.

However, **Kubernetes' schema-level validation (OpenAPI-based) lacks the ability to express conditional logic between fields**. This introduces limitations:

- Non-pointer types defaulting at parse time
- Default values being indistinguishable from explicitly set values in admission webhooks

### Disruption

Karpenter already has Disruption and Interruption Management for nodes we can use the same mechanism. 
Since Static Nodepool and NodeClaim is a variant of existing Nodepool/NodeClaim it will inherit Karpenter dynamic provider integration. Drift Detection can also be inherited to trigger replacement of drifted nodes. During this Karpenter will respect the Disruption Budget.

We will support scaling static node pools using a command such as:

```sh
kubectl scale nodepool <name> --replicas=10

```

When a user scales down a static NodePool, Karpenter will drain nodes and terminate the corresponding NodeClaims/instances.
Key behavior distinctions:
- User-driven actions (scaling replicas) bypass disruption budgets. The user explicitly intends to remove capacity, and we honor that request immediately.
- Karpenter-driven actions (e.g., consolidation, drift) respect disruption budgets and scheduling safety. These are only applicable when replicas is not set.


### Consolidation

Static NodePools are not eligible for consolidation. They act like any other static capacity source (e.g., Managed Node Groups). Their lifecycle is managed directly by the user, not by Karpenter’s dynamic optimization logic.
However, the nodes provisioned by a static pool:
- Participate in scheduling decisions (i.e., pods can land on them)
- Are monitored for drift and interruption, enabling graceful re-creation when necessary
- Will be evenly distributed across AZs when requirements span multiple zones

This means they remain first-class citizens in the cluster but are excluded from cost-based disruption decisions.

## Requirements

The requirements field in a static NodePool behaves identically to dynamic pools—it defines the constraints for all NodeClaims launched under that NodePool.
In static pools, we must choose multiple concrete node configurations up front—i.e., for replicas: 10, we select 10 NodeClaims matching the requirement set.
If the requirements allow multiple combinations:
- Karpenter selects the optimal combination based on cost, availability, and zone balancing
- This selection is done once at provisioning time (unlike dynamic pools, where evaluation occurs per provisioning event)

To ensure high availability and fault tolerance, we automatically spread static nodes across AZs when the zone requirement includes multiple values.
```yaml
- key: topology.kubernetes.io/zone
  operator: In
  values: ["us-west-2a", "us-west-2b", "us-west-2c"]
```

This ensures the nodes are evenly distributed unless the user explicitly narrows the zone selection to a single AZ.

## Example 

```yaml
apiVersion: karpenter.sh/v1beta1
kind: NodePool
metadata:
  name: static-prod-nodepool
spec:
  replicas: 12
  template:
    metadata:
      labels:
        nodepool-type: static
    spec:
      nodeClassRef:
        group: karpenter.k8s.aws
        kind: EC2NodeClass
        name: myEc2NodeClass
      expireAfter: 720h
      taints:
        - key: example.com/special-taint
          effect: NoSchedule
      requirements:
        - key: topology.kubernetes.io/zone
          operator: In
          values: ["us-west-2a", "us-west-2b", "us-west-2c"]
        - key: karpenter.k8s.aws/instance-type
          operator: In
          values: ["m5.2xlarge"]
  disruption:
    budgets:
      - nodes: 10%
      # On Weekdays during business hours, don't do any deprovisioning.
      - schedule: "0 9 * * mon-fri"
        duration: 8h
        nodes: "0"
```

## Alternative Proposal: New Static NodePool API


The alternative approach would be to create a separate StaticNodePool API focused solely on static provisioning. This would include:
- Dedicated API for static provisioning
- Clear separation from existing Nodepool 
- Validation rules specific to static provisioning

However, this approach was rejected because:
- Many core functionalities would be shared between static and dynamic capacity management
- Creates unnecessary cognitive overhead for users
- Requires duplicate documentation

The better approach is to extend the existing NodePool API since the differences represent different modes of the same fundamental abstraction rather than entirely separate concepts.

