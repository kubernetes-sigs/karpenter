# RFC: Configurable registeration TTL

## Motivation

[Issue](https://github.com/kubernetes-sigs/karpenter/issues/357)

Karpenter currently hardcodes [registeration TTL](https://github.com/kubernetes-sigs/karpenter/blob/main/pkg/controllers/nodeclaim/lifecycle/liveness.go#L44) which means nodes must take < 15 minutes to come up and register with the Kubernetes control plane.

This is problematic because some users have nodes that take longer that time to register. As mentioned in the issue, nodes that run GPU can often take > 15 minutes to start up. 

## Configuration

### Option 1 (Preferred):
#### Introduce a configuration registeration TTL to Node Pool then propagate to node claim

We propose introducing a new argument in the Node Pool and propagating it to the NodeClaim during its creation. When the liveness controller evaluates the registrationTTL, it will use the value specified in the NodeClaim.

Importantly, even if the registrationTTL is updated on the NodePool during NodeClaim registration, we will continue to use the NodeClaim's registrationTTL. This approach ensures clarity by demonstrating that the NodeClaim relies on variables defined in its own spec.

Furthermore, this variable will not be considered when evaluating drift, as it is only utilized during the registration process. If a node requires a longer TTL, it would have already been terminated by Karpenter. In cases where the node successfully registers, the TTL becomes irrelevant. Consequently, changes to this variable do not necessitate updates to the drift evaluation process.

Lastly, we will move registrationTTL to fully be in NodePool CRD and default it to 15m.

#### Concerns

N/A

### Option 2:
#### Introduce a configuration registeration TTL to NodeClass and propagate it to the NodeClaim

We will keep a default for all nodes at 15 minute TTL however in the cases that the NodeClass would be used with GPUs or known to take a longer time to register, we will expose a new configuration registerationTTL. We will allow users to configure their own TTL without presets

```yaml
apiVersion: karpenter.sh/v1
kind: NodePool
metadata:
  annotations:
    karpenter.sh/nodepool-hash: "13817533045586861872"
    karpenter.sh/nodepool-hash-version: v3
  creationTimestamp: "2025-02-12T17:59:44Z"
  name: example-nodepool
spec:
  disruption: {}
    consolidateAfter: 5m
    consolidationPolicy: WhenEmptyOrUnderutilized
  limits:
    nodes: "800"
  template:
    metadata: {}
    spec:
      expireAfter: 288h0m0s
      registrationTTL: 20m # New variable
```

#### Evaluation conditions 

1. When evaluating liveness, check if NodeClaim contains a registerationTTL if so utilize that TTL instead of the default. 
