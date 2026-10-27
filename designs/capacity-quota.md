# CapacityQuota: Granular Resource Limits

Support the SIG Autoscaling `CapacityQuota` API, so that resource limits can apply to any set of nodes selected by labels, not only to a NodePool.

## Motivation

Karpenter limits capacity with `NodePool.spec.limits`. A limit applies to the nodes of one NodePool, and there is no way to limit:

- the whole cluster ([AWS best practices](https://docs.aws.amazon.com/eks/latest/best-practices/karpenter.html) recommend limits per NodePool for this reason),
- a set of nodes that spans NodePools, such as the nodes of one team, one machine family, one zone or one OS,
- a scarce resource such as GPUs across all NodePools that can launch them.

SIG Autoscaling designed one API for this for both node autoscalers: [`CapacityQuota`](https://github.com/kubernetes/autoscaler/blob/master/cluster-autoscaler/proposals/granular-resource-limits.md) (`autoscaling.x-k8s.io/v1beta1`). Cluster Autoscaler implements it since Kubernetes 1.35. The proposal's GA criteria wait for a Karpenter implementation until 1.39, so that users get the same limits on either autoscaler. This RFC proposes that implementation ([#2698](https://github.com/kubernetes-sigs/karpenter/issues/2698)).

### Use Cases

1. **Cluster-wide cost cap.** An administrator caps the cluster at 512 vCPU across all NodePools, and individual NodePools can still have their own limits.
2. **Team quotas across NodePools.** Team A runs a spot NodePool and an on-demand NodePool. Their nodes carry `team: a`, and a quota of 64 vCPU applies to both NodePools together.
3. **Scarce or expensive hardware.** No more than 16 `nvidia.com/gpu` across every NodePool that can launch GPU instances.
4. **Zone or IP space protection.** No more than 64 nodes in `us-east-1a`, regardless of NodePool ([kubernetes/autoscaler#6940](https://github.com/kubernetes/autoscaler/issues/6940) asks for the same in CAS).

### Non-Goals

- Changing `NodePool.spec.limits`. They keep working as they do today.
- Minimum limits. The upstream proposal dropped them in favour of the CapacityBuffer API.
- Hard limits that also hold during consolidation. As upstream decided, CapacityQuota limits are soft, like NodePool limits today.
- Defining the `CapacityQuota` API. Karpenter consumes the upstream CRD as is.

## Proposal

### Proposed Spec

Karpenter reads the upstream object. No Karpenter API changes are needed:

```yaml
apiVersion: autoscaling.x-k8s.io/v1beta1
kind: CapacityQuota
metadata:
  name: team-a
spec:
  selector:            # optional; empty or missing selects every node
    matchLabels:
      team: a
  limits:
    resources:
      cpu: "64"
      memory: 256Gi
      nodes: "20"      # the node count, as in the upstream proposal
status:
  used:                # written by the status controller (beta)
    resources:
      cpu: "40"
      memory: 160Gi
      nodes: "11"
```

A launch is allowed only if it fits every limit that applies to it: the NodePool `spec.limits` and every CapacityQuota whose selector matches the node that would be created.

### How It Works

**Which quotas apply to a launch.** The provisioning scheduler already knows the labels a NodeClaim will have before it launches: the NodePool template labels, the single-valued requirements, and the labels of the chosen instance type and offering (`node.kubernetes.io/instance-type`, `kubernetes.io/arch`, `kubernetes.io/os`, `topology.kubernetes.io/zone`, `karpenter.sh/capacity-type` and the cloud provider's instance labels). A quota applies to a candidate *(instance type, offering)* when its selector matches the labels that candidate would produce. A selector on a label Karpenter can't know at launch (for example one added after registration by another controller) can't be checked then; see Edge Cases.

**Remaining capacity.** When the scheduler is built, it computes the remaining capacity of each quota as `limits - usage`. Usage is the summed capacity of the nodes and in-flight NodeClaims in cluster state whose labels match the selector, and the `nodes` resource counts them. This mirrors how `remainingResources` is computed for NodePools today from `cluster.NodePoolResourcesFor`. Cluster state gains a `ResourcesFor(selector)` helper for this.

**Filtering.** Today, `addToNewNodeClaim` drops instance types whose capacity exceeds the NodePool's remaining resources (`filterByRemainingResources`). For quotas the check moves down to offerings, because a quota on a zone or capacity type applies to some offerings of an instance type but not others:

1. for each instance type option, drop the offerings for which some applicable quota has less remaining capacity than the instance type's capacity (or no `nodes` left);
2. drop instance types left with no offering;
3. if no options remain, the NodeClaim can't be created from this NodePool, and the scheduler moves on to the next NodePool by weight, as it does for NodePool limits.

**In-flight accounting.** When a new NodeClaim is added in a scheduling round, the scheduler subtracts, from every quota that may select it, the largest capacity among its remaining options (`subtractMax`), as it does for NodePool limits. This keeps one scheduling round from planning more than a quota allows, at the cost of being conservative when the options differ in size.

**Status (beta).** A `capacityquota.counter` controller, modelled on the NodePool counter controller, writes `status.used` from cluster state. When Cluster Autoscaler and Karpenter run in the same cluster, only one should own `status.used`; Karpenter writes it with its own field manager and only when `--capacity-quota-status` is enabled (Open Question 3).

### Interaction with Existing Features

- **NodePool limits:** both apply; the stricter one wins for each resource.
- **Consolidation and drift:** the disruption controller simulates replacements with the provisioning scheduler, so replacements respect quotas in the same best-effort way as NodePool limits. A replacement may briefly exceed a quota while the old node is still terminating. Consolidation never deletes nodes *to satisfy* a quota, matching upstream soft-limit semantics.
- **Static capacity / repair:** the node-count checks that `repair.go` and `staticdrift.go` make against `NodePool.spec.limits[nodes]` also consider quotas with a `nodes` limit that select the node.
- **Weighted NodePools:** a quota that blocks a higher-weight NodePool makes the scheduler fall through to lower-weight NodePools, as NodePool limits do today.

### Observability

- The existing log line "instance types were excluded because they would breach limits" names the quota as well as the NodePool.
- Pod events: when no NodePool can launch because of quotas, the `FailedScheduling` event says which CapacityQuota is exhausted.
- Metrics: `karpenter_capacityquota_limit` and `karpenter_capacityquota_usage` gauges per quota and resource.
- `kubectl get capacityquota` shows `status.used` once the status controller is enabled.

### Edge Cases

- **Labels unknown at launch.** A quota selecting a label that only appears after registration is counted in usage (so it stops later launches once full), but can't stop the launch that first crosses it. The documentation will recommend selecting on NodePool template labels or well-known labels.
- **Overlapping quotas.** A node may be selected by several quotas; every one must have room.
- **Quota lower than current usage.** Nothing is deleted; no new capacity that the quota selects is launched until usage drops below the limit.
- **CRD not installed.** Karpenter checks for the `capacityquotas.autoscaling.x-k8s.io` CRD at start-up. Without it, the feature is off, as if the gate were disabled, and a log line says so.
- **Empty selector.** It selects every node, so it acts as a cluster-wide limit (Use Case 1).

## Alternatives Considered

### Alternative 1: Karpenter-specific cluster or group limits

Add a cluster-wide limit to Karpenter's options, or a Karpenter CRD for node groups. This would solve Use Case 1 quickly, but it creates a second API for the same problem, differs from Cluster Autoscaler, and blocks the upstream API from GA. Rejected in favour of the shared API.

### Alternative 2: Enforce quotas only at the NodeClaim level, after the cloud provider picks an instance

Admit a NodeClaim, then fail it if the launched instance breaks a quota. This is simpler, but it wastes launches, leaves pods pending longer, and fights with consolidation. Filtering offerings before the launch matches how NodePool limits already work.

### Alternative 3: Map quotas onto NodePool limits

Translate each quota into limits on the NodePools it could select. This fails for quotas that cross NodePools (Use Cases 1–3), where the capacity has to be shared rather than split.

## Backward Compatibility

The feature is behind the `CapacityQuota` feature gate, off in alpha. With the gate off, or without the CRD installed, behaviour is unchanged. Existing NodePool limits keep their semantics.

## Graduation Criteria

- **Alpha:** feature gate off by default; quotas enforced during provisioning and disruption simulation; unit tests for offering-level filtering and in-flight accounting.
- **Beta:** gate on by default; `status.used` controller; metrics; e2e tests for each use case; feedback from alpha users; agreed ownership of `status.used` with Cluster Autoscaler.
- **GA:** follows the upstream API reaching v1 (the upstream GA criteria wait on this implementation).

## Open Questions

1. Should Karpenter also honour `CapacityQuota` for NodeClaims created by static capacity, or only for dynamic provisioning in alpha?
2. Is filtering at offering level acceptable for scheduling performance with many quotas? A benchmark with 100 quotas and the full AWS instance type list is planned for alpha.
3. Who owns `status.used` when both autoscalers run in a cluster: whichever is configured to, a separate controller from SIG Autoscaling, or both writing the same values?
4. Should Karpenter install the `CapacityQuota` CRD in its Helm chart, or require users to install it from SIG Autoscaling?
