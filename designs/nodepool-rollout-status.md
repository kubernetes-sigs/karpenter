# NodePool and NodeClass rollout status

## Motivation

Karpenter exposes drift per NodeClaim, through the `Drifted` status condition. Neither the NodePool nor the NodeClass has an aggregate signal that answers the question an operator or an external orchestrator asks after pushing a change: *"has this spec finished rolling out?"*

In practice, that question is about the NodeClass as often as the NodePool. An AMI bump, a `userData` change, or any other `EC2NodeClass` spec edit changes the NodeClass, while the NodePool stays untouched. Argo CD health-checks the GVK that changed. A Lua check on `NodePool.status` can not see an AMI rollout, and a check on `EC2NodeClass.status` has nothing to read today.

Answering this today requires listing NodeClaims, grouping them by `karpenter.sh/nodepool` (or `spec.nodeClassRef`), and aggregating their conditions. Single-resource status evaluators (Argo CD health checks, `kstatus`, `kubectl wait`) can not do that because they only see the one resource they are evaluating. The workaround is an out-of-band job that re-implements the aggregation Karpenter already does internally ([#3071](https://github.com/kubernetes-sigs/karpenter/issues/3071)). Even consumers that can list NodeClaims (like kro) have no per-NodeClaim field that says which spec revision a node matches. They are stuck reading the `Drifted` condition, which has the issues covered in [Which Drift Causes Count](#which-drift-causes-count).

Core workload controllers solve this by reporting rollout accounting on the parent. A Deployment reports `replicas`, `updatedReplicas`, `readyReplicas`, and `observedGeneration`. A DaemonSet reports `desiredNumberScheduled` and `updatedNumberScheduled`, and Cluster API puts `upToDateReplicas` on MachineDeployments alongside an `UpToDate` condition on Machines. `kubectl rollout status` and most GitOps tooling rely on that pattern. NodePool is already partway there - it aggregates `status.resources` and `status.nodes`, and `status.nodes` is the `statuspath` of its scale subresource (playing the same role as `status.replicas` on a Deployment).

Earlier attempts at this are [#3108](https://github.com/kubernetes-sigs/karpenter/pull/3108) and [#3177](https://github.com/kubernetes-sigs/karpenter/pull/3177). Those proposals focused on reporting drift counts on the NodePool. I propose tracking how many of a parent's nodes are compatible with its current spec generation on both NodePool and NodeClass, with the NodeClaim as the source of truth.

### Use cases

1. Argo CD sync sequencing: An Argo Application deploys a NodePool and its NodeClass, together or in separate Applications. A Lua health check on each GVK needs to return `Progressing` while that resource's spec rolls out and `Healthy` once it finishes, so downstream Applications sync in order. The check can only read that single resource's `status`. This needs to cover AMI, `userData`, and other NodeClass spec changes, plus NodePool `spec.template` edits.
2. Composite APIs (kro): A `ResourceGraphDefinition` wraps NodePool and NodeClass into one custom API. A [collection `externalRef`](https://kro.run/docs/concepts/rgd/resource-definitions/external-references/#external-collections) selecting `karpenter.sh/nodepool` pulls the pool's NodeClaims into the graph so CEL can aggregate over them. Today, CEL can not tell whether a NodeClaim matches the current spec - `Drifted` mixes several causes with no revision attached, and the NodeClaim hash annotation does not cover NodeClass changes. Stamping `compatibleWith*Generation` on the NodeClaim gives CEL a field to compare against the parent's `metadata.generation` (for kro, the parent counts are just a convenience).
3. Threshold gates: Both Argo CD and kro users want a tolerance (e.g `>= 90%` up to date) because disruption budgets make node rollouts gradual by design, and 1 stuck node should not wedge a deployment pipeline indefinitely.
4. Fleet-wide rollout dashboards: Answering "are we fully onto the new AMI?" across the fleet is a time-series question over every drift cause, including out-of-band updates like an `al2023@latest` alias resolving to a new AMI w/o a spec change. Metrics fit this better than resource status (see [Which Drift Causes Count](#which-drift-causes-count)).

### Non-goals

- Aggregating arbitrary NodeClaim conditions onto the NodePool: [#3108](https://github.com/kubernetes-sigs/karpenter/pull/3108) proposed a generic `status.nodeClaimConditions[]` list of `{conditionType, count}`. That puts an open set of condition types (extensible by providers and versions) into the NodePool API with no defined semantics and no way to version or deprecate entries. We use named fields instead.
- Putting rollout policy in the API: No thresholds, settling windows, or "rollout paused/complete" state machine. Karpenter reports counts and consumers apply their own policy.
- Changing disruption behavior: Nothing here gates, throttles, or reorders drift.
- Counting out-of-band or dynamic drift in status: An AMI alias resolving to a new image, a capacity reservation reshuffle, or an instance type disappearing from the catalog does not bump `metadata.generation` on either parent. Status answers whether an applied spec change finished propagating.
- Changing `status.resources`: Capacity accounting stays on cluster state. Only `status.nodes` changes in what it counts (see [Redefining `status.nodes`](#redefining-statusnodes)).

## Proposal

Each NodeClaim records the latest NodePool and NodeClass generations it is compatible with. Each parent counts how many of its NodeClaims match its current generation by comparing those integers.

### NodeClaim Status

```yaml
apiVersion: karpenter.sh/v1
kind: NodeClaim
metadata:
  name: default-abc123
status:
  compatibleWithNodePoolGeneration: 12   # NEW
  compatibleWithNodeClassGeneration: 5    # NEW
  conditions:
    - type: Ready
      status: "True"
    - type: Drifted
      status: "True"
      reason: NodeClassDrifted
```

```go
type NodeClaimStatus struct {
    // ... existing fields ...

    // CompatibleWithNodePoolGeneration is the latest NodePool metadata.generation
    // this NodeClaim is known to still satisfy. Unset (0) means not yet evaluated
    // and is treated as not up to date.
    // +optional
    CompatibleWithNodePoolGeneration int64 `json:"compatibleWithNodePoolGeneration,omitempty"`

    // CompatibleWithNodeClassGeneration is the latest NodeClass metadata.generation
    // this NodeClaim is known to still satisfy. Unset (0) means not yet evaluated
    // and is treated as not up to date.
    // +optional
    CompatibleWithNodeClassGeneration int64 `json:"compatibleWithNodeClassGeneration,omitempty"`
}
```

The `nodeclaim.disruption` controller already evaluates drift and watches both NodePool and NodeClass, so it writes these fields:

- When provisioning creates a NodeClaim, it stamps both fields with the NodePool and NodeClass generations used to launch it.
- On reconcile, `nodeclaim.disruption` updates the two fields independently. Today's `isDrifted()` short-circuits, so static or requirements drift skips `cloudProvider.IsDrifted`. The stamp writer can not short-circuit - it fetches the referenced NodeClass and runs both checks on every pass:
  - If `areStaticFieldsDrifted` and `areRequirementsDrifted` both return `""`, set `compatibleWithNodePoolGeneration = nodePool.generation`
  - If `cloudProvider.IsDrifted` returns `""`, set `compatibleWithNodeClassGeneration = nodeClass.generation`
- When a check reports drift, the controller leaves that field at its previous value. The stamp acts as a high-water mark of the last generation the NodeClaim satisfied.
- When a check returns an error, the controller leaves that field unchanged. An error on one axis does not block the other (unlike today's `isDrifted()`, which returns on the first error). For example, if an invalid NodeClass has no resolved AMIs and `IsDrifted` errors out, the NodeClass stamp stays put while the NodePool stamp still advances if the NodePool checks pass.
- `InstanceTypeNotFound` belongs to neither parent spec and does not block either stamp.

Note: `IsDrifted` only controls whether a stamp advances when a parent's generation changes. Once stamped, a node stays counted as compatible with that generation even if `IsDrifted` later flips to true out-of-band (such as an AMI alias update). See [Which Drift Causes Count](#which-drift-causes-count).

### Shared rollout status

NodePool and every supported NodeClass embed the same `NodeRolloutStatus` struct, serialized inline under `status`:

```go
type NodeRolloutStatus struct {
    // ObservedGeneration is the generation of the parent spec that the node counts
    // below were computed against. Consumers gating on rollout progress must ignore
    // those counts when this does not equal the parent's metadata.generation.
    // +optional
    ObservedGeneration int64 `json:"observedGeneration,omitempty"`

    // Nodes is the count of NodeClaims associated with this parent, including NodeClaims
    // that have not yet launched or registered a Node and NodeClaims that are terminating.
    // +kubebuilder:default:=0
    // +optional
    Nodes *int64 `json:"nodes"`

    // UpToDateNodes is the count of associated NodeClaims whose corresponding
    // compatibleWith*Generation equals this parent's metadata.generation.
    // +kubebuilder:default:=0
    // +optional
    UpToDateNodes *int64 `json:"upToDateNodes"`

    // ReadyNodes is the count of associated NodeClaims whose Ready condition is True,
    // meaning they have launched, registered a Node, and initialized. This tracks
    // initial startup - the underlying conditions do not flip back if the Node later
    // goes NotReady.
    // +kubebuilder:default:=0
    // +optional
    ReadyNodes *int64 `json:"readyNodes"`

    // UpToDateAndReadyNodes is the count of associated NodeClaims counted by both
    // UpToDateNodes and ReadyNodes. A rollout is complete when this equals Nodes.
    // +kubebuilder:default:=0
    // +optional
    UpToDateAndReadyNodes *int64 `json:"upToDateAndReadyNodes"`
}
```

### NodePool status

We add the four rollout fields to `NodePool.status` and redefine `status.nodes` to serve as their denominator (see [Redefining `status.nodes`](#redefining-statusnodes)):

```yaml
apiVersion: karpenter.sh/v1
kind: NodePool
metadata:
  name: default
  generation: 12
status:
  observedGeneration: 12          # NEW: the NodePool generation the counts below were derived from
  nodes: 21                       # REDEFINED: NodeClaims owned by this NodePool
  upToDateNodes: 14               # NEW: of those, compatible with the current NodePool revision
  readyNodes: 18                  # NEW: of those, whose NodeClaim Ready condition is True
  upToDateAndReadyNodes: 12       # NEW: of those, both
  nodeClassObservedGeneration: 5  # exists today - not a NodeClass rollout signal
  resources: {...}
  conditions:
    - type: NodesUpToDate         # NEW
      status: "False"
      reason: RolloutInProgress   # RolloutBlocked when Ready is False
      message: 14/21 nodes are up to date
      observedGeneration: 12
```

```go
type NodePoolStatus struct {
    // ... existing fields ...
    NodeRolloutStatus `json:",inline"`
}
```

A NodeClaim owned by the NodePool is up to date when `compatibleWithNodePoolGeneration == nodePool.metadata.generation`. This does not check NodeClass compatibility, so a NodePool taint change can finish while an AMI rollout is still running on the NodeClass.

We also do not bump `NodePool.status.observedGeneration` when the NodeClass changes. That field records which NodePool spec revision the NodePool counts were computed against. An AMI change does not edit the NodePool, so bumping `observedGeneration` for it would make a starved NodePool counter look current.

### NodeClass Status

When an edit only touches the `EC2NodeClass` (AMI pin, `userData`, tags, block devices), Argo's Lua sandbox only sees the `EC2NodeClass` object. So the rollout counts for the NodeClass have to live on `EC2NodeClass.status`.

Providers embed `NodeRolloutStatus` in their NodeClass status type:

```yaml
apiVersion: karpenter.k8s.aws/v1
kind: EC2NodeClass
metadata:
  name: default
  generation: 5
status:
  observedGeneration: 5           # NEW: the NodeClass generation the counts below were derived from
  nodes: 21                       # NEW: NodeClaims whose spec.nodeClassRef points here
  upToDateNodes: 14               # NEW: of those, compatible with this NodeClass revision
  readyNodes: 18                  # NEW
  upToDateAndReadyNodes: 12       # NEW
  conditions:
    - type: NodesUpToDate         # NEW
      status: "False"
      reason: RolloutInProgress   # RolloutBlocked when Ready is False
      message: 14/21 nodes are up to date
      observedGeneration: 5
```

A NodeClaim referencing the NodeClass is up to date when `compatibleWithNodeClassGeneration == nodeClass.metadata.generation`, independent of whether a NodePool change is in flight. The denominator is every NodeClaim that references this NodeClass across all NodePools, which matches how an operator checks whether an AMI finished rolling out across the nodes using that class.

A new `nodeclass.counter` controller in core lists each supported NodeClass, fetches its NodeClaims via the existing `spec.nodeClassRef` index, and patches status on types that implement `NodeClassWithRolloutStatus`. It skips NodeClasses that do not implement the interface yet, which lets core and KWOK land before every provider merges the CRD changes.

Neither `nodeclass.counter` nor `nodepool.counter` skips resources where `Ready=False`. They still write `observedGeneration` and the counts on every generation. If the counter skipped unready parents, `observedGeneration` would stay behind after a broken edit and consumers could not tell a validation failure apart from a lagging controller (see [Edge Cases](#edge-cases)).

Each parent also gets a `NodesUpToDate` status condition (`True` when `upToDateNodes == nodes`, with `observedGeneration` set). I picked `NodesUpToDate` over Deployment's `Progressing` because `Progressing` inverts the polarity of every other Karpenter condition, where `True` is the healthy state, and the `Nodes` prefix matches `NodeClassReady` and `NodeRegistrationHealthy`. `NodesUpToDate` is not added to the `Ready` aggregate (`status.NewReadyConditions(ConditionTypeValidationSucceeded, ConditionTypeNodeClassReady)`). A pool mid-rollout is still healthy, and folding rollout state into `Ready` would break existing consumers.

When `upToDateNodes < nodes`, the condition reason indicates whether the rollout can make progress:

- `RolloutBlocked`: The parent's `Ready` condition is `False`. The provisioner skips unready NodePools (and NodePool `Ready` includes `NodeClassReady`), so no replacements will launch until the spec is fixed. The message includes the cause, e.g `14/21 nodes are up to date, NodeClass is not ready`.
- `RolloutInProgress`: The parent is `Ready=True`, so Karpenter can launch replacements as disruption budgets allow.

When `upToDateNodes == nodes`, `NodesUpToDate` is `True` regardless of `Ready`, since every node already matches the current spec. `NodesUpToDate` reads `Ready` without being part of the `Ready` aggregate, avoiding a dependency cycle.

The same Argo CD Lua health check works on both NodePool and NodeClass:

```lua
if obj.status.observedGeneration ~= obj.metadata.generation then
  return { status = "Progressing", message = "status is stale" }
end
for _, c in ipairs(obj.status.conditions or {}) do
  if c.type == "NodesUpToDate" and c.reason == "RolloutBlocked" then
    return { status = "Degraded", message = c.message }
  end
end
if obj.status.upToDateAndReadyNodes < obj.status.nodes * 0.9 then
  return { status = "Progressing", message = "rolling out" }
end
return { status = "Healthy" }
```

Returning `Degraded` on `RolloutBlocked` marks the Argo Application `Degraded` so it does not sit in `Progressing` when a rollout is wedged.

Users install this check on both GVKs:

- An Application that only edits the NodeClass relies on the NodeClass check
- An Application that only edits the NodePool relies on the NodePool check
- An Application containing both waits for both checks, since Argo ANDs resource health at the Application level

Why expose `upToDateNodes`, `readyNodes`, and `upToDateAndReadyNodes` separately? Karpenter launches a replacement before terminating the old node. Near the end of a rollout, there is a window where all remaining nodes match the new spec, but the newest replacements have not registered or initialized yet. Gating on `upToDateNodes` alone would report `Healthy` during that window and kick off the next Argo Application too early.

We also need `upToDateAndReadyNodes` because the other two numbers do not tell you their overlap. If 14/21 nodes are up to date and 18/21 are ready, anywhere from 11 to 14 nodes could be both. If the 3 unready nodes are new replacements failing to join, only 11 are both. If the 3 unready nodes are old nodes waiting for replacement, 14 are both and every new node is healthy. A consumer that only sees `status` on one object can not tell those two states apart without the intersection count.

### Redefining `status.nodes`

The four new fields count a parent's NodeClaims. `status.nodes` is the obvious denominator, and users already treat it as one, but its current implementation in `nodepool.counter` pulls from cluster state resource accounting:

```go
nodePool.Status.Resources = lo.Assign(BaseResources, c.cluster.NodePoolResourcesFor(nodePool.Name))
nodeQuantity := nodePool.Status.Resources[resources.Node]
nodePool.Status.Nodes = new(nodeQuantity.Value())
```

That accounting leaves out two sets of NodeClaims:

1. Deleting NodeClaims: `updateNodePoolResources` substitutes an empty `ResourceList` when `StateNode.MarkedForDeletion()` is true. The disruption controller sets `MarkedForDeletion` as soon as it picks a candidate, long before the instance terminates.
2. Unlaunched NodeClaims: `Cluster.UpdateNodeClaim` only creates a `StateNode` after `status.providerID` is populated.

Both exclusions hit at the same time during a rollout and shrink the denominator. Consider a 20-node pool near the end of a rollout w/ the default 10% disruption budget:

- 18 replacements are up and ready
- The disruption controller has selected the last 2 outdated nodes, which are now draining (`MarkedForDeletion` is set)
- 2 new replacements have been created, but have not launched yet (no `providerID`)

Across the 22 NodeClaims that exist, `upToDateAndReadyNodes` is 18 and `upToDateNodes` is 20. However, `status.nodes` reports 18 because it drops the 2 draining nodes and never counted the 2 unlaunched replacements. The Lua gate compares 18 against 18, returns `Healthy`, and releases the next Argo Application while 2 outdated nodes are still running workloads and 2 replacements have not come up. Worse, `upToDateNodes` (20) is greater than `status.nodes` (18).

Counting all NodeClaims owned by the NodePool (including unlaunched and terminating ones) gives `18/22` (~82%), which keeps the gate in `Progressing` until the replacements finish coming up and the old nodes are gone. The counter computes all five fields from a single NodeClaim list in one status patch. NodeClass `status.nodes` uses the exact same definition.

There is one trade-off with this change: `NodePool.status.nodes` will no longer always equal `NodePool.status.resources["nodes"]`. They will differ by the number of unlaunched and terminating NodeClaims. I think this split makes sense - `status.resources` tracks schedulable capacity (where excluding a draining node is right), while `status.nodes` is a replica count. We should call this out clearly in the field docs so it does not surprise users.

### Which Drift Causes Count

The `Drifted` condition is a union of several independent causes, and collapsing that union into a single number on the parent produces a metric that means different things depending on what is happening in the cluster ([maintainer comment on #3071](https://github.com/kubernetes-sigs/karpenter/issues/3071#issuecomment-5170006562)).

Stamping generations separates declarative drift (the spec in git changed) from dynamic drift (something in the environment changed under the same spec). A change triggers the GitOps gate only when it bumps `metadata.generation` and the corresponding drift check refuses to advance the stamp:

| Drift cause | Detected by | Advances `compatibleWithNodePoolGeneration`? | Advances `compatibleWithNodeClassGeneration`? | GitOps gate waits? |
|---|---|---|---|---|
| NodePool `spec.template` static fields | `areStaticFieldsDrifted` (hash compare, core) | No (stays at previous) | n/a | Yes (NodePool generation bumped) |
| NodePool `spec.template.spec.requirements` | `areRequirementsDrifted` | No | n/a | Yes, if the node is now incompatible |
| NodePool behavioral fields (`limits`, disruption, weight) | none (not drift) | Yes (advances to new generation) | n/a | No, nothing needs replacing |
| NodeClass spec change requiring replacement (AMI ID, `userData`, tags, block devices, instance profile, etc) | `cloudProvider.IsDrifted` | n/a | No | Yes, on the NodeClass GVK |
| NodeClass spec change not requiring replacement (e.g adding a compatible AMI or subnet to a selector) | `cloudProvider.IsDrifted` returns `""` | n/a | Yes | No, the existing node still matches |
| Out-of-band AMI (`al2023@latest` resolves to a new image, spec unchanged) | `cloudProvider.IsDrifted` (`AMIDrift`) | n/a | No, but generation did not bump so stamp still matches current generation | No |
| Instance type no longer offered | `instanceTypeNotFound` (core) | not consulted | n/a | No |

So the GitOps gate waits for an AMI change when someone updates the AMI in the NodeClass spec in git, and ignores it when Karpenter resolves a new image from an SSM alias w/o a spec change:

- Resource status answers: "did the spec change I applied finish rolling out?"
- Prometheus metrics answer: "how much of my fleet is drifted right now, and why?" (covered by [#3177](https://github.com/kubernetes-sigs/karpenter/pull/3177) w/ a `reason` label - see [Observability](#observability))

If we used `IsDrifted == ""` directly to count `upToDateNodes`, out-of-band AMI updates would trip the Argo health check and stall deployment pipelines on every weekly AMI publish. Using `IsDrifted == ""` only to advance the generation stamp avoids that footgun.

### How it works

Disruption writes the stamps on NodeClaims, and the two counter controllers aggregate them onto NodePool and NodeClass. Because disruption already watches NodePool and every supported NodeClass, applying a spec change triggers a NodeClaim reconcile immediately.

1. On NodeClaim creation, provisioning populates both `compatibleWith*Generation` fields from the parent objects it used.
2. On reconcile, `nodeclaim.disruption` runs the NodePool drift check and `cloudProvider.IsDrifted` independently. For each check that returns no drift, it advances the matching stamp to that parent's current `metadata.generation`.
3. `nodepool.counter` and `nodeclass.counter` list their NodeClaims, compare the generation stamps and `Ready` conditions, and patch `nodes`, `upToDateNodes`, `readyNodes`, `upToDateAndReadyNodes`, `observedGeneration`, and `NodesUpToDate`.

### Linearizability

[#3108](https://github.com/kubernetes-sigs/karpenter/pull/3108) ran into a race condition: asynchronously reconciled counts can be stale under CPU starvation or client-side throttling, letting a gate pass on numbers computed before the spec change was even observed. Watching for an edge ("the drifted count went up") does not fix this because plenty of valid spec changes cause 0 drift (e.g tightening requirements to drop instance types that no node is using).

Tying the counts to `observedGeneration` on the same object the consumer is checking closes that gap. This is also why NodeClass needs its own `observedGeneration`: an AMI change does not bump `NodePool.metadata.generation`, so a consumer watching the NodePool would have no way to know whether the counter had run since the AMI edit landed.

| Scenario | Parent `metadata.generation` | Status after counter runs (`upToDateNodes`/`nodes`) | Consumer sees |
|---|---|---|---|
| NodePool spec change that drifts nodes | NodePool `G+1` | NodePool `observedGeneration: G+1`, `14/20` | Progressing on NodePool |
| Same, counter starved | NodePool `G+1` | stale `observedGeneration: G` | Progressing (generation mismatch) |
| NodePool spec change that drifts nothing (`limits`) | NodePool `G+1` | `observedGeneration: G+1`, `20/20` (stamps advanced) | Healthy immediately (no rollout needed) |
| NodeClass spec change that drifts nodes (AMI pin, `userData`, etc) | NodeClass `C+1` | NodeClass `observedGeneration: C+1`, `14/20` (NodePool unchanged) | Progressing on NodeClass, NodePool stays Healthy |
| Same, NodeClass counter starved | NodeClass `C+1` | stale NodeClass `observedGeneration: C` | Progressing (generation mismatch on NodeClass) |
| NodeClass spec change that drifts nothing (selector widened) | NodeClass `C+1` | stamps advanced, `20/20` | Healthy immediately |
| Out-of-band AMI (alias, spec unchanged) | NodeClass `C` (unchanged) | stamps still equal `C`, `20/20` | Healthy for GitOps (`Drifted` condition and metrics still report `AMIDrift`) |
| Combined Application (NodePool + NodeClass), AMI pin | NodeClass `C+1`, NodePool unchanged | NodeClass `14/20`, NodePool `20/20` | Application Progressing until NodeClass check is Healthy |
| Rollout completes | current | `20/20` on the parent that changed | Healthy |

When a spec edit drifts nothing, `nodeclaim.disruption` advances the stamps on the first reconcile and the counter writes `20/20` at `G+1`. No special-casing is needed for no-op spec updates.

### Interaction with Existing Features

- Disruption budgets and drift back-off: Unchanged. Budgets and back-off control how fast `upToDateNodes` converges, so a budget-constrained pool stays in `RolloutInProgress` longer.
- Terminating NodeClaims: A NodeClaim with a deletion timestamp stays in `nodes` until it is deleted from the API server (see [Redefining `status.nodes`](#redefining-statusnodes)).
- Static NodePools (`spec.replicas`) and the scale subresource: Works with no special cases. Redefining `status.nodes` puts it on the same NodeClaim basis that the static provisioning and deprovisioning controllers already use to reconcile `spec.replicas`, so a static pool at its target size keeps `nodes == spec.replicas` mid-rollout.
- `do-not-disrupt` NodeClaims: These can hold `upToDateNodes` below `nodes` indefinitely, which is why the API exposes counts so consumers can apply a threshold.
- Hash version bumps across Karpenter upgrades: Karpenter already re-stamps NodeClaims that are not drifted. During an upgrade, `areStaticFieldsDrifted` returns `""` because a hash version mismatch is not treated as drift, so `compatibleWithNodePoolGeneration` advances normally.
- NodeClaims missing the `nodepool-hash` annotation: This happens on adopted NodeClaims or ones hydrated from an older Karpenter version. Today `areStaticFieldsDrifted` returns `""` when the annotation is missing, which would advance the stamp. I want to verify the exact ordering against the hydration controller before implementing this - if hydration can lag behind disruption, we should treat a missing hash annotation as not compatible so we fail closed.
- Existing NodeClaims after CRD upgrade: `compatibleWith*Generation` starts at `0` until the first disruption reconcile. Because disruption watches NodePool and NodeClass and lists all NodeClaims, populating the stamps takes one queue drain (we do not wait for the 5-minute drift interval).
- NodeClass shared by multiple NodePools: The NodeClass counter sums NodeClaims across every pool referencing it, so the NodeClass reports complete only when all of those pools have rolled.
- Out-of-band AMI update concurrent with a non-drifting NodeClass edit: `IsDrifted` returns `AMIDrift`, so `nodeclaim.disruption` can not tell that the spec edit itself was benign and leaves `compatibleWithNodeClassGeneration` at the old generation. The GitOps gate waits for the AMI replacement, which was going to happen anyway.

### Observability

To cover fleet-wide drift dashboards (use case 4), the metric proposed in [#3177](https://github.com/kubernetes-sigs/karpenter/pull/3177) needs a `reason` label. The NodeClaim `Drifted` condition already sets the reason via `SetTrueWithReason(ConditionTypeDrifted, driftedReason, ...)`:

```
karpenter_nodepools_nodeclaim_condition{nodepool, condition, status, reason}
```

Adding `reason` lets operators slice by `NodePoolDrifted`, `AMIDrift`, `NodeClassDrifted`, etc in Prometheus without baking provider-specific drift reasons into the CRD schema. Providers already use a bounded set of reason strings, so metric cardinality stays low.

### Edge Cases

- NodeClaim created from the old spec, not yet in the informer cache: If a NodeClaim is launched from generation `G` right as `G+1` lands, the counter can briefly miss it and report `20/20` before flipping to `20/21` once the watch event arrives. That window is bounded by informer watch latency and causes a brief `Healthy -> Progressing` flap, not a gate stuck at `Healthy`. Every other cache lag case fails closed (an unobserved new NodeClaim already has the `G+1` stamp, and a stale cache entry for a deleted old NodeClaim just makes the rollout look less complete).
- Empty NodePool or NodeClass: All four counts are `0` and `NodesUpToDate` is `True`. Consumers that care about non-empty pools can check `nodes > 0`.
- Unready nodes unrelated to a rollout: A NodeClaim stuck launching for other reasons holds `readyNodes` below `nodes` even with no rollout running (same as a crashlooping pod in a Deployment). The consumer's threshold tolerance handles single-node failures, and `NodeRegistrationHealthy` still flags pools that can not launch nodes at all.
- NodePool with `Ready=False` due to a bad `nodeClassRef`: The NodePool counter still updates the NodePool counts, and `NodesUpToDate` sets reason `RolloutBlocked` if a rollout is incomplete. If the NodeClass object does not exist at all, `nodeclass.counter` has nothing to patch until the reference is fixed.
- NodeClass edit that fails validation: Suppose someone applies an AMI selector at generation `C+1` that matches 0 AMIs. The NodeClass goes `Ready=False`, which makes the NodePool `Ready=False`, so the provisioner stops launching replacements. Stamps on nodes that need replacement stay at `C` (because `IsDrifted` either reports drift or errors out). `nodeclass.counter` still writes `observedGeneration: C+1` and sets `NodesUpToDate=False` w/ reason `RolloutBlocked`. The Lua check sees `RolloutBlocked` at the current generation and returns `Degraded`. Note: if the invalid edit somehow did not drift any existing nodes, the stamps would advance to `C+1` and `NodesUpToDate` would be `True` - only `Ready=False` shows the failure in that case, so health checks should also inspect `Ready`.
- Rapid successive edits (`G -> G+1 -> G+2`): The Lua check blocks on `observedGeneration == metadata.generation` until the counter has run against `G+2`, so it will not pass on counts from `G+1`. Once the counter catches up to `G+2`, the gate waits for the NodeClaim stamps to reach `G+2`.
- KWOK and providers where `IsDrifted` always returns `""`: On these providers, every NodeClass edit advances `compatibleWithNodeClassGeneration` on the next reconcile, so `upToDateNodes` immediately matches `nodes`. That is expected since KWOK has no AMI or `userData` to roll. The NodeClass status still tracks `observedGeneration` and `readyNodes`.

## Alternatives Considered

### Option 1: Aggregate the `Drifted` Condition onto the NodePool (#3108)

This is what [#3071](https://github.com/kubernetes-sigs/karpenter/issues/3071) originally asked for and what [#3108](https://github.com/kubernetes-sigs/karpenter/pull/3108) implemented. I rejected this for two reasons:

- `Drifted` is a union of declarative and dynamic causes, so a drifted count > 0 does not tell Argo CD whether a git change is rolling out or an SSM parameter updated in the background
- `Drifted` is reconciled asynchronously on a 5-minute loop with no generation anchor, so a health check can easily read stale 0-drift counts right after a spec apply and pass prematurely

### Option 2: Add a Boolean `Drifted` Condition on the NodePool

A boolean condition is simple to read, but it forces all-or-nothing semantics where a single `do-not-disrupt` node blocks a pipeline forever, and it has the same union problem as Option 1. Including `NodesUpToDate` alongside the integer counts gives simple consumers a boolean (anchored to the spec generation) without taking thresholds away from Argo CD and kro.

### Option 3: Have the NodePool Counter Re-Parse Hashes Directly

In the first revision of this RFC, I had `nodepool.counter` inspect the NodeClaim hash annotations itself. I dropped that approach because `nodeclaim.disruption` already has all the drift inputs (including `cloudProvider.IsDrifted`) and already watches NodeClass. Putting `compatibleWithNodeClassGeneration` on the NodeClaim lets core run `nodeclass.counter` without changing the `CloudProvider` interface to categorize drift reasons.

### Option 4: Combine `nodeClassObservedGeneration` with `Drifted` on the NodePool (No NodeClaim Stamps)

`nodeClassObservedGeneration` bumps on every NodeClass edit, including ones that do not require replacing nodes. Pairing that with a boolean `Drifted==True` check on NodeClaims fails in one of two ways: either non-drifting NodeClass edits look like active rollouts while unrelated drift exists, or dynamically drifted nodes look like an active spec rollout when the generation never changed. Tracking the compatible generation per axis on the NodeClaim avoids both bugs.

### Option 5: Fold NodeClass Compatibility into `NodePool.status.upToDateNodes` Only

This breaks the Argo CD use case because Argo only checks the resource that changed. An Application that only manages `EC2NodeClass` (e.g bumping an AMI pin) does not touch the NodePool and would have no status to watch. Bumping `NodePool.status.observedGeneration` on NodeClass edits would also make a starved NodePool counter look current. Putting the status on both GVKs matches how Argo evaluates health.

### Option 6: Prometheus Metrics Only (#3177)

Argo CD Lua health checks and kro CEL expressions only have access to Kubernetes objects in the cluster and can not query Prometheus. We need both status fields (for orchestration gates) and metrics (for fleet dashboards).

### Option 7: `ControllerRevision` Accounting (Like DaemonSet)

Creating `ControllerRevision` objects for each NodePool and NodeClass change would give full revision history and rollback support, at the cost of managing and garbage-collecting a new set of API objects just to track what two `int64` generation stamps already tell us.

## Backward Compatibility

Everything in this proposal is gated behind the `NodeRolloutStatus` feature gate (off by default in Alpha). When disabled:

- Provisioning and `nodeclaim.disruption` do not write the generation stamps
- `nodeclaim.disruption` keeps its existing short-circuiting `isDrifted()` check
- `nodepool.counter` keeps `status.nodes` equal to `status.resources["nodes"]` and skips the new rollout fields and `NodesUpToDate` condition
- `nodeclass.counter` is not registered

The new fields on NodeClaim, NodePool, and NodeClass are read-only status fields, so no user manifests need to change. Users will need to apply the updated CRDs during upgrade as usual. Because `NodesUpToDate` is separate from `Ready`, existing tooling watching `Ready` is unaffected.

Providers can adopt `NodeClassWithRolloutStatus` incrementally in follow-up PRs (AWS, Azure, GCP). Core skips any NodeClass CRD that does not implement the interface yet, and KWOK ships with it in the initial PR as the reference implementation. Until a provider adds the NodeClass status fields, users can still read the generation stamps on NodeClaims directly.

## Graduation criteria

The feature ships behind `--feature-gates NodeRolloutStatus=true` (or `FEATURE_GATES=NodeRolloutStatus=true`):

- Alpha (`NodeRolloutStatus=false` by default): Initial implementation in core and KWOK
- Beta (`NodeRolloutStatus=true` by default): API server status-patch load tested at scale, user feedback gathered on the `status.nodes` change, and `NodeRolloutStatus` adopted on `EC2NodeClass` in `karpenter-provider-aws`
- GA (gate removed): At least one minor release in Beta with no regressions in controller reconcile latency or API server write load

## Open Questions

1. Should we keep `status.nodes` and `status.resources["nodes"]` in sync during rollouts? This proposal leaves `status.resources` on cluster state and moves `status.nodes` to count NodeClaims. Moving both to NodeClaims would keep them equal, at the cost of `status.resources` including the capacity of nodes that are draining or have not launched yet.

## References

- Issue: [Surface NodeClaim drift/rollout progress in NodePool status (#3071)](https://github.com/kubernetes-sigs/karpenter/issues/3071)
- Prior PRs: [#3108](https://github.com/kubernetes-sigs/karpenter/pull/3108) (status), [#3177](https://github.com/kubernetes-sigs/karpenter/pull/3177) (metrics)
- Maintainer feedback: [#3071 (comment)](https://github.com/kubernetes-sigs/karpenter/issues/3071#issuecomment-5170006562), [#3216 (NodeClass generation)](https://github.com/kubernetes-sigs/karpenter/pull/3216#discussion_r3857866911), [#3216 (AMI / Argo)](https://github.com/kubernetes-sigs/karpenter/pull/3216#discussion_r3876142714), [#3216 (NodeClaim stamps)](https://github.com/kubernetes-sigs/karpenter/pull/3216#discussion_r3898578138)
- Drift design docs: [`designs/drift.md`](./drift.md), [`designs/drift-hash-versioning.md`](./drift-hash-versioning.md)
- Prior art: Deployment `status.updatedReplicas` / `observedGeneration`, Cluster API `MachineDeployment.status.upToDateReplicas` + Machine `UpToDate` condition, [`kstatus`](https://github.com/kubernetes-sigs/cli-utils/tree/master/pkg/kstatus)
