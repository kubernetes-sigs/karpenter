# RFC: Degraded NodePool Status Condition

## Overview

Karpenter can launch nodes with a NodePool that will never join a cluster when a NodeClass is misconfigured.

One example is that when a network path does not exist due to a misconfigured VPC (network access control lists, subnets, route tables), Karpenter will not be able to provision compute with that NodeClass that joins the cluster until the error is fixed. Crucially, this will continue to charge users for compute that can never be used in a cluster.

To improve visibility of these possible failure modes, this RFC proposes the addition of a `Degraded` status condition that indicates to cluster admins there may be a problem with a NodePool needs to be investigated and corrected.

## Options

### [Recommended] Option 1: Introduce a Generalized `Degraded` Status Condition

```
// ConditionTypeDegraded = "Degraded" condition indicates that a misconfiguration exists that prevents the normal, successful use of a Karpenter resource
ConditionTypeDegraded = "Degraded"
```

This option would set `Degraded: true` on a NodePool whenever Karpenter suspects something is wrong with the launch path but isn't sure. In this case, if 3 or more NodeClaims fail to launch with a NodePool then the NodePool will be marked as degraded. The retries are included to account for transient errors. The number of failed launches is stored as a status on the NodePool and then reset to zero following an edit to the NodePool or a sufficient amount time has passed.

```
// NodePoolStatus defines the observed state of NodePool
type NodePoolStatus struct {
	// Resources is the list of resources that have been provisioned.
	// +optional
	Resources v1.ResourceList `json:"resources,omitempty"`
	// FailedLaunches tracks the number of times a nodepool failed before being marked degraded
	// +optional
	FailedLaunches int `json:"failedLaunches,omitempty"`
	// Conditions contains signals for health and readiness
	// +optional
	Conditions []status.Condition `json:"conditions,omitempty"`
}
```

Once a NodePool is `Degraded`, it recovers with `Degraded: false` after an update to the NodePool or when the NodeClaim registration expiration TTL (currently 15 minutes) passes since the `lastTransitionTime` for the status condition on the NodePool, whichever comes first. A `Degraded` NodePool is not passed over when provisioning and may continue to be chosen during scheduling. A successful provisioning could also remove the status condition but this may cause more apiserver and metric churn than is necessary.

As additional misconfigurations are handled, they can be added to the `Degraded` status condition and the `Degraded` controller expanded to handle automated recovery efforts. This is probably most simpoly achieved by changing the Status Condition metrics to use comma-delimiting for `Reason`s with the most recent change present in the `Message`.

```
  - lastTransitionTime: "2024-12-16T12:34:56Z"
    message: "FizzBuzz component was misconfigured"
    observedGeneration: 1
    reason: FizzBuzzFailure,FooBarFailure
    status: "True"
    type: Degraded
```

This introduces challenges when determining when to evaluate contributors to the status condition but since the `Degraded` status condition only has a single contributor this decision can be punted. When the time comes to implement the multiple contributors to this status condition, this probably looks like a `Degraded` controller which acts as a "heartbeat" and evaluates each of the contributors.

Finally, this status condition would not be a precondition for NodePool `Readiness` because the NodePool should still be considered for scheduling purposes.

#### Considerations

1. 👎 Three retries can still be a long time to wait on compute that would never succeed
2. 👍 Karpenter continues to try and launch with other potentially valid NodePools
3. 👍 Observability improvements so that users can begin triaging misconfigurations
4. 👍 `Degraded` is not a pre-condition for NodePool readiness

### Option 2: Expand `Validated` Status Condition and Use Reasons

The implementation is roughly the same except that validation is a pre-condition for `Readiness`. This has impact in a larger portion of the code because `Validated` would no longer block provisioning or `Readiness`. However, it is still an option that Karpenter could expand the `Valdiated` status condition so that any time a misconfiguration is encountered, the NodePool is treated as having failed validation.

#### Considerations

1. 👎👎 Validation implies the original state of the NodePool was correct and is something Karpenter can vet with certainty. A NodePool could have been correctly validated but then degraded.
2. 👎👎 Changes the meaning of `Validated` in terms of `Readiness`
2. 👎 Relies on statuses that were not part of the original validation of a NodePool.
3. 👍 Limits changes to customer-facing APIs.

### Further Dicussion Needed

Should this status condition be used to affect Karpenter functionality/scheduling or should it only exist to improve observability? For example, NodePools with this status condition could be seen as having a lower weight than normal so that other NodePools are prioritized. This is probably more surprising than not for most users and should not be considered pursuing.
