# NodeOverlay Integration for Dynamic Resource Allocation

## TODO

- [ ] Concrete schema example
- [ ] NodeOverlay conflict resolution definition
- [ ] Integration with NodeClaim initialization check

## Background

When Karpenter constructs a NodeClaim for a given set of pods, it uses information from the CloudProvider to determine
the resources that will exist on a Node if one is created for a given instance type. That is then used to determine
which instance types will satisfy a given set of requests. CloudProviders can get this information from any number of
sources, for example EC2's `DescribeInstanceTypes` API, and they use their knowledge of well-known device plugins to
determine the resulting Node's allocatable resources. The drawback of this approach is that each CloudProvider needs to
build integrations for specific drivers, and users are limited to those integrations. `NodeOverlays` were introduced to
close that gap by allowing end users to explicitly inform Karpenter of the resources that will be made available for a
given instance type. For example, the following `NodeOverlay` will add the `smarter-device/fuse` device to `c5.large`
instances from the NodePool `fuse-np`.

```yaml
kind: NodeOverlay
apiVersion: karpenter.sh/v1alpha1
spec:
  requirements:
  - key: karpenter.sh/nodepool
    operator: In
    values: ['fuse-np']
  - key: node.kubernetes.io/instance-type
    operator: In
    values: ['c5.large']
  capacity:
    smarter-device/fuse: 1
```

We face the same challenge with DRA: CloudProviders will need to be made aware of the `ResourceSlices` that will be
published when a given instance type registers with the cluster as a node. This RFC proposes extending the existing
NodeOverlay API to support specifying `ResourceSlices`  in addition to conventional extended resources.

## Non-goals
- Support for provisioning devices which aren't associated directly with a node
	- As a node autoscaler, I would consider this out-of-scope for Karpenter initial support. The integration in Karpenter's
      scheduler should be able to support any `ResourceSlice` which has already been provisioned, but that's a separate
      matter from supporting the provisioning of new devices.

## General API Shape

At a high level, we will want to add a new field to `NodeOverlay`, `resourceSlices`, which will be a list of
`ResourceSlices` to associate with any matching instance types. This wouldn't be a breaking change to the API, so it
would remain `v1alpha1`.


There is some nuance to they type of the `resourceSlices` field. There are three options:
- A list of unstructured objects
- The upstream v1 schema
- A custom `ResourceSlice` schema

A list of unstructured objects has the advantage of not tying the CRD to a particular version of the `ResourceSlice`
schema. We have considered changing other fields to accept an unstructured object, namely `spec.kubelet` and `spec.userData`
(Bottlerocket) on the `EC2NodeClass`, for this reason. However, the advantages it provides for those fields don't extend to
`ResourceSlices`. Both of those fields are passed through umutated to a separate process, Kubelet and the Bottlerocket
bootstrapper respectively. Karpenter is not coupled to a particular version of either of those processes. Since Karpenter
won't be passing through this `ResourceSlice` object to any other process and is processing it itself there isn't an
advantage to decoupling the version of the schema.

Using the upstream schema is the next obvious solution. Users are already familiar with this schema, and it simplifies
the onboarding process by allowing existing manifests to be copied into the `NodeOverlay`. However, this also comes with
some notable drawbacks:
- Karpenter won't support all of the fields that are present in the upstream schema, for example those backed by alpha
  features.
- Some of the fields are redundant, e.g. `spec.nodeName`, since it will be implicitly associated with the instance
  that's launched. Additionally, in this case it's impossible to know the concrete value ahead of launch.
- Some of the required fields don't provide useful information for Karpenter, e.g. `spec.pool`. Requiring users to
  configure these values would be confusing, so we would minimally need to change CEL validation rules.

For these reasons, my recommendation is that we make a custom type in Karpenter. This custom `ResourceSlice` type will
only exist as part of the `NodeOverlay` schema and it will only surface the fields supported by Karpenter. This helps to
self-document the subset of the DRA feature-set that's supported on a given Karpenter version. Additionally, this gives
us the flexibility to add additional field when necessary (as proposed in "Supporting Relative Topology Requirements").

```yaml
kind: NodeOverlay
apiVersion: karpenter.sh/v1alpha1
spec:
  # ...
  resourceSlices: []ResourceSliceTemplate
```

### Supported Fields

The following principles should be used to determine if fields should be included in Karpenter's `ResourceSlice` schema:
- Alpha features should be excluded
- Only fields which impact Karpenter's scheduling simulation should be included

The following fields from the `v1` API in Kubernetes 1.34 would be excluded:

| Field                                     | Rationale                                                                                                                                                                                |
| ----------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `spec.nodeName`                           | The concrete value for this field can't be known when creating the template. If no other selector is specified in the template, we should assume the `ResourceSlice` will be node local. |
| `spec.pool`                               | Irrelevant to Karpenter's scheduling simulation                                                                                                                                          |
| `spec.sharedCounters`                     | Alpha                                                                                                                                                                                    |
| `spec.devices[].bindingConditions`        | Alpha. Shouldn't be required even when the feature goes beta / GA since it's not relevant to instance type selection.                                                                    |
| `spec.devices[].bindingFailureConditions` | Alpha. Shouldn't be required even when the feature goes beta / GA since it's not relevant to instance type selection.                                                                    |
| `spec.devices[].bindsToNode`              | Alpha. Shouldn't be required even when the feature goes beta / GA since it's not relevant to instance type selection.                                                                    |
| `spec.devices[].capacity`                 | Alpha                                                                                                                                                                                    |
| `spec.devices[].consumesCounters`         | Alpha                                                                                                                                                                                    |
| `spec.devices[].taints`                   | Alpha                                                                                                                                                                                    |

