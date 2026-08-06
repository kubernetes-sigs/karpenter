# NodePool Rollout Status

## Motivation

Karpenter exposes drift per-NodeClaim, via the `Drifted` status condition. There is no aggregate signal on the NodePool that answers the question an operator or an external orchestrator asks after pushing a change: *"has this NodePool finished rolling out the spec I just applied?"*

Answering it today requires listing NodeClaims, grouping them by `karpenter.sh/nodepool`, and aggregating their conditions. Every consumer that gates on a single resource's status — Argo CD health checks, kro `readyWhen` expressions, `kstatus`, `kubectl wait` — is structurally unable to do that, because their evaluation is scoped to the one resource they are looking at. The workaround is an out-of-band job that re-implements the aggregation Karpenter already performs internally ([#3071](https://github.com/kubernetes-sigs/karpenter/issues/3071)).

Core workload controllers solve this by reporting rollout accounting on the parent: a Deployment reports `replicas`/`updatedReplicas`/`readyReplicas` plus `observedGeneration`, a DaemonSet reports `desiredNumberScheduled`/`updatedNumberScheduled`, and Cluster API reports `upToDateReplicas` on MachineDeployments alongside an `UpToDate` condition on Machines. `kubectl rollout status` and essentially all GitOps tooling are built on that convention. NodePool already aggregates `status.nodes` and `status.resources`; this RFC extends that aggregation to rollout progress.

