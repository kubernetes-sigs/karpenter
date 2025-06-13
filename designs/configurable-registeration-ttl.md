# RFC: Configurable registeration TTL

## Motivation

[Issue](https://github.com/kubernetes-sigs/karpenter/issues/357)

Karpenter currently hardcodes [registeration TTL](https://github.com/kubernetes-sigs/karpenter/blob/main/pkg/controllers/nodeclaim/lifecycle/liveness.go#L44) which means nodes must take < 15 minutes to come up and register with the Kubernetes control plane.

This is problematic because some users have nodes that take longer that time to register. As mentioned in the issue, nodes that run GPU can often take > 15 minutes to start up. 


## Configuration
### Option 1:
#### Introduce a configuration registeration TTL to NodeClass and propagate it to the NodeClaim

We will keep a default for all nodes at 15 minute TTL however in the cases that the NodeClass would be used with GPUs or known to take a longer time to register, we will expose a new configuration registerationTTL. We will allow users to configure their own TTL without presets

```yaml
spec:
  registerationTTL:  30m
```

#### Evaluation conditions 

1. When evaluating liveness, check if NodeClaim contains a registerationTTL if so utilize that TTL instead of the default

### Option 2 (Preferred):
#### Introduce a configuration registeration TTL to Node Pool then propagate to node claim

We would introduce the new argument in Node Pool and perform similar propagation of the argument to Node Claim

#### Concerns

Although we already attempt to access the node pool for node pool registration health I don't believe this argument fits in Node Pools as it is some what tied to the way nodes are configured 
which seems to be a NodeClass property rather than Node Pool
