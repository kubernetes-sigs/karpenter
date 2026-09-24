# NodePool and NodeClass Rollout Status

## Motivation

Karpenter exposes drift per-NodeClaim, via the `Drifted` status condition. There is no aggregate signal on the NodePool — or on the NodeClass — that answers the question an operator or an external orchestrator asks after pushing a change: *"has this spec finished rolling out?"*

That question is as often about the NodeClass as the NodePool. An AMI bump, a `userData` change, or any other EC2NodeClass spec edit is applied to the NodeClass, not the NodePool. Argo CD health-checks the GVK that changed. A Lua check on `NodePool.status` cannot see an AMI rollout, and a check on `EC2NodeClass.status` has nothing to read today.

Answering it today requires listing NodeClaims, grouping them by `karpenter.sh/nodepool` (or `spec.nodeClassRef`), and aggregating their conditions. Every consumer that gates on a single resource's status — Argo CD health checks, kro `readyWhen` expressions, `kstatus`, `kubectl wait` — is structurally unable to do that, because their evaluation is scoped to the one resource they are looking at. The workaround is an out-of-band job that re-implements the aggregation Karpenter already performs internally ([#3071](https://github.com/kubernetes-sigs/karpenter/issues/3071)).

Core workload controllers solve this by reporting rollout accounting on the parent: a Deployment reports `replicas`/`updatedReplicas`/`readyReplicas` plus `observedGeneration`, a DaemonSet reports `desiredNumberScheduled`/`updatedNumberScheduled`, and Cluster API reports `upToDateReplicas` on MachineDeployments alongside an `UpToDate` condition on Machines. `kubectl rollout status` and essentially all GitOps tooling are built on that convention. NodePool is already partway there: it aggregates `status.resources` and `status.nodes`, and `status.nodes` is the `statuspath` of its scale subresource, so it already occupies the position `status.replicas` does. This RFC extends that accounting to rollout progress on **both** parents, with the NodeClaim as the source of truth for "compatible with generation G."

