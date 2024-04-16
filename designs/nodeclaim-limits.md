# NodeClaim Limits

**NodeClaim**: Karpenter’s representation of a Node request. A NodeClaim declaratively describes how the node should look, including instance type requirements that constrain the options that the Cloud Provider can launch with. For the AWS Cloud Provider, the instance requirements of the NodeClaim translate to the requirements of the CreateFleet request. More details can be found [here](https://github.com/aws/karpenter-provider-aws/blob/main/designs/node-ownership.md).

WIP PR: https://github.com/kubernetes-sigs/karpenter/pull/1151

## Motivation

The Karpenter NodePool Spec allows for specifying [provisioning limits](https://karpenter.sh/docs/concepts/nodepools/#speclimits) which can be used to limit based on any resource type provided by the cloudprovider or device plugins.  If a limit is exceeded, node provisioning is prevented until more resources become available (typically in the form of node terminations).

This works for users who are purely concerned about limiting resource usage within a NodePool, but doesn't work for specifying a global limit across all NodePools or for limiting a higher level representation of usage such as the number of nodes in a cluster (without also reducing flexibility of instance types).

## Use-Cases
1. I'm using a shared network with limited IP pools and need to ensure Karpenter's usage doesn't consume too many IPs. Each Node consumes a similar number of IPs but low-level resource usage for things like CPU/Memory differs between instance types.
2. I have a special NodePool (for example a set of high performance nodes or AWS Reserved Instances) with a finite amount of compute available. I need a way to prevent Karpenter from launching too many nodes of this type. The only to do this is by calculating against an available resource like number of CPU's and adding this as a limit. This doesn't target the actual limiter which is number of instances rather than resource consumption.
3. My CNI can't support more than 400 nodes but I'm not concerned about underlying resource usage. I have no way to restrict Karpenter to this limit without severly limiting instance type options.

Currently, Karpenter users face a limitation in controlling the maximum number of nodes provisioned by Karpenter. This constraint hinders operations in environments where the CNI in use can't scale beyond a certain number of nodes or in scenarios involving shared networks with a finite IP address allocation. Some additional use cases and discussion on this limitation can be seen in the following issues: (#732, aws/karpenter-provider-aws#4462).

## Proposal

### New controller flag `max-node-claims` and per-NodePool limits in the `spec.limits` section of NodePool template

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
2. 👎 May be used as a crutch vs having the resource provider supporting the relevant resources. An example here could be a CNI or cloudprovider sharing information on IP usage.