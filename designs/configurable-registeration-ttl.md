# RFC: Configurable registeration TTL

## Motivation

[Issue](https://github.com/kubernetes-sigs/karpenter/issues/357)

Karpenter currently hardcodes [registeration TTL](https://github.com/kubernetes-sigs/karpenter/blob/main/pkg/controllers/nodeclaim/lifecycle/liveness.go#L44) which means nodes must take < 15 minutes to come up and register with the Kubernetes control plane.

This is problematic because some users have nodes that take longer that time to register. As mentioned in the issue, nodes that run GPU can often take > 15 minutes to start up. 


## Configuration
### Option 1:
#### Introduce a configuration registeration TTL to NodeClass

We will keep a default for all nodes at 15 minute TTL however in the cases that the NodeClass would be used with GPUs or known to take a longer time to register, we will expose a new configuration registerationTTL.

```yaml
spec:
  registerationTTL:  30m
```

#### Evaluation conditions 

1. When evaluating liveness, check if NodeClass ref of NodeClaim contains a registerationTTL
2. If it does, use that TTL instead of the default 15 minutes to evaluate if registeration TTL has been breached

#### Concerns

- We don't evaluate NodeClass within nodeclaim liveness, that means we would need to now obtain the NodeClass then evaluate registeration TTL. Note that we now do access Node Pool for
Node registration health https://github.com/kubernetes-sigs/karpenter/blob/main/pkg/controllers/nodeclaim/lifecycle/liveness.go#L80 which previously wasn't accessed within NodeClaim
- If Node Class has been updated, we would read the newer TTL vs old (BAD)

### Option 1.5 (Preferred):
#### Introduce a configuration registeration TTL to NodeClass and propagate it to the NodeClaim

Similar to option 1, we would set the TTL in NodeClass but rather than accessing the NodeClass we would propagate this to the NodeClaim allowing us to access the argument within the NodeClaim

### Option 2:
#### Introduce a configuration registeration TTL to Node Pool then propagate to node claim

We would introduce the new argument in Node Pool and perform similar propagation of the argument to Node Claim

#### Concerns

Although we already attempt to access the node pool for node pool registration health I don't believe this argument fits in Node Pools as it is some what tied to the way nodes are configured 
which seems to be a NodeClass property rather than Node Pool

I personally prefer option 1.5 for location of the new configuration

## Free value vs preset
We may want to limit the TTL control such that user doesn't and full reign on what the TTL could be. Rather than letting the user set a value, we could change 
the arguement to `longRegisteration` limit it to just true and false where the default is false. We would allow longRegisteration to be 30 minutes.

I don't mind this however we should ask our users if they have a reason to set a specific TTL and if 30 minutes is a reasonable time for bootstrapping.
