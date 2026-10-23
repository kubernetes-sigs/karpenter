# NodePool and NodeClass rollout status

## Motivation

Karpenter exposes drift per NodeClaim, through the `Drifted` status condition. Neither the NodePool nor the NodeClass has an aggregate signal that answers the question an operator or an external orchestrator asks after pushing a change: *"has this spec finished rolling out?"*

That question is as often about the NodeClass as the NodePool. An AMI bump, a `userData` change, or any other EC2NodeClass spec edit changes the NodeClass, not the NodePool. Argo CD health-checks the GVK that changed. A Lua check on `NodePool.status` cannot see an AMI rollout, and a check on `EC2NodeClass.status` has nothing to read today.

Answering it today requires listing NodeClaims, grouping them by `karpenter.sh/nodepool` (or `spec.nodeClassRef`), and aggregating their conditions. Consumers that gate on a single resource's status cannot do that, because they evaluate only the one resource they are looking at. These consumers include Argo CD health checks, `kstatus`, and `kubectl wait`. The workaround is an out-of-band job that re-implements the aggregation Karpenter already performs internally ([#3071](https://github.com/kubernetes-sigs/karpenter/issues/3071)). Consumers that can list NodeClaims, such as kro, still have no per-NodeClaim field that says which spec revision a node matches. They are left with the `Drifted` condition, which has the problems described in [Which drift causes count](#which-drift-causes-count).

Core workload controllers solve this by reporting rollout accounting on the parent. A Deployment reports `replicas`/`updatedReplicas`/`readyReplicas` plus `observedGeneration`, a DaemonSet reports `desiredNumberScheduled`/`updatedNumberScheduled`, and Cluster API reports `upToDateReplicas` on MachineDeployments alongside an `UpToDate` condition on Machines. `kubectl rollout status` and most GitOps tooling build on that convention. NodePool is already partway there. It aggregates `status.resources` and `status.nodes`, and `status.nodes` is the `statuspath` of its scale subresource, so it already plays the role `status.replicas` plays on a Deployment. This RFC extends that accounting to rollout progress on both parents, with the NodeClaim as the source of truth for "compatible with generation G."

