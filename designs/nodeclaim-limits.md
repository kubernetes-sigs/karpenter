# NodeClaim Limits

**NodeClaim**: Karpenter’s representation of a Node request. A NodeClaim declaratively describes how the node should look, including instance type requirements that constrain the options that the Cloud Provider can launch with. For the AWS Cloud Provider, the instance requirements of the NodeClaim translate to the requirements of the CreateFleet request. More details can be found [here](https://github.com/aws/karpenter-provider-aws/blob/main/designs/node-ownership.md).

## Motivation

The Karpenter NodePool Spec allows for specifying [provisioning limits](https://karpenter.sh/docs/concepts/nodepools/#speclimits) which can be used to limit based on any resource type provided by the cloudprovider.  If a limit is exceeded, node provisioning is prevented until more resources become available (e.g. node terminations).

This works for users who are purely concerned about limiting resource usage within a NodePool, but doesn't work for specifying a global limit across all NodePools or for limiting a higher level representation of usage such as the number of nodes in a cluster (without also reducing flexibility of instance types).

## Use-Cases
1. I'm using a shared network with limited IP pools and need to ensure Karpenter's usage doesn't consume too many IPs. Each Node consumes a minimum number of IPs but low-level resource usage for things like CPU/Memory differs between instance types.
2. I have a special NodePool (for example a set of high performance nodes or AWS Reserved Instances) with a finite amount of compute available. I need a way to prevent Karpenter from launching too many nodes of this type. The only to do this is by calculating against an avialable resource like number of CPU's and adding this as a limit. This doesn't target the actual limiter which is number of instances rather than resource consumption.
3. My CNI can't support more than 400 nodes but I'm not concerned about resource usage. I have no way to restrict Karpenter to this limit without severly limiting instance type options.


Currently, Karpenter users face a limitation in controlling the maximum number of nodes provisioned by Karpenter. This constraint hinders operations in environments where the CNI in use can't scale beyond a certain number of nodes or in scenarios involving shared networks with a finite IP address allocation. Some additional use cases and discussion on this limitation can be seen in the following issues: (#732, aws/karpenter-provider-aws#4462).

## Solutions

### [Recommended] Solution 1: New controller flag `max-node-claims` and per-NodePool limits in the `spec.limits` section of NodePool template

1. A new controller startup flag `max-node-claims` which would allow for setting a global cap on the number of nodes provisioned via this Karpenter controller.
2. Allow for setting per-NodePool limits on number of NodeClaims. 

```
--max-node-claims="-1" Usage: The maximum allowed number of NodeClaims that if exceeded will stop additional creations. Negative values are treated as unlimited and is the default.
```

```
apiVersion: karpenter.sh/v1beta1
kind: NodePool
spec:
  template:
    spec:
        limits: 
            nodes: 10
```

#### Pros/Cons

1. 👍👍 Allows for simple cap on number on NodeClaims created by Karpenter, allowing users to apply this globally and per-NodePool
2. 👎 May be used as a crutch vs having the cloudprovider providing the relevant resources. An example here could be supporting a limit for IP usage.

### Solution 2: Suggest users request/implement reporting of resource types in CloudProvider

In this solution, the community would need to update Cloud Providers to collect and report on resource usage of the desired types.

#### Pros/Cons

1. 👍👍 For use cases like operating in shared/limited IP pools would address the underlying limitation head-on rather than a higher level 
2. 👍 Allows for configuring limits on very granular resource usage.
3. 👎 Given the flexibility of needs and variance of tooling choices like CNIs this would probably be a signifigant effort across many providers.
4. 👎 Even with the more granular resources exposed, some users may just want a simpler configuration so is missing some of the value adding a NodeClaim limit would provide.