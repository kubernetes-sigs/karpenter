# Implementing a Karpenter Cloud Provider

Karpenter core decides when to launch, replace and remove nodes. A cloud provider supplies the cloud-specific half: creating and deleting instances, describing what can be launched, and bringing up Nodes that core can adopt. This document covers what a provider has to do beyond satisfying the `CloudProvider` interface in [`types.go`](./types.go).

The KWOK provider in [`kwok/`](../../kwok) is a small reference implementation. The fake provider in [`fake/`](./fake) is what core's own tests use.

## What core owns and what the provider owns

Core owns the NodePool and NodeClaim APIs, scheduling and bin-packing, and the controllers that drive a NodeClaim through launch, registration, initialization, liveness, garbage collection, disruption, repair and termination.

A provider owns everything that touches the cloud or the node image:

- Creating, deleting, listing and looking up instances.
- Instance type and offering data, including price and availability.
- The NodeClass API and its `Ready` condition.
- Node bootstrap: registering a Node with the right provider ID, adding the `karpenter.sh/unregistered` taint, and removing any startup taints.
- Repair policies and provider-side drift detection.
- Any provider-specific labels.

## Wiring a provider into core

[`kwok/main.go`](../../kwok/main.go) is the template. In order:

1. Register the supported capacity types with `cloudprovider.SetCapacityTypeClassResolver`. Call it before any controller runs. It also sets the valid values of the `karpenter.sh/capacity-type` requirement.
2. Optionally call `cloudprovider.SetNodeClaimPriceProvider` if a launched NodeClaim can't be priced from a single offering.
3. Create the operator with `operator.NewOperator()`, then build the provider.
4. Wrap the provider with `overlay.Decorate`, which applies NodeOverlays to the instance types it returns.
5. Build the repair policy matcher with `health.NewRepairPolicyMatcher` and the cluster state with `state.NewCluster`.
6. Pass the decorated provider and the undecorated provider to `controllers.NewControllers`. Options such as `controllers.WithRegistrationHook` add behavior.

## The CloudProvider interface

What core does with each method:

- `Create` launches an instance and returns a hydrated NodeClaim. Core copies the returned labels, annotations, `ProviderID`, `ImageID`, `Capacity` and `Allocatable` onto the NodeClaim and sets `Launched`. Labels already on the NodeClaim win over returned ones.
- `Delete` returns nil once deletion is triggered and `NodeClaimNotFoundError` once the instance is gone. Core calls it repeatedly until it gets `NodeClaimNotFoundError`, so it must be safe to call more than once.
- `Get` looks up a NodeClaim by provider ID. When a Node that isn't `Ready` is being terminated and `Get` returns `NodeClaimNotFoundError`, core skips the drain and removes the Node's finalizer, since no instance is left.
- `List` returns the NodeClaims the provider has launched. The garbage collector deletes a registered NodeClaim whose provider ID is missing from `List`, unless its Node is still `Ready`. A `List` that omits a live instance whose Node isn't `Ready` gets that NodeClaim deleted.
- `GetInstanceTypes` returns every instance type for a NodePool, including types with no available offerings. Scheduling and consolidation read requirements, offerings and overhead from it.
- `IsDrifted` returns a `DriftReason`, or an empty value when the NodeClaim hasn't drifted. Core checks drift first against the NodePool's static-field hash and requirements, then checks that the NodeClaim's instance type and offering still exist in `GetInstanceTypes`, and only then calls `IsDrifted`.
- `Reboot` restarts the instance in place. Return `NodeRebootNotImplementedError` if the provider can't, or `NodeRebootFailedError` if retrying can't help. Node repair uses it.
- `RepairPolicies` returns the full static set of repair policies.
- `GetSupportedNodeClasses` returns the provider's NodeClass types, with the default first.

### Errors from Create

Core branches on the error type in [`launch.go`](../controllers/nodeclaim/lifecycle/launch.go):

- `InsufficientCapacityError`: the NodeClaim is deleted and an event is published for each pod that was scheduled to it.
- `NodeClassNotReadyError`: the NodeClaim is deleted.
- `CreateError`: `Launched` is set to Unknown with the error's reason and message, and the launch is retried.
- Any other error: `Launched` is set to Unknown with reason `LaunchFailed`, and the launch is retried.

### Instance types and offerings

Core sends at most 600 instance types for launch (`MaxInstanceTypes` in `nodeclaimtemplate.go`), so a NodeClaim passed to `Create` carries no more than that many.

Every `InstanceType.Requirements` must define each well-known label, even if empty. Every `Offering` must carry the capacity-type and zone requirements. The doc comments on `Offering` in `types.go` define `Available`, `Compatible`, `ReservationCapacity` and `Launchable`.

## Nodes

Core matches a Node to a NodeClaim by provider ID. The instance has to register a Node whose `spec.providerID` equals the NodeClaim's `status.providerID`.

- **Registration.** Core finds the Node, syncs the NodeClaim's labels, annotations, taints, finalizer and owner references onto it, and sets `Registered`. The provider is responsible for adding the `karpenter.sh/unregistered` taint to the Nodes it creates, so it is present when the Node first registers. Core logs an error and continues if it is missing, but the taint protects against races during registration. Core removes it when registration completes.
- **Timeouts.** A NodeClaim that hasn't launched within 5 minutes of its first failed launch, or whose Node hasn't registered within 15 minutes, is deleted and the work is retried (`liveness.go`).
- **Registration hooks.** `controllers.WithRegistrationHook` takes a `NodeLifecycleHook`. Registration finishes only when every hook returns an empty result.
- **Initialization.** `Initialized` is set when the Node is `Ready`, the NodeClaim's startup taints and the known ephemeral taints are gone, every resource the NodeClaim requests has a non-zero allocatable on the Node, and, when DRA is enabled, every requested DRA driver has published a complete pool. Core only waits for startup taints to disappear. Something running on the node has to remove them.

## NodeClasses

The provider defines its NodeClass CRDs. Each type implements `status.Object` and reports a `Ready` condition. Core watches every type returned by `GetSupportedNodeClasses` and sets the NodePool's `NodeClassReady` condition from it. A NodePool whose NodeClass is missing, terminating or not `Ready` isn't used for provisioning.

## Repair policies

When node repair is enabled, `RepairPolicies` must return at least one policy and the set is validated at startup. Exactly one policy must have an empty `ReasonRegex`, which is the default fallback. See `RepairPolicy` in `types.go` for the remaining fields.

## Labels

Labels a provider adds to NodePool requirements are inserted into `v1.WellKnownLabels` at startup, as in [`kwok/cloudprovider/helpers.go`](../../kwok/cloudprovider/helpers.go).

## Testing

Core's own tests use `fake.NewCloudProvider()`. For running Karpenter's integration suites against a provider, see [Integrating Karpenter Tests for Cloud Providers](../../test/README.md#integrating-karpenter-tests-for-cloud-providers).