Earlier attempts are [#3108](https://github.com/kubernetes-sigs/karpenter/pull/3108) and [#3177](https://github.com/kubernetes-sigs/karpenter/pull/3177). Unlike those, this RFC does not propose reporting "drift" on the NodePool. It proposes reporting *how many of a parent's nodes were provisioned from its current spec revision*. Drift is the mechanism that eventually makes those numbers converge. The revision is the contract consumers gate on.

### Use cases

1. **GitOps sync sequencing (Argo CD).** An Argo Application deploys a NodePool and its NodeClass, either together or in separate Applications. A custom Lua health check on each GVK must report `Progressing` while that resource's spec propagates and `Healthy` once it has, so that downstream Applications sync in order. The check can read only that resource's `status`. This covers AMI, `userData`, and any other NodeClass spec change, as well as NodePool `spec.template` edits.
2. **Composite APIs (kro).** A `ResourceGraphDefinition` wraps NodePool and NodeClass into one custom API. A [collection `externalRef`](https://kro.run/docs/concepts/rgd/resource-definitions/external-references/#external-collections) with a selector on `karpenter.sh/nodepool` brings the pool's NodeClaims into the graph, and CEL can aggregate over them. What CEL cannot do today is tell whether a given NodeClaim matches the current spec. The `Drifted` condition mixes several causes and is not tied to a revision, and the NodeClaim hash annotation does not cover NodeClass changes. The `compatibleWith*Generation` stamps give CEL a field to compare against the parent's `metadata.generation`. For kro, the parent counts are a convenience, not a requirement.
3. **Threshold-based gates.** Both of the above want a tolerance ("settled at ≥ 90% up to date") rather than an all-or-nothing boolean. Disruption budgets make node rollout gradual by design, and a single stuck node should not block a pipeline indefinitely.
4. **Fleet-wide rollout dashboards.** "Are we fully onto the new AMI?" is a time-series question over every drift cause, including out-of-band ones such as an `al2023@latest` alias resolving to a new AMI with no spec change. This is a different question from use cases 1 to 3, and we suggest metrics rather than status for it. See [Which drift causes count](#which-drift-causes-count).

### Non-goals

- **Aggregating arbitrary NodeClaim conditions onto the NodePool.** #3108 proposed a generic `status.nodeClaimConditions[]` list of `{conditionType, count}`. That puts an open set of condition types, extensible by providers and versions, into the NodePool API. No entry has defined semantics, and there is no way to version or deprecate individual entries. We propose named fields with defined meanings instead.
- **Policy in the API.** No thresholds, settling windows, or "rollout paused/complete" state machine. Karpenter reports counts, and consumers apply their own policy.
- **Changing disruption behavior.** Nothing here gates, throttles, or reorders drift.
- **Covering out-of-band or dynamic drift in the status counts.** An AMI alias resolving to a new image, a capacity-reservation reshuffle, or an instance type disappearing from the catalog does not bump `metadata.generation` on either parent. Status answers "did the spec I applied finish propagating?", and those events are not a spec apply. See [Which drift causes count](#which-drift-causes-count).
- **Changing `status.resources`.** Capacity accounting stays on cluster state. The one existing field this RFC changes is `status.nodes`, and only in what it counts. See [Redefining `status.nodes`](#redefining-statusnodes).

## Proposal

The NodeClaim records the latest parent generation it is still compatible with. Each parent counts how many of its NodeClaims are compatible with *its* current generation. The counter does not re-derive drift. It compares integers.

### NodeClaim status (source of truth)

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

The existing `nodeclaim.disruption` controller already evaluates drift and watches NodePool and NodeClass. It becomes the writer.

- When provisioning creates a NodeClaim, it stamps both fields with the generations of the NodePool and NodeClass it used to launch the NodeClaim.
- On reconcile, the controller advances the two fields independently. Today's `isDrifted()` short-circuits, so static or requirements drift skips `cloudProvider.IsDrifted`. The stamp writer must not short-circuit. It fetches the referenced NodeClass and runs both checks on every pass.
  - If `areStaticFieldsDrifted` and `areRequirementsDrifted` are both empty, set `compatibleWithNodePoolGeneration = nodePool.generation`.
  - If `cloudProvider.IsDrifted` returns `""`, set `compatibleWithNodeClassGeneration = nodeClass.generation`.
- If a check reports drift, the controller leaves the corresponding field at its previous value. The field is a high-water mark of the last generation this claim still matched, not a copy of the current generation.
- If a check returns an error, the controller treats it like drift and leaves that field unchanged. An error on one axis does not stop the other from advancing. Today's `isDrifted()` returns on the first error, so the stamp writer cannot reuse it as is. For example, a provider may fail `IsDrifted` when an invalid NodeClass has no resolved AMIs. The NodeClass stamp stays put, and the NodePool stamp still advances if the NodePool checks pass.
- `InstanceTypeNotFound` belongs to neither axis and does not block either stamp.

`IsDrifted` is only the signal to *advance* a stamp. It is not the predicate for *counting* a node. That split lets a GitOps gate cover NodeClass spec changes, including AMI, without stalling on out-of-band AMI alias updates. See [Which drift causes count](#which-drift-causes-count).

### Shared rollout status

NodePool and every supported NodeClass anonymously embed the same rollout accounting struct. Go promotes the embedded fields, and JSON serializes them directly under `status`, so both resources expose the same flat paths.

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
    // meaning they have launched, registered a Node, and initialized. This reports
    // whether a node successfully came up, not whether it is currently healthy: the
    // underlying conditions do not revert if the Node later goes NotReady.
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

This RFC adds four read-only fields to `NodePool.status` and redefines the existing `status.nodes` so that it can serve as their denominator (see [Redefining `status.nodes`](#redefining-statusnodes)).

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
  nodeClassObservedGeneration: 5  # exists today; not a NodeClass rollout signal (see below)
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

A NodeClaim owned by the NodePool is up to date when `compatibleWithNodePoolGeneration == nodePool.metadata.generation`. The count does *not* require NodeClass compatibility, so a NodePool taint change can report complete while an AMI rollout is in flight on the other axis. Each parent reports the propagation of its own spec.

We do not bump `NodePool.status.observedGeneration` when the NodeClass changes. That field means "these NodePool counts were computed against this NodePool spec revision." An AMI pin does not edit the NodePool, so bumping `observedGeneration` for it would make a starved counter look current. Argo health-checks the GVK that changed, and AMI belongs on the NodeClass.

### NodeClass status (provider contract)

The Argo CD AMI use case depends on this section. An AMI pin, a `userData` change, or any other EC2NodeClass spec edit changes the NodeClass. Argo's Lua sandbox sees only that object, so the counts have to live there. Folding them into NodePool would leave an Application that only applies `EC2NodeClass` with nothing to read.

Providers anonymously embed the shared `NodeRolloutStatus` in their NodeClass status type.

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

A NodeClaim referencing the NodeClass is up to date when `compatibleWithNodeClassGeneration == nodeClass.metadata.generation`. The count does *not* require NodePool compatibility, so an AMI rollout can report complete while a NodePool taint change is in flight on the other axis. Each parent reports the propagation of its own spec.

The denominator is every NodeClaim that references this NodeClass, across NodePools. That matches the question "did this AMI finish rolling out?", which is about one NodeClass and every node that uses it.

A new `nodeclass.counter` controller in core lists each supported NodeClass, lists its NodeClaims through the existing `spec.nodeClassRef` index, and patches status for types that implement `NodeClassWithRolloutStatus`. It skips NodeClasses that do not implement the interface yet, so this can land in core (KWOK and the shared type) before every provider has merged the CRD fields.

The counter does not check the NodeClass `Ready` condition. It writes `observedGeneration` and the counts for every generation, including one that leaves the NodeClass `Ready=False`. If it skipped NodeClasses that are not ready, `observedGeneration` would stay behind after an invalid edit, and consumers could not tell a broken change from a counter that is behind. `nodepool.counter` follows the same rule for NodePools. See the case of a NodeClass edit that fails validation under [Edge cases](#edge-cases).

Each parent also gets one status condition, `NodesUpToDate`. It is `True` when `upToDateNodes == nodes`, and it sets `observedGeneration`. We chose this name over Deployment's `Progressing` because `Progressing` inverts the polarity of every other Karpenter condition, where `True` is the settled state. The `Nodes` prefix matches the existing `NodeClassReady` and `NodeRegistrationHealthy`. The condition is not part of the NodePool's `Ready` aggregate (`status.NewReadyConditions(ConditionTypeValidationSucceeded, ConditionTypeNodeClassReady)`). A pool mid-rollout is healthy, not unready, and adding the condition would change `Ready` semantics for every existing consumer without warning. The same applies to NodeClass `Ready`.

While the count is short, the `NodesUpToDate` reason says whether the rollout can make progress.

- `RolloutBlocked` when the parent's `Ready` condition is `False`. The provisioner skips NodePools that are not ready, and NodePool `Ready` includes `NodeClassReady`, so no replacements launch until someone fixes the spec. The message names the cause, for example `14/21 nodes are up to date; NodeClass is not ready`.
- `RolloutInProgress` otherwise. Karpenter can launch replacements, and the count converges as disruption budgets allow.

When `upToDateNodes == nodes`, the condition is `True` whatever `Ready` says, because every node already matches the current spec. `NodesUpToDate` reads `Ready` but is not part of the `Ready` aggregate, so the two conditions do not depend on each other in a cycle. No reason value names the axis that is behind, because the counts on each parent already show that.

Consumers gate as follows, and the same Lua works on NodePool and on NodeClass. It reports `Degraded` for a blocked rollout, so Argo marks the Application `Degraded` instead of leaving it `Progressing` on a rollout that cannot finish.

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

Install the check on both GVKs. Each one covers changes the other cannot see.

- An Application that only changes the NodeClass (AMI pin, `userData`, tags, block devices) needs the NodeClass check. A NodePool-only check cannot see the change.
- An Application that only changes the NodePool needs the NodePool check. A NodeClass-only check cannot see the change.
- An Application that contains both waits for both health checks, because Argo already ANDs resource health at the Application level. Folding NodeClass compatibility into NodePool `upToDateNodes` would mark a NodePool-only Application Progressing during an AMI rollout it did not apply, and would still leave a NodeClass-only Application with no signal.

A rollout gate needs both revision agreement and workload readiness. That is why `upToDateNodes` and `readyNodes` are separate fields, the same way Deployment separates `updatedReplicas` from `readyReplicas`. Karpenter creates a replacement before it terminates the node being replaced. Near the end of a rollout there is therefore a window where every remaining node is up to date but the newest ones have not registered or initialized. A gate on `upToDateNodes` alone would report Healthy during that window and let the next Argo Application in the sequence sync too early.

`upToDateAndReadyNodes` exists because the other two counts cannot determine the intersection. If 14 of 21 nodes are up to date and 18 of 21 are ready, anywhere from 11 to 14 can be both. If the 3 unready nodes are new replacements, only 11 are both, and the new nodes are failing to come up. If the 3 unready nodes are old ones awaiting replacement, 14 are both, and every new node is healthy. A consumer restricted to a single resource's status cannot tell these cases apart.

### Redefining `status.nodes`

The four new fields count the NodePool's NodeClaims. `status.nodes` is the natural denominator for them, and consumers already reach for it, but its current definition cannot serve that role. `nodepool.counter` reads it out of the cluster-state resource accounting.

```go
nodePool.Status.Resources = lo.Assign(BaseResources, c.cluster.NodePoolResourcesFor(nodePool.Name))
nodeQuantity := nodePool.Status.Resources[resources.Node]
nodePool.Status.Nodes = new(nodeQuantity.Value())
```

That accounting omits two groups. NodeClaims marked for deletion contribute nothing, because `updateNodePoolResources` substitutes an empty `ResourceList` when `StateNode.MarkedForDeletion()` is true. The disruption controller sets `MarkedForDeletion` as soon as it *selects* a candidate, long before the instance is gone. NodeClaims that have not launched are also absent, because `Cluster.UpdateNodeClaim` creates a `StateNode` only once `status.providerID` is set.

Both exclusions apply at the same moment, and both understate the denominator. Take a 20-node pool near the end of a rollout with the default 10% disruption budget. Eighteen replacements are up and ready. The disruption controller has selected the last 2 outdated nodes, and they are draining. Their 2 replacements exist but have not launched. Counted over the 22 NodeClaims that exist, `upToDateAndReadyNodes` is 18 and `upToDateNodes` is 20. `status.nodes` reports 18. It excludes the 2 draining nodes because they are marked for deletion, and the 2 unlaunched replacements never became `StateNode`s.

The gate above then compares 18 against 18, returns Healthy, and releases the next Argo Application while two outdated nodes still run workloads and two replacements have yet to come up. On the NodeClaim basis it compares 18 against 22, which is 82%, and correctly reports Progressing. The same arithmetic puts `upToDateNodes` (20) above `status.nodes` (18), which makes no sense for two fields consumers are expected to divide.

This RFC therefore redefines `status.nodes` to count the NodePool's NodeClaims, including ones that have not launched and ones that are terminating. The counter computes all five fields from one NodeClaim list in one status patch. Keeping terminating outdated nodes in the denominator until they are gone is what a rollout gate wants, because the pool reports incomplete until the replacement is in place. NodeClass `status.nodes` uses the same definition for the same reason.

One consequence needs a decision. NodePool `status.nodes` currently *is* `status.resources["nodes"]`, and decoupling them means the two can disagree by the number of unlaunched and terminating NodeClaims. The split is defensible. `status.resources` reports schedulable capacity, where excluding a draining node is correct, while `status.nodes` is a replica count. The field documentation should state this so users do not find out by surprise.

### Which drift causes count

The `Drifted` condition is the union of several independent causes. Collapsing that union into a parent-level number produces a signal that means different things at different times ([maintainer comment on #3071](https://github.com/kubernetes-sigs/karpenter/issues/3071#issuecomment-5170006562)).

The generation stamps separate *declarative* drift, where the spec in git changed, from *dynamic* drift, where the world changed under an unchanged spec. A field is "in the GitOps gate" when a change to it bumps `metadata.generation` and the corresponding `IsDrifted` or static-hash check refuses to advance the stamp.

| Drift cause | Detected by | Advances `compatibleWithNodePoolGeneration`? | Advances `compatibleWithNodeClassGeneration`? | GitOps gate waits? |
|---|---|---|---|---|
| NodePool `spec.template` static fields | `areStaticFieldsDrifted` (hash compare, core) | No (stays at previous) | n/a | Yes (NodePool generation bumped) |
| NodePool `spec.template.spec.requirements` | `areRequirementsDrifted` | No | n/a | Yes, if the node is now incompatible |
| NodePool behavioral fields (`limits`, disruption, weight) | none (not drift) | Yes (advances to new generation) | n/a | No. Nothing needs replacing. |
| NodeClass spec change that requires replacement (AMI ID, `userData`, tags, block devices, instance profile, …) | `cloudProvider.IsDrifted` | n/a | No | Yes, on the NodeClass GVK |
| NodeClass spec change that does not require replacement (e.g. adding a compatible AMI or subnet to a selector) | `cloudProvider.IsDrifted` returns `""` | n/a | Yes | No. The existing node still matches. |
| Out-of-band AMI (`al2023@latest` resolves to a new image, spec unchanged) | `cloudProvider.IsDrifted` (`AMIDrift`) | n/a | No, but the generation did not bump, so the stamp still equals the current generation | No |
| Instance type no longer offered | `instanceTypeNotFound` (core) | not consulted | n/a | No |

The GitOps gate therefore covers an AMI change when the AMI is in the NodeClass spec that Argo applied. It does not cover an AMI change when Karpenter resolved a new image from an alias with no spec change. Those are different questions.

- Status answers "did the declarative change I applied finish propagating?" It is anchored to a revision, level-triggered, and readable by evaluators that see one resource. An AMI pin in git counts. An alias auto-update does not.
- Metrics answer "what is the state of drift across my fleet, now and over time?" They cover the full union, sliced by reason. That is the job of #3177, extended with a `reason` label (see [Observability](#observability)).

Using `IsDrifted == ""` as the *count* predicate would pull out-of-band AMI updates into the gate and stall pipelines on a weekly AMI cadence. Stamping generations, and advancing them only when `IsDrifted == ""`, gives the Argo use case NodeClass coverage without that stall.

### How it works

Disruption writes the stamps, and two counters aggregate them onto each parent. Disruption already watches NodePool and every supported NodeClass, so a spec apply reconciles NodeClaims immediately.

On NodeClaim create, provisioning stamps both fields with the current parent generations. On reconcile, disruption evaluates NodePool drift and `cloudProvider.IsDrifted` independently, even if one axis has drifted. For each axis that has not drifted, it advances the matching stamp to that parent's current generation. For an axis that has drifted, it leaves the stamp unchanged.

Each parent counter lists its NodeClaims, counts stamp matches and `Ready`, and writes `nodes`, `upToDateNodes`, `readyNodes`, `upToDateAndReadyNodes`, and `observedGeneration` in one status patch. `nodepool.counter` extends the existing NodePool status writer and compares only the NodePool stamp. `nodeclass.counter` is new. It lists by `spec.nodeClassRef` and patches only types that implement `NodeClassWithRolloutStatus`. Counters compare integers and do not re-derive drift.

### Linearizability

#3108 raised a second concern. Counts reconciled asynchronously can be arbitrarily stale under CPU starvation or client throttling, so a gate can pass on numbers computed before the change landed. Edge detection ("the drifted count went up") does not fix this, because a NodePool update does not always cause drift. Restricting requirements to prune instance types that were never in use bumps the generation and drifts nothing.

Anchoring the counts to the generation of the *same object the consumer is looking at* solves both problems. That is why NodeClass needs its own `observedGeneration` instead of bumping NodePool's. An AMI spec change does not bump `NodePool.metadata.generation`, and pretending it did would make stale NodePool counts look current.

| Scenario | Parent `metadata.generation` | Status after counter runs (`upToDateNodes`/`nodes`) | Consumer sees |
|---|---|---|---|
| NodePool spec change that drifts nodes | NodePool `G+1` | NodePool `observedGeneration: G+1`, `14/20` | Progressing on NodePool |
| Same, counter starved | NodePool `G+1` | stale `observedGeneration: G` | Progressing (generation mismatch) |
| NodePool spec change that drifts nothing (`limits`) | NodePool `G+1` | `observedGeneration: G+1`, `20/20` (stamps advanced) | Healthy immediately, which is correct because no rollout was needed |
| NodeClass spec change that drifts nodes (AMI pin, `userData`, …) | NodeClass `C+1` | NodeClass `observedGeneration: C+1`, `14/20`. NodePool counts unchanged (NodePool generation did not bump) | Progressing on NodeClass. NodePool Lua stays Healthy, which is correct because that Application did not apply the AMI. |
| Same, NodeClass counter starved | NodeClass `C+1` | stale NodeClass `observedGeneration: C` | Progressing (generation mismatch on the NodeClass). This window is why the health check belongs on the NodeClass GVK, not on NodePool. Disruption already watches NodeClass, so stamp updates take one reconcile, not the 5-minute drift interval. |
| NodeClass spec change that drifts nothing (selector widened) | NodeClass `C+1` | stamps advanced, `20/20` | Healthy immediately, which is correct |
| Out-of-band AMI (alias, spec unchanged) | NodeClass `C` (unchanged) | stamps still equal `C`, `20/20` | Healthy, which is correct for GitOps. The `Drifted` condition and the reason-labeled metric still show `AMIDrift`. |
| Combined Application (NodePool + NodeClass), AMI pin | NodeClass `C+1`, NodePool unchanged | NodeClass `14/20`; NodePool `20/20` | Application Progressing until the NodeClass check is Healthy |
| Rollout completes | current | `20/20` on the parent that changed | Healthy |

The rows where a spec change drifts nothing are the case that defeats edge-triggered designs. A level-triggered count anchored to the revision handles them without special logic. The disruption controller advances the stamp when the node still matches, and the counter recomputes on every pass. "Nothing needed to change" and "everything already changed" therefore look the same to the consumer.

### Interaction with existing features

- **Disruption budgets and drift back-off.** Unchanged. Budgets and back-off control *how fast* `upToDateNodes` converges, not what is counted. A pool that is backed off or blocked by a budget reports incomplete for longer.
- **Terminating NodeClaims.** A NodeClaim with a deletion timestamp counts in `nodes` until it is gone. This changes today's behavior, as described in [Redefining `status.nodes`](#redefining-statusnodes). If the NodeClaim is outdated, the pool reports incomplete until the replacement is in place, which is what a rollout gate wants.
- **Static NodePools (`spec.replicas`) and the scale subresource.** The same accounting applies with no special cases. The redefinition puts `status.nodes` on the same basis the static provisioning and deprovisioning controllers already use to satisfy `spec.replicas`. A pool at its replica count now reports `nodes == spec.replicas` mid-rollout instead of dipping below it.
- **`do-not-disrupt` NodeClaims.** These can hold `upToDateNodes` below `nodes` indefinitely. That report is correct, and it is the reason the API exposes counts rather than a boolean. Consumers set a tolerance.
- **Hash version bumps across Karpenter upgrades.** Karpenter already re-stamps NodeClaims that are not drifted. During the upgrade window `areStaticFieldsDrifted` returns `""`, because it does not treat a version mismatch as drift, so `compatibleWithNodePoolGeneration` still advances. Counts stay correct.
- **NodeClaims with no `nodepool-hash` annotation.** This includes NodeClaims adopted or hydrated from an older version. `areStaticFieldsDrifted` returns `""` when annotations are missing, so the stamp would advance. We should confirm this against the hydration controller before implementation. If hydration lags, we should treat missing annotations as *not* compatible so that errors fall on the conservative side.
- **Existing NodeClaims after the CRD upgrade.** `compatibleWith*` is unset (`0`) until the first disruption reconcile, so counts read low for one pass per NodeClaim. Disruption watches NodePool and NodeClass and lists every NodeClaim, so the window lasts one controller queue drain, not until the node next drifts.
- **A NodeClass used by several NodePools.** Its counts sum NodeClaims across those pools, so it reports complete only when every pool has rolled the NodeClass spec. Each NodePool reports complete when *its* own spec has rolled, independent of the AMI axis.
- **Out-of-band AMI concurrent with a non-drifting NodeClass edit.** `IsDrifted` still returns `AMIDrift`, so the controller does not advance `compatibleWithNodeClassGeneration`, even though the spec change alone would not require replacement. The GitOps gate stays Progressing until those nodes are replaced. This is conservative. See [Edge cases](#edge-cases).

### Observability

Status covers declarative rollouts, and metrics cover the rest. The complementary metric work in #3177 needs one addition, a `reason` label on the drift condition. The NodeClaim condition already carries the reason (`SetTrueWithReason(ConditionTypeDrifted, driftedReason, ...)`).

```
karpenter_nodepools_nodeclaim_condition{nodepool, condition, status, reason}
```

That label gives use case 4 a per-cause view (`reason="NodePoolDrifted"`, `reason="AMIDrift"`, `reason="NodeClassDrifted"`) without putting the taxonomy in the API. Providers define a bounded set of reason values, so cardinality stays manageable.

### Edge cases

- **NodeClaim created from the previous revision, not yet in the informer cache.** A NodeClaim launched from spec `G` while the update to `G+1` lands can be briefly invisible, so the pool reports `20/20` before flipping to `20/21`. Watch latency bounds the window, and the result is a brief flap from Healthy to Progressing, not a gate stuck at Healthy. All other cache-lag cases err on the conservative side. An unobserved new NodeClaim gets the current generation stamp and counts as up to date, and a stale cache entry for a deleted outdated NodeClaim only makes the pool look less complete.
- **Empty NodePool or NodeClass.** All four counts are `0` and the condition is `True`. Consumers that need "nonempty and settled" check `nodes > 0` themselves.
- **Unready nodes that are not part of a rollout.** A NodeClaim stuck launching for unrelated reasons holds `readyNodes` below `nodes` with no rollout in flight. A Deployment with a crash-looping pod behaves the same way. The count is accurate, and the consumer's tolerance decides whether it blocks. The existing `NodeRegistrationHealthy` condition remains the signal for a NodePool that cannot launch nodes at all.
- **NodePool with `Ready: False` from a bad NodeClass reference.** The NodePool still reports counts against the NodePool stamp, and the existing `Ready` condition signals the failure. If a NodePool rollout is in flight, `NodesUpToDate` reports `RolloutBlocked`. If the NodeClass object is missing, the NodeClass counter has nothing to patch, and those NodeClaims are absent from NodeClass counts until the reference resolves.
- **A NodeClass edit that fails validation.** Take an AMI selector that matches nothing. Applying it bumps the generation to `C+1` and leaves the NodeClass `Ready=False`. NodePool `Ready` includes `NodeClassReady`, and the provisioner skips NodePools that are not ready, so no replacements launch. Stamps on nodes that need replacement stay at `C`, either because `IsDrifted` reports drift or because it returns an error. The counter still writes `observedGeneration: C+1`. Consumers see current counts with `upToDateNodes < nodes` that do not move, alongside a `Ready=False` condition whose own `observedGeneration` is `C+1`. `NodesUpToDate` is `False` with reason `RolloutBlocked`, and the example health check reports `Degraded` instead of `Progressing`. If the invalid edit does not make any node drift, the stamps advance, the counts read complete, and `NodesUpToDate` is `True`. Only `Ready` shows the problem in that case, so a health check that needs to catch it must read `Ready` as well. Fixing or reverting the spec bumps the generation again, and the stamps follow the normal rules.
- **Rapid successive edits.** Each apply bumps `metadata.generation`, from G to G+1 to G+2. The Lua check fails `observedGeneration == metadata.generation` until the counter has run against the *latest* generation, so it cannot pass on counts computed for G+1 after G+2 has landed. Once the counter has observed G+2, the stamps still have to catch up. Disruption either advances them, if the latest spec does not require replacement, or leaves them at an earlier generation until replacements exist. The gate stays Progressing through both windows and does not go Healthy on an intermediate revision.
- **Non-drifting NodeClass spec change while nodes have dynamic drift, such as an AMI alias update.** `IsDrifted` stays true, so the NodeClass stamp does not advance and the GitOps gate waits for replacement. That is conservative relative to the spec change, and the replacement was going to happen anyway.
- **KWOK and providers where `IsDrifted` always returns `""`.** The NodeClass stamp advances whenever `IsDrifted` is empty, so on these providers every NodeClass spec edit advances every stamp on the next disruption reconcile. `upToDateNodes` then equals `nodes`. That is correct, because KWOK has no AMI, `userData`, or equivalent, so there is no NodeClass-driven replacement to wait for. The NodeClass GVK still gets `observedGeneration`, so a starved counter cannot pass. It also still gets the Ready counts, because launch and initialization still matter. NodePool stamps are unaffected and still follow the NodePool hash and requirements checks.

## Alternatives considered

**Aggregate the existing `Drifted` condition onto the NodePool.** This is what the issue requested and what #3108 implemented. We rejected it as the primary mechanism for two reasons. The union semantics make the number mean different things in different clusters. The controller also writes the condition asynchronously with a 5-minute requeue and no revision anchor, so it cannot support a correct gate. The revision comparison is more precise for this use case and cheaper to compute.

**A NodePool-level `Drifted` condition only.** This gives a simple boolean gate, but it forces all-or-nothing semantics with no tolerance for a single `do-not-disrupt` node, and it carries the same union ambiguity. The proposed `NodesUpToDate` condition gives consumers who want a boolean one, defined against the revision instead of the union.

**Have the NodePool counter re-parse hashes itself.** This was the first revision of this RFC. We rejected it in favor of NodeClaim stamps. The disruption controller already has the drift inputs, including `cloudProvider.IsDrifted`, and already watches NodeClass. It is also the component that *knows* whether a NodeClass spec change requires replacement. Putting `compatibleWithNodeClassGeneration` on the NodeClaim makes a NodeClass counter possible without a cloud-provider interface change to categorize drift reasons.

**Use `nodeClassObservedGeneration` plus `IsDrifted` as a boolean on the NodePool, with no NodeClaim stamps.** `nodeClassObservedGeneration` bumps for every NodeClass spec change, including ones that do not require replacement. Combining it with a NodeClaim `Drifted==True` boolean leads to one of two errors. Either the design treats non-drifting spec changes as rollouts until something else clears Drifted, or it treats dynamically drifted nodes as in-rollout even when the generation did not change. The two generation stamps separate those cases by construction.

**Fold NodeClass compatibility into NodePool `upToDateNodes` and skip NodeClass status.** This does not work for the Argo use case, and it puts the signal on the wrong GVK. Argo health-checks the object that changed. An Application that applies an AMI pin to `EC2NodeClass` without the NodePool would have no signal. Bumping NodePool `observedGeneration` on NodeClass edits would also make a starved NodePool counter look current. NodeClass `observedGeneration` protects against stale counts on that GVK, and Argo's Application-level AND of the two health checks combines the two signals.

**Metrics only (#3177).** Argo CD health checks and kro CEL expressions read Kubernetes objects and cannot query Prometheus. Metrics complement status and do not replace it, so this RFC proposes both.

**ControllerRevision-based accounting, like DaemonSet.** Materializing revisions would give richer history, such as which revision each NodeClaim belongs to, and would support rollback. It would also add a new persisted object per revision, plus garbage collection for those objects, to record information the generation stamp already encodes.

## Backward compatibility

The new NodeClaim fields, NodePool fields, and condition are additive and read-only, and no YAML needs to change. Users must apply the updated CRDs to see the fields, as with any Karpenter CRD upgrade. `NodesUpToDate` is not part of the `Ready` aggregate, so `Ready` semantics stay the same for existing consumers.

NodeClass status fields are also additive on each provider CRD. Providers that have not added them do not implement `NodeClassWithRolloutStatus`, and core skips them. KWOK ships the fields in the same change as the NodePool API, so the in-tree provider is a complete example.

`status.nodes` is the one field whose meaning changes. It is read-only, so nothing breaks on the wire, but its value shifts. It now includes NodeClaims that have not launched and NodeClaims that are terminating, so it reads higher than before during provisioning and disruption. For a steady-state pool it is unchanged.

Inlining `NodeRolloutStatus` preserves the existing JSON path and ordinary Go selectors such as `nodePool.Status.Nodes`. Go callers that use keyed composite literals need a source change. `NodePoolStatus{Nodes: value}` must become `NodePoolStatus{NodeRolloutStatus: NodeRolloutStatus{Nodes: value}}`. Karpenter does not use such literals internally, but external Go consumers may need this mechanical update.

## Graduation criteria

We propose no feature gate. The change is additive and read-only, uses data Karpenter already maintains, and does not affect provisioning or disruption behavior. The main risk is the API shape, which this RFC exists to settle.

Provider NodeClass fields can lag behind the core NodeClaim and NodePool fields. AMI, `userData`, and other NodeClass spec rollouts become visible to Argo on the NodeClass GVK once each provider adds the CRD fields, in follow-up PRs for AWS, Azure, and GCP. Until then, users can already query the NodeClaim stamps, and KWOK is the in-tree example. A NodePool health check does not wait for NodeClass compatibility, so an AMI Application must health-check the NodeClass GVK.

## Open questions

1. **Should `status.nodes` and `status.resources["nodes"]` be reconciled rather than allowed to diverge?** The proposal keeps `status.resources` on cluster state and moves only `status.nodes`. Moving both to the NodeClaim basis would be more consistent, but `status.resources` would then report the capacity of nodes that are draining or have not launched.
## References

- Issue: [Surface NodeClaim drift/rollout progress in NodePool status (#3071)](https://github.com/kubernetes-sigs/karpenter/issues/3071)
- Prior implementation attempts: [#3108](https://github.com/kubernetes-sigs/karpenter/pull/3108) (status), [#3177](https://github.com/kubernetes-sigs/karpenter/pull/3177) (metrics)
- Maintainer feedback this RFC responds to: [#3071 (comment)](https://github.com/kubernetes-sigs/karpenter/issues/3071#issuecomment-5170006562), [#3216 (NodeClass generation)](https://github.com/kubernetes-sigs/karpenter/pull/3216#discussion_r3857866911), [#3216 (AMI / Argo)](https://github.com/kubernetes-sigs/karpenter/pull/3216#discussion_r3876142714), [#3216 (NodeClaim stamps)](https://github.com/kubernetes-sigs/karpenter/pull/3216#discussion_r3898578138)
- Drift semantics: [`designs/drift.md`](./drift.md), [`designs/drift-hash-versioning.md`](./drift-hash-versioning.md)
- Precedent: Deployment `status.updatedReplicas`/`observedGeneration`; Cluster API `MachineDeployment.status.upToDateReplicas` and the Machine `UpToDate` condition; [`kstatus`](https://github.com/kubernetes-sigs/cli-utils/tree/master/pkg/kstatus)
