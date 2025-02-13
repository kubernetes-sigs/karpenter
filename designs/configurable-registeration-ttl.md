# RFC: Configurable registeration TTL

## Motivation

[Issue](https://github.com/kubernetes-sigs/karpenter/issues/357)

Karpenter currently hardcodes [registeration TTL](https://github.com/kubernetes-sigs/karpenter/blob/main/pkg/controllers/nodeclaim/lifecycle/liveness.go#L39) which means nodes must take < 15 minutes to come up and register with the Kubernetes control plane.

This is problematic because some users have nodes that take longer that time to register. As mentioned in the issue, nodes that run GPU can often take > 15 minutes to start up. 

## Introduce a configuration registeration TTL to NodeClass

We will keep a default for all nodes at 15 minute TTL however in the cases that the NodeClass would be used with GPUs or known to take a longer time to register, we will expose a new configuration registerationTTL.

```yaml
spec:
  registerationTTL:  30m
```

Evaluation conditions - 

1. When evaluating liveness, check if NodeClass ref of NodeClaim contains a registerationTTL
2. If it does, use that TTL instead of the default 15 minutes to evaluate if registeration TTL has been breached