Earlier attempts at this are [#3108](https://github.com/kubernetes-sigs/karpenter/pull/3108) and [#3177](https://github.com/kubernetes-sigs/karpenter/pull/3177). The difference from these is that we do not propose surfacing "drift" on the NodePool. We propose surfacing *how many of a parent's nodes were provisioned from its current spec revision*. Drift is the mechanism that eventually makes those numbers converge; the revision is the contract consumers gate on.

### Use Cases

1. **GitOps sync sequencing (Argo CD).** A NodePool and its NodeClass are deployed by an Argo Application (together or in separate Applications). A custom Lua health check on each GVK must report `Progressing` while that resource's spec propagates and `Healthy` once it has, so that downstream Applications sync in order. The check can read only that resource's `status`. This includes AMI, `userData`, and any other NodeClass spec change — not only NodePool `spec.template` edits.
2. **Composite APIs (kro).** A `ResourceGraphDefinition` wraps NodePool + NodeClass into one custom API and evaluates `readyWhen` CEL against the resources in the graph. NodeClaims are not in the graph — their names and count are not known at authoring time — so the instance flips to ready as soon as the NodePool is admitted rather than when the replacement completes.
3. **Threshold-based gates.** Both of the above want tolerance ("settled at ≥ 90% up to date") rather than an all-or-nothing boolean, because disruption budgets make node rollout deliberately gradual and a single stuck node should not block a pipeline indefinitely.
4. **Fleet-wide rollout dashboards.** "Are we fully onto the new AMI?" — a time-series question over *all* drift vectors, including out-of-band ones (an `al2023@latest` alias resolving to a new AMI with no spec change). This is a different question from 1–3 and we suggest using metrics rather than status. See [Which drift vectors count](#which-drift-vectors-count).

### Non-Goals

- **Aggregating arbitrary NodeClaim conditions onto the NodePool.** #3108 proposed a generic `status.nodeClaimConditions[]` list of `{conditionType, count}`. That makes an open, provider- and version-extensible set of condition types part of the NodePool API with no defined semantics per entry, and no way to version or deprecate individual entries. We propose named fields with defined meanings instead.
- **Policy in the API.** No thresholds, settling windows, or "rollout paused/complete" state machine. Karpenter reports counts; consumers apply their own policy.
- **Changing disruption behavior.** Nothing here gates, throttles, or reorders drift.
- **Covering out-of-band / dynamic drift in the status counts.** An AMI alias resolving to a new image, a capacity-reservation reshuffle, or an instance type disappearing from the catalog does not bump `metadata.generation` on either parent. Status answers "did the spec I applied finish propagating?"; those events are not a spec apply. See [Which drift vectors count](#which-drift-vectors-count).
- **Changing `status.resources`.** Capacity accounting stays on cluster state, untouched. The one existing field this RFC does change is `status.nodes`, and only in what it counts — see [Redefining `status.nodes`](#redefining-statusnodes).

## Proposal

The NodeClaim records the latest parent generation it is still compatible with. Each parent counts how many of its NodeClaims are compatible with *its* current generation. The counter does not re-derive drift; it compares integers.

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

The existing `nodeclaim.disruption` controller already evaluates drift and already watches NodePool and NodeClass. It becomes the writer:

- On NodeClaim create, provisioning stamps both fields to the generation of the NodePool and NodeClass resolved to launch the NodeClaim.
- On reconcile, the two fields are advanced **independently**. Today's `isDrifted()` short-circuits (static/requirements drift skips `cloudProvider.IsDrifted`); the stamp writer must not. It Gets the referenced NodeClass and runs both checks every pass:
  - If `areStaticFieldsDrifted` and `areRequirementsDrifted` are both empty, set `compatibleWithNodePoolGeneration = nodePool.generation`.
  - If `cloudProvider.IsDrifted` returns `""`, set `compatibleWithNodeClassGeneration = nodeClass.generation`.
- If the corresponding check is drifted, the field is left at its previous value. It is a high-water mark of "last generation this claim still matched," not a copy of the current generation.
- `InstanceTypeNotFound` is neither axis: it does not block either stamp.

`IsDrifted` is used only as the *advance* signal, not as the *count* predicate. That is what lets a GitOps gate cover NodeClass spec changes (including AMI) without stalling on out-of-band AMI alias updates — see [Which drift vectors count](#which-drift-vectors-count).

### Shared rollout status

NodePool and every supported NodeClass anonymously embed the same rollout accounting struct. Anonymous embedding promotes the fields in Go and serializes them directly under `status`, so both resources expose the same flat JSON paths:

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

Four additive, read-only fields on `NodePool.status`, plus a redefinition of the existing `status.nodes` so that it can serve as their denominator (see [Redefining `status.nodes`](#redefining-statusnodes)):

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
      reason: RolloutInProgress
      message: 14/21 nodes are up to date
      observedGeneration: 12
```

```go
type NodePoolStatus struct {
    // ... existing fields ...
    NodeRolloutStatus `json:",inline"`
}
```

A NodeClaim owned by the NodePool is up to date when `compatibleWithNodePoolGeneration == nodePool.metadata.generation`. NodeClass compatibility is *not* required: a NodePool taint change should be able to report complete even if an AMI rollout is in flight on a different axis. Each parent reports its own spec's propagation.

We do **not** bump `NodePool.status.observedGeneration` when the NodeClass changes. That field means "these NodePool counts were computed against this NodePool spec revision." An AMI pin does not edit the NodePool, so lying about `observedGeneration` would make a starved counter look current. Argo health-checks the GVK that changed; AMI belongs on the NodeClass.

### NodeClass status (provider contract)

This is the load-bearing surface for the Argo CD AMI use case. An AMI pin, `userData` change, or any other EC2NodeClass spec edit is applied to the NodeClass. Argo's Lua sandbox sees only that object, so the counts have to live here — folding them into NodePool would leave an Application that only applies `EC2NodeClass` with nothing to read.

Providers anonymously embed the shared `NodeRolloutStatus` in their NodeClass status type:

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
      reason: RolloutInProgress
      message: 14/21 nodes are up to date
      observedGeneration: 5
```

A NodeClaim referencing the NodeClass is up to date when `compatibleWithNodeClassGeneration == nodeClass.metadata.generation`. NodePool compatibility is *not* required: an AMI rollout should be able to report complete even if a NodePool taint change is in flight on a different axis. Each parent reports its own spec's propagation.

The denominator is every NodeClaim that references this NodeClass, across NodePools. That is the right unit for "did this AMI finish rolling out": one NodeClass, every node that uses it.

A `nodeclass.counter` controller in core lists each supported NodeClass, lists its NodeClaims via the existing `spec.nodeClassRef` index, and patches status for types that implement the interface. NodeClasses that do not yet implement it are skipped, so this can land in core (KWOK + the shared type) before every provider has merged the CRD fields.

Plus one status condition on each parent, `NodesUpToDate`, `True` when `upToDateNodes == nodes`, with `observedGeneration` set. The name is chosen over Deployment's `Progressing` because `Progressing` inverts the polarity of every other Karpenter condition, where `True` is the settled state; the `Nodes` prefix matches the existing `NodeClassReady` and `NodeRegistrationHealthy`. It is deliberately **not** added to the NodePool's `Ready` aggregate (`status.NewReadyConditions(ConditionTypeValidationSucceeded, ConditionTypeNodeClassReady)`) — a pool mid-rollout is healthy, not unready, and folding this in would silently change `Ready` semantics for every existing consumer. Same for NodeClass `Ready`.

The `NodesUpToDate` reason is `RolloutInProgress` while the count is short. We do not currently see a need for a second reason value; the counts on each parent already say which spec is behind.

Consumers gate as follows. The same Lua works on NodePool and on NodeClass:

```lua
if obj.status.observedGeneration ~= obj.metadata.generation then
  return { status = "Progressing", message = "status is stale" }
end
if obj.status.upToDateAndReadyNodes < obj.status.nodes * 0.9 then
  return { status = "Progressing", message = "rolling out" }
end
return { status = "Healthy" }
```

Install it on **both** GVKs. That is required, not optional:

- An Application that only changes the NodeClass (AMI pin, `userData`, tags, block devices) is gated by the NodeClass check. A NodePool-only check cannot see it.
- An Application that only changes the NodePool is gated by the NodePool check. A NodeClass-only check cannot see it.
- An Application that contains both waits for both health checks, which is the AND Argo already performs at Application level. Folding NodeClass into NodePool `upToDateNodes` would make a NodePool-only Application Progressing during an AMI rollout it did not apply, and would still leave a NodeClass-only Application with no signal.

A rollout gate needs both revision agreement and workload readiness, which is why `upToDateNodes` and `readyNodes` are reported separately in the same way Deployment separates `updatedReplicas` from `readyReplicas`. Karpenter creates a replacement before terminating the node it replaces, so there is a window near the end of a rollout where every remaining node is up to date but the newest ones have not registered or initialized yet. A gate on `upToDateNodes` alone would report Healthy during that window and allow the next Argo Application in the overall sequence to sync prematurely.

`upToDateAndReadyNodes` is reported because that intersection cannot be derived from the other two counts. Knowing that 14 of 21 nodes are up to date and 18 of 21 are ready says nothing about how many are both: anywhere from 11 to 14, depending on whether the unready nodes are the new ones or the ones still awaiting replacement. Those two cases mean opposite things — a rollout nearly finished versus one that has barely started replacing unhealthy nodes — and a consumer restricted to a single resource's status cannot tell them apart.

### Redefining `status.nodes`

The four new fields are counted over the NodePool's NodeClaims. `status.nodes` is the natural denominator for them, and is already the field consumers reach for, but its current definition cannot serve that role. `nodepool.counter` reads it back out of the cluster-state resource accounting:

```go
nodePool.Status.Resources = lo.Assign(BaseResources, c.cluster.NodePoolResourcesFor(nodePool.Name))
nodeQuantity := nodePool.Status.Resources[resources.Node]
nodePool.Status.Nodes = new(nodeQuantity.Value())
```

That accounting omits two groups. NodeClaims marked for deletion contribute nothing (`updateNodePoolResources` substitutes an empty `ResourceList` when `StateNode.MarkedForDeletion()`), and `MarkedForDeletion` is set as soon as the disruption controller *selects* a candidate — long before the instance is gone. NodeClaims that have not launched are also absent, because `Cluster.UpdateNodeClaim` only creates a `StateNode` once `status.providerID` is set.

Both exclusions apply at the same moment, and both understate the denominator. Take a 20-node pool near the end of a rollout, with the default 10% disruption budget: 18 replacements are up and ready, the last 2 outdated nodes have been selected for disruption and are draining, and their 2 replacements have been created but not yet launched. Counted over the 22 NodeClaims that exist, `upToDateAndReadyNodes` is 18 and `upToDateNodes` is 20. `status.nodes` reports 18 — the 2 draining nodes are excluded as marked for deletion, and the 2 unlaunched replacements were never `StateNode`s.

The gate above then evaluates 18 against 18, returns Healthy, and releases the next Argo Application while two outdated nodes are still running workloads and two replacements have yet to come up. On the NodeClaim basis it evaluates 18 against 22 — 82% — and correctly reports Progressing. The same arithmetic also puts `upToDateNodes` (20) above `status.nodes` (18), which is incoherent for a pair of fields consumers are expected to divide.

So `status.nodes` is redefined to count the NodePool's NodeClaims, including ones that have not launched and ones that are terminating, and all five fields are computed from a single NodeClaim list in a single status patch. Terminating outdated nodes staying in the denominator until they are gone is the behavior a rollout gate wants: the pool reports incomplete until the replacement is actually in place. The NodeClass `status.nodes` uses the same NodeClaim-list definition for the same reason.

One consequence to settle: NodePool `status.nodes` currently *is* `status.resources["nodes"]`, and decoupling them means the two keys can disagree by the number of unlaunched and terminating NodeClaims. The split is defensible — `status.resources` is a report of schedulable capacity, where excluding a draining node is correct, while `status.nodes` is a replica count — but it should be stated in the field documentation rather than discovered.

### Which drift vectors count

[The `Drifted` condition is the union of several independent causes, and collapsing that union into a parent-level number produces a signal that means different things at different times.](https://github.com/kubernetes-sigs/karpenter/issues/3071#issuecomment-5170006562)

The generation stamps separate *declarative* drift (the spec in git changed) from *dynamic* drift (the world changed under a spec that did not). A field is "in the GitOps gate" when a change to it bumps `metadata.generation` **and** the corresponding `IsDrifted` / static-hash check refuses to advance the stamp.

| Drift vector | Detected by | Advances `compatibleWithNodePoolGeneration`? | Advances `compatibleWithNodeClassGeneration`? | GitOps gate waits? |
|---|---|---|---|---|
| NodePool `spec.template` static fields | `areStaticFieldsDrifted` (hash compare, core) | No (left at previous) | — | **Yes** (NodePool gen bumped) |
| NodePool `spec.template.spec.requirements` | `areRequirementsDrifted` | No | — | **Yes**, if the node is now incompatible |
| NodePool behavioral fields (`limits`, disruption, weight) | none (not drifted) | Yes (advanced to new gen) | — | No — correctly, nothing to replace |
| NodeClass spec change that requires replacement (AMI ID, `userData`, tags, block devices, instance profile, …) | `cloudProvider.IsDrifted` | — | No | **Yes**, on the NodeClass GVK |
| NodeClass spec change that does not require replacement (e.g. adding a compatible AMI/subnet to a selector) | `cloudProvider.IsDrifted` returns `""` | — | Yes | No — correctly, the existing node still matches |
| Out-of-band AMI (`al2023@latest` resolves to a new image, spec unchanged) | `cloudProvider.IsDrifted` (`AMIDrift`) | — | No, but gen **did not bump**, so stamp still equals current gen | **No** |
| Instance type no longer offered | `instanceTypeNotFound` (core) | not consulted | — | **No** |

The GitOps gate therefore covers AMI when the AMI is in the NodeClass spec that Argo applied, and does not cover AMI when Karpenter discovered a new image from an alias with no spec change. Those are different questions:

- **Status** answers "did the declarative change I applied finish propagating?" — revision-anchored, level-triggered, consumable by single-resource evaluators. An AMI pin in git is in. An alias auto-update is not.
- **Metrics** answer "what is the state of drift across my fleet right now and over time?" — the full union, sliced by reason. That is #3177's job, extended with a `reason` label ([Observability](#observability)).

Using `IsDrifted == ""` as the *count* predicate would have pulled out-of-band AMI into the gate and stalled pipelines on a weekly AMI cadence. Stamping generations and only *advancing* them when `IsDrifted == ""` gives the Argo use case the NodeClass coverage it needs without that stall.

### How It Works

Disruption writes the stamps; two counters aggregate them onto each parent. Disruption already watches NodePool and every supported NodeClass, so a spec apply reconciles NodeClaims immediately.

On NodeClaim create, stamp both fields to the current parent generations. On reconcile, evaluate NodePool drift and `cloudProvider.IsDrifted` independently — even if one axis has drifted — and advance the matching stamp to that parent's current generation. Leave it unchanged if that axis has drifted.

Each parent counter lists its NodeClaims, counts stamp-matches and `Ready`, and writes `nodes`, `upToDateNodes`, `readyNodes`, `upToDateAndReadyNodes`, and `observedGeneration` in a single status patch. `nodepool.counter` extends the existing NodePool status writer and compares only the NodePool stamp. `nodeclass.counter` is new: it lists by `spec.nodeClassRef` and patches only types that implement `NodeClassWithRolloutStatus`. Counters compare integers; they do not re-derive drift.

### Linearizability

The other concern in #3108: counts reconciled asynchronously can be arbitrarily stale under CPU starvation or client throttling, so a gate can pass on numbers computed before the change landed. Edge detection ("the drifted count went up") does not fix it, because a NodePool update is not guaranteed to induce drift at all — restricting requirements to prune instance types that were never in use bumps the generation and drifts nothing.

Anchoring the counts to the generation of the *same object the consumer is looking at* resolves both halves. That is why NodeClass needs its own `observedGeneration` rather than bumping NodePool's: an AMI spec change does not bump `NodePool.metadata.generation`, and pretending it did would make stale NodePool counts look current.

| Scenario | Parent `metadata.generation` | Status after counter runs (`upToDateNodes`/`nodes`) | Consumer sees |
|---|---|---|---|
| NodePool spec change that drifts nodes | NodePool `G+1` | NodePool `observedGeneration: G+1`, `14/20` | Progressing on NodePool |
| Same, counter starved | NodePool `G+1` | stale `observedGeneration: G` | Progressing (generation mismatch) |
| NodePool spec change that drifts nothing (`limits`) | NodePool `G+1` | `observedGeneration: G+1`, `20/20` (stamps advanced) | Healthy immediately — correct, no rollout was needed |
| NodeClass spec change that drifts nodes (AMI pin, `userData`, …) | NodeClass `C+1` | NodeClass `observedGeneration: C+1`, `14/20`. NodePool counts unchanged (NodePool gen did not bump) | Progressing on NodeClass. NodePool Lua stays Healthy, which is correct: that Application did not apply the AMI. |
| Same, NodeClass counter starved | NodeClass `C+1` | stale NodeClass `observedGeneration: C` | Progressing (generation mismatch on the NodeClass). This window is why the health check belongs on the NodeClass GVK, not on NodePool. Disruption already watches NodeClass, so stamp updates are one reconcile, not the 5-minute drift interval. |
| NodeClass spec change that drifts nothing (selector widened) | NodeClass `C+1` | stamps advanced, `20/20` | Healthy immediately — correct |
| Out-of-band AMI (alias, spec unchanged) | NodeClass `C` (unchanged) | stamps still equal `C`, `20/20` | Healthy — correct for GitOps; the `Drifted` condition and the reason-labeled metric still show `AMIDrift` |
| Combined Application (NodePool + NodeClass), AMI pin | NodeClass `C+1`, NodePool unchanged | NodeClass `14/20`; NodePool `20/20` | Application Progressing until the NodeClass check is Healthy |
| Rollout completes | current | `20/20` on the parent that changed | Healthy |

The "spec change that drifts nothing" rows are the case that defeats edge-triggered designs and that a level-triggered, revision-anchored count handles for free: the disruption controller advances the stamp when the node still matches, the counter recomputes every pass, so "nothing needed to change" and "everything already changed" are indistinguishable.

### Interaction with Existing Features

- **Disruption budgets / drift back-off.** Unchanged. Budgets and back-off govern *how fast* `upToDateNodes` converges; they do not change what is counted. A pool that is backed off or budget-blocked simply reports incomplete for longer.
- **Terminating NodeClaims.** A NodeClaim with a deletion timestamp still counts in `nodes` until it is gone — a change from today's behavior, per [Redefining `status.nodes`](#redefining-statusnodes). If it is outdated, the pool keeps reporting incomplete until the replacement is in place, which is what a rollout gate wants.
- **Static NodePools (`spec.replicas`) and the scale subresource.** Same accounting applies; no special casing. The redefinition brings `status.nodes` onto the same basis the static provisioning and deprovisioning controllers already use to satisfy `spec.replicas`, so a pool at its replica count now reports `nodes == spec.replicas` mid-rollout instead of dipping below it.
- **`do-not-disrupt` NodeClaims.** These can pin `upToDateNodes` below `nodes` indefinitely. This is a correct report, and the reason the API exposes counts rather than a boolean: consumers set a tolerance.
- **Hash version bumps across Karpenter upgrades.** Existing behavior already re-stamps NodeClaims that are not drifted; during the window `areStaticFieldsDrifted` returns `""` (version mismatch is not treated as drifted), so `compatibleWithNodePoolGeneration` still advances. Counts stay correct.
- **NodeClaims with no `nodepool-hash` annotation** (e.g. adopted/hydrated from an older version): `areStaticFieldsDrifted` returns `""` when annotations are missing, so the stamp would advance. Worth confirming against the hydration controller before implementation; if hydration lags, we should treat missing annotations as *not* compatible so the conservative direction holds.
- **Existing NodeClaims after the CRD upgrade.** `compatibleWith*` is unset (`0`) until the first disruption reconcile. Counts read conservatively low for one pass per NodeClaim; disruption watches NodePool/NodeClass and lists every NodeClaim, so the window is one controller queue drain, not "until the node next drifts."
- **Shared NodeClass.** NodeClass counts sum NodeClaims across NodePools. A NodeClass used by two pools reports complete only when both have rolled the NodeClass spec. Each NodePool reports complete when *its* NodePool spec has rolled, independently of the AMI axis.
- **Out-of-band AMI concurrent with a non-drifting NodeClass edit.** `IsDrifted` is still true (`AMIDrift`), so `compatibleWithNodeClassGeneration` is not advanced even though the spec change itself would not have required replacement. The GitOps gate stays open until those nodes are replaced. Conservative; listed under [Edge Cases](#edge-cases).

### Observability

Status is only half the answer. The complementary metric work in #3177 only needs one addition: label the drift condition with its reason, which the NodeClaim condition already carries (`SetTrueWithReason(ConditionTypeDrifted, driftedReason, ...)`).

```
karpenter_nodepools_nodeclaim_condition{nodepool, condition, status, reason}
```

That single label is what gives use case 4 the differentiated view — `reason="NodePoolDrifted"` vs `reason="AMIDrift"` vs `reason="NodeClassDrifted"` — without putting the taxonomy in the API. Reason values are a bounded, provider-defined set, so cardinality is manageable.

### Edge Cases

- **NodeClaim created from the previous revision, not yet in the informer cache.** A NodeClaim launched from spec `G` concurrently with the update to `G+1` can be briefly invisible, letting the pool report `20/20` before flipping back to `20/21`. The window is bounded by watch latency, and the consequence is a transient Healthy → Progressing flap rather than a stuck-Healthy. All other cache-lag directions are conservative: an unobserved new NodeClaim is stamped with the current generation (up to date), a stale cached entry for a deleted outdated NodeClaim only makes the pool look less complete.
- **Empty NodePool / NodeClass.** All four counts are `0` and the condition is `True`. Consumers that need "nonempty and settled" check `nodes > 0` themselves.
- **Unready nodes that are not part of a rollout.** A NodeClaim stuck launching for unrelated reasons holds `readyNodes` below `nodes` with no rollout in flight. This is the same shape as a Deployment with a crash-looping pod: the count is accurate and the consumer's tolerance decides whether it blocks. The existing `NodeRegistrationHealthy` condition remains the signal for a NodePool that cannot launch nodes at all.
- **NodePool with `Ready: False`** (bad NodeClass reference): NodePool counts still reported against the NodePool stamp; the existing `Ready` condition is the signal for that failure mode. The NodeClass counter has nothing to patch if the object is missing; those NodeClaims are absent from NodeClass counts until the reference resolves.
- **Rapid successive edits.** Each apply bumps `metadata.generation` (G → G+1 → G+2). The Lua check fails `observedGeneration == metadata.generation` until the counter has run against the *latest* generation, so it cannot pass on counts computed for G+1 after G+2 has landed. Once the counter has observed G+2, the stamps still have to catch up: disruption either advances them (the latest spec does not require replacement) or leaves them at an earlier generation until replacements exist. The gate stays Progressing through both windows; it does not go Healthy on an intermediate revision.
- **Non-drifting NodeClass spec change while nodes are dynamically drifted (e.g. AMI alias).** `IsDrifted` stays true, so the NodeClass stamp does not advance and the GitOps gate waits for replacement. Conservative relative to the spec change, and the replacement was already going to happen.
- **KWOK / providers where `IsDrifted` always returns `""`.** The NodeClass stamp advances whenever `IsDrifted` is empty, so on these providers every NodeClass spec edit advances every stamp on the next disruption reconcile. `upToDateNodes` equals `nodes` immediately after that — there is no NodeClass-driven replacement to wait for, which is correct: KWOK has no AMI / `userData` / equivalent. The NodeClass GVK still gets `observedGeneration` (so a starved counter cannot pass) and the Ready counts (launch/init still matter). NodePool stamps are unaffected; they still follow the NodePool hash/requirements checks.

## Alternatives Considered

**Aggregate the existing `Drifted` condition onto the NodePool.** Directly what the issue requested and what #3108 implemented. Rejected as the primary mechanism for the two reasons above: the union semantics make the number mean different things in different clusters, and the condition is written asynchronously with a 5-minute requeue and no revision anchor, so it cannot support a correct gate. The revision comparison is strictly more precise for this use case and strictly cheaper to compute.

**A NodePool-level `Drifted` condition only.** Simple boolean gate, but forces all-or-nothing semantics — no tolerance for a single `do-not-disrupt` node — and carries the same union ambiguity. The proposed `NodesUpToDate` condition provides the boolean for consumers that want it, defined against the revision instead of the union.

**Have the NodePool counter re-parse hashes itself.** The first revision of this RFC. Rejected in favor of the NodeClaim stamps: the disruption controller already has the drift inputs (including `cloudProvider.IsDrifted`), already watches NodeClass, and is the component that *knows* whether a NodeClass spec change requires replacement. Putting `compatibleWithNodeClassGeneration` on the NodeClaim is what lets a NodeClass counter exist at all without a cloud-provider interface change to categorize drift reasons.

**Use `nodeClassObservedGeneration` plus `IsDrifted` as a boolean on the NodePool, with no NodeClaim stamps.** `nodeClassObservedGeneration` bumps for every NodeClass spec change, including ones that do not require replacement, so combining it with a NodeClaim `Drifted==True` boolean either (a) treats non-drifting spec changes as rollouts until something else clears Drifted, or (b) treats dynamically drifted nodes as in-rollout even when generation did not change. The two generation stamps distinguish those cases by construction.

**Fold NodeClass compatibility into NodePool `upToDateNodes`, skip NodeClass status.** Insufficient for the Argo use case, and the wrong GVK besides. Argo health-checks the object that changed. An Application that applies an AMI pin to `EC2NodeClass` and does not include the NodePool would have no signal. Bumping NodePool `observedGeneration` on NodeClass edits would also make a starved NodePool counter look current. NodeClass `observedGeneration` is the linearizability trick for that GVK; Application-level AND of the two health checks is the composition.

**Metrics only (#3177).** Argo CD health checks and kro `readyWhen` cannot read Prometheus; the evaluation sandbox sees one resource. Metrics are complementary, not a substitute — hence both.

**ControllerRevision-based accounting, like DaemonSet.** Materializing revisions would give richer history (which revision each NodeClaim belongs to, rollback support) but introduces a new persisted object per revision and a garbage collection story, for information the generation stamp already encodes.

## Backward Compatibility

The new NodeClaim fields, NodePool fields, and condition are additive and read-only, and no YAML needs to change. Users must apply the updated CRDs to see the fields, per the usual Karpenter CRD upgrade path. `NodesUpToDate` is not part of the `Ready` aggregate, so `Ready` semantics are unchanged for existing consumers.

NodeClass status fields are similarly additive on each provider CRD. Providers that have not yet added them simply do not implement `NodeClassWithRolloutStatus`; core skips them. KWOK ships the fields in the same change as the NodePool API so the in-tree provider is a complete example.

`status.nodes` is the one field whose meaning changes. It is read-only, so nothing breaks on the wire, but its value shifts: it now includes NodeClaims that have not launched and NodeClaims that are terminating, so it reads higher than before during provisioning and disruption and is unchanged for a steady-state pool.

Inlining `NodeRolloutStatus` also preserves the existing JSON path and ordinary Go selectors such as `nodePool.Status.Nodes`. It is a source-level change for Go callers that use keyed composite literals: `NodePoolStatus{Nodes: value}` must become `NodePoolStatus{NodeRolloutStatus: NodeRolloutStatus{Nodes: value}}`. Karpenter does not use such literals internally, but external Go consumers may need this mechanical update.

## Graduation Criteria

No feature gate proposed. The change is only additive, read-only, computed from data Karpenter already maintains, and has no effect on provisioning or disruption behavior. The main risk is API shape, which is what this RFC is for.

Provider NodeClass fields can trail the core NodeClaim/NodePool fields. AMI / `userData` / other NodeClass spec rollouts become visible to Argo on the NodeClass GVK when the provider adds the CRD fields (AWS/Azure/GCP follow-up PRs). Until then, the NodeClaim stamps are already queryable and KWOK is the in-tree example. A NodePool health check does **not** wait for NodeClass compatibility, so an AMI Application must health-check the NodeClass GVK.

## Open Questions

1. **Should `status.nodes` and `status.resources["nodes"]` be reconciled rather than allowed to diverge?** The proposal keeps `status.resources` on cluster state and moves only `status.nodes`. Keeping both on the NodeClaim basis would be more internally consistent but would make `status.resources` report the capacity of nodes that are draining or have not launched.

## References

- Issue: [Surface NodeClaim drift/rollout progress in NodePool status (#3071)](https://github.com/kubernetes-sigs/karpenter/issues/3071)
- Prior implementation attempts: [#3108](https://github.com/kubernetes-sigs/karpenter/pull/3108) (status), [#3177](https://github.com/kubernetes-sigs/karpenter/pull/3177) (metrics)
- Maintainer feedback this RFC responds to: [#3071 (comment)](https://github.com/kubernetes-sigs/karpenter/issues/3071#issuecomment-5170006562), [#3216 (NodeClass generation)](https://github.com/kubernetes-sigs/karpenter/pull/3216#discussion_r3857866911), [#3216 (AMI / Argo)](https://github.com/kubernetes-sigs/karpenter/pull/3216#discussion_r3876142714), [#3216 (NodeClaim stamps)](https://github.com/kubernetes-sigs/karpenter/pull/3216#discussion_r3898578138)
- Drift semantics: [`designs/drift.md`](./drift.md), [`designs/drift-hash-versioning.md`](./drift-hash-versioning.md)
- Precedent: Deployment `status.updatedReplicas`/`observedGeneration`; Cluster API `MachineDeployment.status.upToDateReplicas` and the Machine `UpToDate` condition; [`kstatus`](https://github.com/kubernetes-sigs/cli-utils/tree/master/pkg/kstatus)
