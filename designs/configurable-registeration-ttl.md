# RFC: Configurable MaxNodeProvisionTime

## Motivation

[Issue](https://github.com/kubernetes-sigs/karpenter/issues/357)

Karpenter currently hardcodes [registration TTL](https://github.com/kubernetes-sigs/karpenter/blob/main/pkg/controllers/nodeclaim/lifecycle/liveness.go#L44) which means nodes must take < 15 minutes to come up and register with the Kubernetes control plane.

This is problematic because some users have nodes that take longer that time to register. As mentioned in the issue, nodes that run GPU can often take > 15 minutes to start up. 
Use cases:
- GPU driver installation during node bootstrap
- Windows AMIs security tooling installation


## Non-Goal
Setting cloud provider defaults are not within the scope of this proposal/ RFC

## Configuration

### Option 1 (Preferred):
#### Introduce a configuration registeration TTL as MaxNodeProvisionTime to Node Pool then propagate to node claim

We propose introducing a new argument in the Node Pool and propagating it to the NodeClaim during its creation. When the liveness controller evaluates the maxNodeProvisionTime, it will use the value specified in the NodeClaim.

Importantly, even if the maxNodeProvisionTime is updated on the NodePool during NodeClaim registration, we will continue to use the NodeClaim's maxNodeProvisionTime. This approach ensures clarity by demonstrating that the NodeClaim relies on variables defined in its own spec.

Furthermore, this variable will not be considered when evaluating drift, as it is only utilized during the registration process. If a node requires a longer TTL, it would have already been terminated by Karpenter. In cases where the node successfully registers, the TTL becomes irrelevant. Consequently, changes to this variable do not necessitate updates to the drift evaluation process.

Lastly, we will move maxNodeProvisionTime to fully be in NodePool CRD and default it to 15m.

```yaml
apiVersion: karpenter.sh/v1
kind: NodePool
metadata:
  name: example-nodepool
spec:
  template:
    metadata: {}
    spec:
      expireAfter: 288h0m0s
      maxNodeProvisionTime: 20m # New variable
```

#### Concerns

N/A

### Option 2:
#### Introduce a configuration maxNodeProvisionTime to NodeClass and propagate it to the NodeClaim

We will keep a default for all nodes at 15 minute TTL however in the cases that the NodeClass would be used with GPUs or known to take a longer time to register, we will expose a new configuration maxNodeProvisionTime. We will allow users to configure their own TTL without presets

#### Evaluation conditions 

1. When evaluating liveness, check if NodeClaim contains a maxNodeProvisionTime if so utilize that TTL instead of the default.

### Option 3:
#### Leveraging NodeOverlay 

We could enable this feature via Node Overlay. However the issue is it isn't always the case that a instance type specifically is causing issues but rather a instance type running a specific AMI. This makes it difficult to use NodeOverlay to change the maxNodeProvisionTime. 

## Future Investigation/ Discussion 
### Cluster level flag

Cluster level flag: Cluster Autoscaler has this, and it wasn't enough, users ended up adding a nodepool level configurable flag.

Nodepool level flag: As different nodepools have different expectations and requirements around registration, nodepool level makes more sense and avoids forcing one-size-fits-all behavior.

We should consider allowing both a Cluster level flag with nodepool override. This would allow Cloud providers to override max provision time. Note that at this time we don't have a good interface for Cloud provider to set / modify these kind of behaviors.

From Bryce:
```
(I know you mentioned cloudprovider override out of scope, but i would mention that as the fourth option, with the fifth being node overlay)

User stories (why this is needed)
Some notes from my experience

As different nodepools have different expectations and requirements around registration. I have seen customers set this value to 45m and to 5m, both extending provisioning time to give long P99 requests longer to finish without abandoning the call, and then separately, abandoning requests that are slower than we see on average and re-rolling. These are two things we could include as user stories for why this is needed, outside of standard GPU provisioning + bootstrapping times.

Also, different cloudproviders have different performance expectations. 15 minutes seems reasonable for most instance types + images for Azure + AWS, but maybe there are other cloudproviders that have different performance expectations.

Sometimes the node registration failures come from a source outside of Karpenter’s control. For example the kubelet is getting throttled by the apiserver, due to APIServer QPS. Deleting the underlying VM will not make the scale up goal faster in this case, it does the opposite. Some customers are willing to pay for a bigger apiserver, some customers don't have that option. Either way, it makes sense that we would want to prevent this VM GC if it’s not the VM provisioning that’s the bottleneck in registration, but rather the apiserver. Deleting and recreating adds additional pressure and churn to the apiserver that's already overloaded. This is yet another reason to let that registration be configurable by the user.
```