Earlier attempts at this are [#3108](https://github.com/kubernetes-sigs/karpenter/pull/3108) and [#3177](https://github.com/kubernetes-sigs/karpenter/pull/3177). The difference from these is that we do not propose surfacing "drift" on the NodePool. We propose surfacing *how many NodeClaims were provisioned from the NodePool's current spec revision*. Drift is the mechanism that eventually makes those numbers converge; the revision is the contract consumers gate on.

### Use Cases

1. **GitOps sync sequencing (Argo CD).** A NodePool is deployed by an Argo Application. A custom Lua health check must report `Progressing` while the change propagates and `Healthy` once it has, so that downstream Applications sync in order. The check can read only `NodePool.status`.
2. **Composite APIs (kro).** A `ResourceGraphDefinition` wraps NodePool + NodeClass into one custom API and evaluates `readyWhen` CEL against the resources in the graph. NodeClaims are not in the graph — their names and count are not known at authoring time — so the instance flips to ready as soon as the NodePool is admitted rather than when the replacement completes.
3. **Threshold-based gates.** Both of the above want tolerance ("settled at ≥ 90% up to date") rather than an all-or-nothing boolean, because disruption budgets make node rollout deliberately gradual and a single stuck node should not block a pipeline indefinitely.
4. **Fleet-wide rollout dashboards.** "Are we fully onto the new AMI?" — a time-series question over *all* drift vectors, including out-of-band ones. This is a different question from 1–3 and we suggest using metrics rather than status. See [Which drift vectors count](#which-drift-vectors-count).

### Non-Goals

- **Aggregating arbitrary NodeClaim conditions onto the NodePool.** #3108 proposed a generic `status.nodeClaimConditions[]` list of `{conditionType, count}`. That makes an open, provider- and version-extensible set of condition types part of the NodePool API with no defined semantics per entry, and no way to version or deprecate individual entries. We propose named fields with defined meanings instead.
- **Policy in the API.** No thresholds, settling windows, or "rollout paused/complete" state machine. Karpenter reports counts; consumers apply their own policy.
- **Changing disruption behavior.** Nothing here gates, throttles, or reorders drift.
- **Covering cloud-provider and out-of-band drift in v1 counts.** See [Extending beyond NodePool-attributable drift](#extending-beyond-nodepool-attributable-drift) for the path to adding it.

## Proposal

### Proposed Spec

Five additive, read-only fields on `NodePool.status`:

```yaml
apiVersion: karpenter.sh/v1
kind: NodePool
metadata:
  name: default
  generation: 12
status:
  observedGeneration: 12          # NEW: the generation the counts below were derived from
  nodeClaims: 20                  # NEW: NodeClaims owned by this NodePool
  upToDateNodeClaims: 14          # NEW: of those, provisioned from the current NodePool revision
  readyNodeClaims: 18             # NEW: NodeClaims whose Ready condition is True
  upToDateAndReadyNodeClaims: 12  # NEW: NodeClaims that are both
  nodeClassObservedGeneration: 4  # exists today
  nodes: 20
  resources: {...}
  conditions:
    - type: NodeClaimsUpToDate    # NEW
      status: "False"
      reason: RolloutInProgress
      message: 14/20 NodeClaims are up to date
      observedGeneration: 12
```

```go
type NodePoolStatus struct {
    // ... existing fields ...

    // ObservedGeneration is the generation of the NodePool spec that the NodeClaim counts
    // below were computed against. Consumers gating on rollout progress must ignore those
    // counts when this does not equal metadata.generation.
    // +optional
    ObservedGeneration int64 `json:"observedGeneration,omitempty"`

    // NodeClaims is the count of NodeClaims owned by this NodePool, including NodeClaims
    // that have not yet registered a Node and NodeClaims that are terminating.
    // +kubebuilder:default:=0
    // +optional
    NodeClaims *int64 `json:"nodeClaims"`

    // UpToDateNodeClaims is the count of NodeClaims owned by this NodePool that were
    // provisioned from the NodePool's current spec.template revision. The difference
    // between NodeClaims and UpToDateNodeClaims is the number of NodeClaims that Karpenter
    // will replace to complete the rollout of the current NodePool spec.
    // +kubebuilder:default:=0
    // +optional
    UpToDateNodeClaims *int64 `json:"upToDateNodeClaims"`

    // ReadyNodeClaims is the count of NodeClaims owned by this NodePool whose Ready
    // condition is True, meaning they have launched, registered a Node, and initialized.
    // +kubebuilder:default:=0
    // +optional
    ReadyNodeClaims *int64 `json:"readyNodeClaims"`

    // UpToDateAndReadyNodeClaims is the count of NodeClaims owned by this NodePool that are
    // counted by both UpToDateNodeClaims and ReadyNodeClaims. A rollout of the current
    // NodePool spec is complete when this equals NodeClaims.
    // +kubebuilder:default:=0
    // +optional
    UpToDateAndReadyNodeClaims *int64 `json:"upToDateAndReadyNodeClaims"`
}
```

Plus one status condition, `NodeClaimsUpToDate`, `True` when `upToDateNodeClaims == nodeClaims`, with `observedGeneration` set. The name is chosen over Deployment's `Progressing` because `Progressing` inverts the polarity of every other Karpenter condition, where `True` is the settled state. It is deliberately **not** added to the NodePool's `Ready` aggregate (`status.NewReadyConditions(ConditionTypeValidationSucceeded, ConditionTypeNodeClassReady)`) — a pool mid-rollout is healthy, not unready, and folding this in would silently change `Ready` semantics for every existing consumer.

Consumers gate as follows. Argo CD:

```lua
if obj.status.observedGeneration ~= obj.metadata.generation then
  return { status = "Progressing", message = "NodePool status is stale" }
end
if obj.status.upToDateAndReadyNodeClaims < obj.status.nodeClaims * 0.9 then
  return { status = "Progressing", message = "rolling out" }
end
return { status = "Healthy" }
```

"Drifted by a NodePool change" is `nodeClaims - upToDateNodeClaims`. The issue asked for a `driftedNodeClaims` field directly; the positive framing names what is being counted — agreement with a revision — rather than a union of unrelated causes, and matches the Deployment and Cluster API precedent consumers already know.

A rollout gate needs both revision agreement and workload readiness, which is why `upToDateNodeClaims` and `readyNodeClaims` are reported separately in the same way Deployment separates `updatedReplicas` from `readyReplicas`. Karpenter creates a replacement before terminating the node it replaces, so there is a window near the end of a rollout where every remaining NodeClaim is up to date but the newest ones have not registered a Node or initialized yet. A gate on `upToDateNodeClaims` alone would report Healthy during that window and allow the next Argo Application in the overall sequence to sync prematurely.

`upToDateAndReadyNodeClaims` is reported because that intersection cannot be derived from the other two counts. Knowing that 14 of 20 NodeClaims are up to date and 18 of 20 are ready says nothing about how many are both: anywhere from 12 to 14, depending on whether the unready NodeClaims are the new ones or the ones still awaiting replacement. Those two cases mean opposite things — a rollout nearly finished versus one that has barely started replacing unhealthy nodes — and a consumer restricted to a single resource's status cannot tell them apart.

`status.nodes` does not substitute for any of this: it is derived from cluster state and counts a NodeClaim as soon as it reports capacity, before the Node registers or initializes, so it cannot distinguish "coming up" from "ready". It also excludes NodeClaims marked for deletion, which `nodeClaims` includes, so the two are not a sound numerator and denominator for the same gate.

### Which drift vectors count

[The `Drifted` condition is the union of several independent causes, and collapsing that union into a NodePool-level number produces a signal that means different things at different times.](https://github.com/kubernetes-sigs/karpenter/issues/3071#issuecomment-5170006562)

| Drift vector | Detected by | Counts against `upToDateNodeClaims`? |
|---|---|---|
| NodePool `spec.template` static fields | `areStaticFieldsDrifted` (hash compare, core) | **Yes** |
| NodePool `spec.template.spec.requirements` | `areRequirementsDrifted` (label compatibility, core) | **Yes** |
| NodeClass spec change | `cloudProvider.IsDrifted` (e.g. `NodeClassDrifted`) | No (v1) |
| Out-of-band cloud provider change (new AMI, capacity reservation reshuffle, ...) | `cloudProvider.IsDrifted` | No |
| Instance type no longer offered | `instanceTypeNotFound` (core) | No |

The narrow definition makes the signal correct for use cases 1–3. In a fleet with frequent out-of-band drift (AMI releases on a weekly cadence, reservation churn), a gate keyed on the drift union never closes, so a GitOps pipeline sequenced on it stalls forever on changes unrelated to what was pushed. Conversely, use case 4 wants the union, and it is a time-series question that metrics answer better than status does. The split emphasizes that:

- **Status** answers "did the declarative change I applied finish propagating?" — narrow, revision-anchored, level-triggered, consumable by single-resource evaluators.
- **Metrics** answer "what is the state of drift across my fleet right now and over time?" — the full union, sliced by reason. That is #3177's job, extended with a `reason` label ([Observability](#observability)).

### How It Works

A NodeClaim is up to date with respect to a NodePool at generation `G` when, evaluated against the NodePool spec at `G`:

1. `nodeClaim.annotations["karpenter.sh/nodepool-hash-version"]` equals `v1.NodePoolHashVersion` **and** `nodeClaim.annotations["karpenter.sh/nodepool-hash"]` equals `nodePool.Hash()`, and
2. `areRequirementsDrifted(nodePool, nodeClaim)` returns `""`.

Both are the existing NodePool-attributable halves of `Drift.isDrifted`. NodeClaims whose hash version does not match the current one are counted as not up to date; that window is transient (the `nodepool.hash` controller re-stamps them) and erring toward "still rolling out" is the safe direction.

A NodeClaim is ready when its `Ready` status condition is `True`, which the NodeClaim API already defines as the roll-up of `Launched`, `Registered`, and `Initialized`. No new readiness definition is introduced; `readyNodeClaims` is a count of an existing per-NodeClaim signal.

The `nodepool.counter` controller computes the counts, rather than a new controller: it already reconciles every NodePool on a 5s requeue and already owns a status patch, so the marginal cost is one NodeClaim list per pass and the counts land atomically alongside `status.resources`/`status.nodes` instead of racing a second writer. The change is to list the NodePool's NodeClaims (`nodeclaimutils.ListManaged(ctx, client, cloudProvider, nodeclaimutils.ForNodePool(name))`), bucket them, and write all five new fields in the same status patch. Listing NodeClaims rather than walking cluster state is deliberate: it includes NodeClaims that have not yet registered a Node.

Two details make the result trustworthy:

- **Compute the hash, don't read the annotation.** The counter calls `nodePool.Hash()` on the object it is reconciling rather than reading `nodePool.annotations["karpenter.sh/nodepool-hash"]`. The annotation is written by a separate controller, so reading it opens a window where `metadata.generation` is already `G+1` while the annotation still holds the `G` hash — the counter would then report "everything up to date" and stamp `observedGeneration: G+1`, passing a gate prematurely. Deriving the hash from the spec in hand closes that window by construction.
- **Write `observedGeneration` in the same patch as the counts.** The counts and the generation they were derived from must never be observable independently.

### Linearizability

The other concern in #3108: counts reconciled asynchronously can be arbitrarily stale under CPU starvation or client throttling, so a gate can pass on numbers computed before the change landed. Edge detection ("the drifted count went up") does not fix it, because a NodePool update is not guaranteed to induce drift at all — restricting requirements to prune instance types that were never in use bumps the generation and drifts nothing.

Anchoring the counts to the generation resolves both halves:

| Scenario | `metadata.generation` | Status after counter runs | Consumer sees |
|---|---|---|---|
| Spec change that drifts nodes | `G+1` | `observedGeneration: G+1`, `14/20` | Progressing |
| Same, counter starved | `G+1` | stale `observedGeneration: G` | Progressing (generation mismatch) |
| Spec change that drifts nothing (`limits`, pruned unused requirements) | `G+1` | `observedGeneration: G+1`, `20/20` | Healthy immediately — correct, no rollout was needed |
| Rollout completes | `G+1` | `observedGeneration: G+1`, `20/20` | Healthy |

The third row is the case that defeats edge-triggered designs and that a level-triggered, revision-anchored count handles for free: the counter recomputes up-to-dateness every pass, so "nothing needed to change" and "everything already changed" are indistinguishable.

### Interaction with Existing Features

- **Disruption budgets / drift back-off.** Unchanged. Budgets and back-off govern *how fast* `upToDateNodeClaims` converges; they do not change what is counted. A pool that is backed off or budget-blocked simply reports incomplete for longer.
- **Terminating NodeClaims.** A NodeClaim with a deletion timestamp still counts in `nodeClaims` until it is gone. If it is outdated, the pool keeps reporting incomplete until the replacement is in place — desirable for a rollout gate.
- **Static NodePools (`spec.replicas`).** Same accounting applies; no special casing.
- **`do-not-disrupt` NodeClaims.** These can pin `upToDateNodeClaims` below `nodeClaims` indefinitely. This is a correct report, and the reason the API exposes counts rather than a boolean: consumers set a tolerance.
- **Hash version bumps across Karpenter upgrades.** Existing behavior already re-stamps NodeClaims that are not drifted; during the window the counts read conservatively low.
- **NodeClaims with no `nodepool-hash` annotation** (e.g. adopted/hydrated from an older version): counted as not up to date, consistent with the conservative direction. Worth confirming against the hydration controller's behavior before implementation.

### Observability

Status is only half the answer. The complementary metric work in #3177 only needs one addition: label the drift condition with its reason, which the NodeClaim condition already carries (`SetTrueWithReason(ConditionTypeDrifted, driftedReason, ...)`).

```
karpenter_nodepools_nodeclaim_condition{nodepool, condition, status, reason}
```

That single label is what gives use case 4 the differentiated view  — `reason="NodePoolDrifted"` vs `reason="AMIDrift"` vs `reason="NodeClassDrifted"` — without putting the taxonomy in the API. Reason values are a bounded, provider-defined set, so cardinality is manageable.

Additionally we could also add:

- `karpenter_nodepools_nodeclaims{nodepool}`, `karpenter_nodepools_uptodate_nodeclaims{nodepool}`, `karpenter_nodepools_ready_nodeclaims{nodepool}`, and `karpenter_nodepools_uptodate_and_ready_nodeclaims{nodepool}` gauges mirroring the status fields, so the same gate can be alerted on ("pool has been < 90% up to date and ready for 2h").
- An event on the NodePool when `NodeClaimsUpToDate` transitions, so `kubectl describe nodepool` shows rollout start/finish.
- Optional printer column: `ROLLOUT  12/20`, from `upToDateAndReadyNodeClaims` over `nodeClaims`.

### Edge Cases

- **NodeClaim created from the previous revision, not yet in the informer cache.** A NodeClaim launched from spec `G` concurrently with the update to `G+1` can be briefly invisible, letting the pool report `20/20` before flipping back to `20/21`. The window is bounded by watch latency, and the consequence is a transient Healthy → Progressing flap rather than a stuck-Healthy. All other cache-lag directions are conservative: an unobserved new NodeClaim is by definition up to date, a stale cached entry for a deleted outdated NodeClaim only makes the pool look less complete.
- **Empty NodePool.** All four counts are `0` and the condition is `True`. Consumers that need "nonempty and settled" check `nodeClaims > 0` themselves.
- **Unready NodeClaims that are not part of a rollout.** A NodeClaim stuck launching for unrelated reasons holds `readyNodeClaims` below `nodeClaims` with no rollout in flight. This is the same shape as a Deployment with a crash-looping pod: the count is accurate and the consumer's tolerance decides whether it blocks. The existing `NodeRegistrationHealthy` condition remains the signal for a NodePool that cannot launch nodes at all.
- **NodePool with `Ready: False`** (bad NodeClass reference): counts still reported; the existing `Ready` condition is the signal for that failure mode.
- **Rapid successive edits.** Each bumps the generation; the gate stays open until the counter observes the latest one.

## Alternatives Considered

**Aggregate the existing `Drifted` condition onto the NodePool.** Directly what the issue requested and what #3108 implemented. Rejected as the primary mechanism for the two reasons above: the union semantics make the number mean different things in different clusters, and the condition is written asynchronously with a 5-minute requeue and no revision anchor, so it cannot support a correct gate. The revision comparison is strictly more precise for this use case and strictly cheaper to compute.

**A NodePool-level `Drifted` condition only.** Simple boolean gate, but forces all-or-nothing semantics — no tolerance for a single `do-not-disrupt` node — and carries the same union ambiguity. The proposed `NodeClaimsUpToDate` condition provides the boolean for consumers that want it, defined against the revision instead of the union.

**Metrics only (#3177).** Argo CD health checks and kro `readyWhen` cannot read Prometheus; the evaluation sandbox sees one resource. Metrics are complementary, not a substitute — hence both.

**ControllerRevision-based accounting, like DaemonSet.** Materializing revisions would give richer history (which revision each NodeClaim belongs to, rollback support) but introduces a new persisted object per revision and a garbage collection story, for information the existing hash annotation already encodes.

### Extending beyond NodePool-attributable drift

Several reporters will eventually want the gate to cover NodeClass changes too — Argo syncs the NodePool and the NodeClass in the same Application, so "my change finished rolling out" arguably spans both. Core cannot compute that today: NodeClass up-to-dateness is provider-specific (the AWS provider stamps its own `karpenter.k8s.aws/ec2nodeclass-hash` on NodeClaims) and reaches core only as an opaque `DriftReason` string.

The clean extension is to categorize drift reasons at the cloud provider boundary — for example having `IsDrifted` return a reason plus a category (`StaticNodeClass` vs `Dynamic`) — which would let core fold static NodeClass drift into `upToDateNodeClaims` and, separately, improve the metric labeling. That is a cloud-provider interface change affecting every provider, so it is proposed as follow-up work rather than a prerequisite. `status.nodeClassObservedGeneration` already exists and gives consumers a partial NodeClass-side signal in the meantime.

## Backward Compatibility

All five fields and the condition are additive and read-only; no existing field changes meaning and no YAML needs to change. Users must apply the updated CRDs to see the fields, per the usual Karpenter CRD upgrade path. `NodeClaimsUpToDate` is not part of the `Ready` aggregate, so `Ready` semantics are unchanged for existing consumers.

## Graduation Criteria

No feature gate proposed. The change is only additive, read-only, computed from data Karpenter already maintains, and has no effect on provisioning or disruption behavior — comparable to `status.nodeClassObservedGeneration`, which shipped ungated. The main risk is API shape, which is what this RFC is for.

## Open Questions

1. **Is `observedGeneration` alone sufficient, or should the status also echo the `nodepool-hash`?** The generation is the conventional anchor and is what `kstatus` checks, but the hash is what up-to-dateness is actually computed against. Echoing both is cheap and would let consumers distinguish "spec changed but not in a drift-relevant way."

## References

- Issue: [Surface NodeClaim drift/rollout progress in NodePool status (#3071)](https://github.com/kubernetes-sigs/karpenter/issues/3071)
- Prior implementation attempts: [#3108](https://github.com/kubernetes-sigs/karpenter/pull/3108) (status), [#3177](https://github.com/kubernetes-sigs/karpenter/pull/3177) (metrics)
- Maintainer feedback this RFC responds to: [#3071 (comment)](https://github.com/kubernetes-sigs/karpenter/issues/3071#issuecomment-5170006562)
- Drift semantics: [`designs/drift.md`](./drift.md), [`designs/drift-hash-versioning.md`](./drift-hash-versioning.md)
- Precedent: Deployment `status.updatedReplicas`/`observedGeneration`; Cluster API `MachineDeployment.status.upToDateReplicas` and the Machine `UpToDate` condition; [`kstatus`](https://github.com/kubernetes-sigs/cli-utils/tree/master/pkg/kstatus)