## Supporting Relative Topology Requirements

One of the goals is that we should be able to express **relative requirements**. When Karpenter creates a NodeClaim object, the topology of the final Node hasn't been resolved yet. For example, a NodeClaim may be compatible with three zones and it is up to the cloudprovider implementation to select one of those zones. In the AWS provider, this is dictated by `CreateFleet`.

The challenge this poses with DRA is simple: if a ResourceSlice has a requirement that is relative to it's host (the instance Karpenter launches), we need a way for users to express these requirements. We'll need some way for the user to express that they don't know they don't know the concrete value, but it will be the same value as the created NodeClaim. There is some prior art for this scenario: `matchLabelKeys` for pod affinity and topology spread constraints ([ref](https://kubernetes.io/blog/2024/08/16/matchlabelkeys-podaffinity/)). The motivating example for this feature was that users wanted to specify requirements based on `pod-template-hash`, the concrete value of which isn't resolved until the pods are created. This is more or less the same scenario, and I recommend we mirror that API direction:

```yaml
kind: 'NodeOverlay'
apiVersion: 'karpenter.sh/v1alpha1'
spec:
  resourceSlices:
  - kind: 'ResourceSlice'
    apiVersion: 'resource.k8s.io/v1'
    spec:
	  nodeSelector:
	    # nodeSelectorTerms is now optional
		matchLabelKeys:
		- 'topology.kubernetes.io/zone'
```

Another option I had been considering is using CEL in the existing API definition, but I believe the previous approach has a few key advantages:
- It limits the scope of the feature, reducing possible edge conditions
- There's prior art in the k8s API so users are already familiar with the semantic
The main disadvantage is that it increases the schema diff from the upstream `ResourceSlice` API, but this does not outweigh the advantages. This an example of what that may have looked like:
```yaml
kind: NodeOverlay
apiVersion: karpenter.sh/v1alpha1
spec:
  resourceSlices:
  - kind: ResourceSlice
    apiVersion: resource.k8s.io/v1
    spec:
	  nodeSelector:
	    nodeSelectorTerms:
	    - matchFields:
	        topology.kubernetes.io/zone: "${resolvedLabel('topology.kubernetes.io/zone')}"
```

This decision is also not a one-way door. If a use-case presents itself where the flexibility of CEL based selectors is required we can add them at that time. However, the flexibility they provide is not required for current use-cases.

Scheduling semantics of matchLabelKeys:
- No difference while all pods are "bound" to the same NodeClaim since all pods are guaranteed to share the same domain. For example, if we have a requirement on zone the NodeClaim's zonal requirement will still be compatible with the set of zones from compatible instance types.
- As soon as we have a pod that lands on a different node that consumes the same resource slice, we collapse that requirement to a single value. This value needs to be part of the set intersection of the two hosts. We can choose randomly, but we'll likely want to add some additional logic to choose the domain which minimizes cost (primary) and maximizes flexibility (secondary).
	- Alternatively, we could introduce a concept of "linked requirements". This would essentially be a way to say "these two NodeClaims can be launched in any of these domains, but they must be launched in the same domain." The advantage of this is that it could theoretically reduce pod creation to ready latency by minimizing the number of scheduling simulations / NodeClaim creations required to find a satisfiable solution. That being said, this is probably offset by the additional complexity that would need to be added to the launch path of the cloudproviders (e.g. multiple batched CreateFleet calls or the need to launch serially for AWS)
- In Karpenter's DRA allocator we'd have the concept of a "linked" ResourceSlice. This would be any ResourceSlice which is non-node local but has dependent topology requirements. When evaluating it's NodeSelector, we would evaluate the current requirements for the linked NodeClaim.


## Scratch Notes (WIP)

## Feature Gate Summary
- Admin Access (Beta)
- Consumable Capacity
- Device Binding Conditions
- Device Taints
- Extended Resource
- Partitionable Devices
- Prioritized List (Beta)
- ResourceClaim Device Status
- Scheduler Filter Timeout


```go
// InstanceType describes the properties of a potential node (either concrete attributes of an instance of this type
// or supported options in the case of arrays)
// +k8s:deepcopy-gen=true
type InstanceType struct {
	// Name of the instance type, must correspond to corev1.LabelInstanceTypeStable
	Name string
	// Requirements returns a flexible set of properties that may be selected
	// for scheduling. Must be defined for every well known label, even if empty.
	Requirements scheduling.Requirements
	// Note that though this is an array it is expected that all the Offerings are unique from one another
	Offerings Offerings
	// Resources are the full resource capacities for this instance type
	Capacity corev1.ResourceList
	// Overhead is the amount of resource overhead expected to be used by kubelet and any other system daemons outside
	// of Kubernetes.
	Overhead               *InstanceTypeOverhead
	once                   sync.Once
	allocatable            corev1.ResourceList
	capacityOverlayApplied bool
}

// to

type NodeShape interface {
	ID() string
	Matches(*corev1.Node) bool

	Requirements() scheduling.Requirements
	Allocatable() corev1.ResourceList
	// Provides the custom Karpenter ResourceSlice. The upstream Karpenter
	// implementation will decorate the cloudprovider provided slices to
	// inject values from NodeOverlay
	ResourceSlices() v1.ResourceSlice

	Offerings() []Offering
}
```gg
