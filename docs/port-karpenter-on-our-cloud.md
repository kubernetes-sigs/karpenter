# Porting Karpenter to your cloud provider

## Pre-requisites

Karpenter requires your to implement an interface to use your own cloud provider.
The interface is defined in the `pkg/cloudprovider/types.go` file.

```go
// CloudProvider interface is implemented by cloud providers to support provisioning.
type CloudProvider interface {
	// Create launches a NodeClaim with the given resource requests and requirements and returns a hydrated
	// NodeClaim back with resolved NodeClaim labels for the launched NodeClaim
	Create(context.Context, *v1beta1.NodeClaim) (*v1beta1.NodeClaim, error)
	// Delete removes a NodeClaim from the cloudprovider by its provider id
	Delete(context.Context, *v1beta1.NodeClaim) error
	// Get retrieves a NodeClaim from the cloudprovider by its provider id
	Get(context.Context, string) (*v1beta1.NodeClaim, error)
	// List retrieves all NodeClaims from the cloudprovider
	List(context.Context) ([]*v1beta1.NodeClaim, error)
	// GetInstanceTypes returns instance types supported by the cloudprovider.
	// Availability of types or zone may vary by nodepool or over time.  Regardless of
	// availability, the GetInstanceTypes method should always return all instance types,
	// even those with no offerings available.
	GetInstanceTypes(context.Context, *v1beta1.NodePool) ([]*InstanceType, error)
	// IsDrifted returns whether a NodeClaim has drifted from the provisioning requirements
	// it is tied to.
	IsDrifted(context.Context, *v1beta1.NodeClaim) (DriftReason, error)
	// Name returns the CloudProvider implementation name.
	Name() string
}
```

## Implementing the CloudProvider interface

### Create method

The `Create` method will receive a NodeClaim object and must implement the logic to
create a compute ressource which will trigger a `Node` object creation in your cluster.
For example, it can create a virtual machine on your cloud provider which will auto-register
in your Kubernetes cluster.

To have NodeClaim properly hydrated, you must set the `.status.providerID` and
`.metadata.labels` fields on the NodeClaim object.

Returned NodeClaim must be a new object and not an updated version of the input NodeClaim.

To ensure Karpenter works properly, `Node` object must have the `.spec.providerID` field set, else
Karpenter won't match it and will recreate a new one. If you don't use cloud-controller-manager or
the one you use don't handle this field, you should create a `Node` object with the `.spec.providerID`
directly in this call.

A good idea, if your cloud provider support it, is to add a tag related to cluster context on the cloud
provider created ressource in order to be able to filter them out in the `List` method
(ie. `karpenter.sh/cluster-name` tag).

### Delete method

`Delete` method will receive a NodeClaim object and must implement the logic to Terminate a compute
ressource. You don't need to manage the `Node` object, Karpenter engine will do it for you.

### Get method

`Get` method is simply the way to map back a cloud provider ressource to a Karpenter NodeClaim object using
its `providerID`. You must return a NodeClaim object with the `.status.providerID` properly set.

### List method

`List` method must return all the NodeClaim objects that are currently managed by your cloud provider.
If your cloud provider has a way to filter out the resources, by a label or a tag, don't hesitate to do it.
Some cloud providers are using pagination, be careful. Don't forget to populate the `.status.providerID` field.

### GetInstanceTypes method

`GetInstanceTypes` method must return all the instance types supported by your cloud provider. It's very
cloud provider tied.

If you are able to set any CPU/memory amount on your cloud provider, it can be
a good idea to generate dynamically the list of allowed instance types Karpenter can use. Those instances can have
a pricing weight, so Karpenter can choose the best instance type to optimize costs.

### IsDrifted method

`IsDrifted` method returns whether a NodeClaim has drifted from the provisioning requirements it is tied to.
It can be a check that the underlying compute resource use a up to date image, or that the instance type is still
available. You can use any healthchecking method defining how it drifted.

### Name method

It's just your cloud provider name. Any hardcoded returned string will do the trick.

