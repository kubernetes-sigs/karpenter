# RFC: Formalizing the Node Termination Contract

## Summary

Node termination is one of Karpenter's oldest and most trafficked flows. Multiple entry points funnel into it — user `Node`/`NodeClaim` deletion, expiration, disruption (consolidation, drift), and health-driven forceful termination — and the same set of controllers coordinate through implicit, code-only conventions to drain pods, wait for volumes to detach, and terminate the underlying cloud instance. There is no single place that names the flow's inputs, its status-condition progression, or the guarantees it makes to callers.

This RFC formalizes that contract as a durable design document. The contract itself changes **no observable behavior**. It catalogs what the flow already does, sequences the status conditions it drives, enumerates its guarantees and edge cases, and disambiguates the ways the `karpenter.sh/nodeclaim-termination-timestamp` annotation gets set. It also proposes how that annotation migrates to a validated NodeClaim status field, including the naming and deprecation plan, so the plan is reviewed before any implementation PR. Its purpose is to make the flow legible enough that termination changes stop being expensive to review and grace-period correctness bugs stop being expensive to find.

---

## Motivation

### The flow has many callers and no single description

Every one of these paths ends up in the same `node/termination` controller finalizing the same Node object:

- `kubectl delete node` (or any client that deletes the Node directly)
- `kubectl delete nodeclaim` — `nodeclaim/lifecycle` deletes the backing Node
- The `nodeclaim/expiration` controller when a NodeClaim ages out
- The `disruption` controller (consolidation, drift) via NodeClaim deletion
- The `node/health` controller for forceful termination of unhealthy nodes

Each entry point makes slightly different assumptions about what the termination flow will do. The flow itself makes assumptions about what its callers have already done (annotations set? finalizers present? NodeClaim resolvable by provider ID?). Those assumptions are only visible by reading four packages side-by-side — `pkg/controllers/node/termination`, `pkg/controllers/node/termination/terminator`, `pkg/controllers/nodeclaim/lifecycle`, `pkg/controllers/node/health` — and reconstructing the invariants from scratch.

### The silent contract has produced recurring bugs

Grace-period correctness has bitten the project repeatedly because the interaction between the two `nodeclaim-termination-timestamp` writers (`nodeclaim/lifecycle` sets `deletionTimestamp + spec.terminationGracePeriod`; `node/health` overwrites to `now()`) was never formalized:

- [#3032](https://github.com/kubernetes-sigs/karpenter/issues/3032) — negative pod grace period when Node Repair triggers (fixed).
- [#3111](https://github.com/kubernetes-sigs/karpenter/issues/3111) — grace-period correctness under a related race (open).

Consolidation of the eviction/force-delete paths ([#3063](https://github.com/kubernetes-sigs/karpenter/pull/3063)) took multiple review rounds partly because reviewers had to re-establish the drain contract from the code each time before evaluating the change. That's overhead every future termination PR pays until the contract is written down.

### The next RFC in the area already depends on this one

Derek's node-repair RFC ([#3192](https://github.com/kubernetes-sigs/karpenter/pull/3192)) explicitly names this issue in its follow-ups: *"Termination contract (#3029) — repair coordinates with the termination controller through the `nodeclaim-termination-timestamp` annotation hack (with explicit optimistic-lock code to avoid a race). Formalizing a first-class termination contract resolves that and the related grace-period correctness bugs."* Landing this contract before the repair follow-ups makes those follow-ups smaller and safer.

---

## The contract

### 1. Trigger and inputs

The `node/termination` controller reconciles [managed](https://github.com/kubernetes-sigs/karpenter/blob/main/pkg/utils/node/node.go) Nodes but only takes action on a Node with both:

1. A non-zero `DeletionTimestamp`, and
2. The `karpenter.sh/termination` finalizer (`v1.TerminationFinalizer`).

Given such a Node, the flow's inputs are:

| input | type | source | may be absent? |
|---|---|---|---|
| Node | `*corev1.Node` | reconcile target | never |
| NodeClaim | `*v1.NodeClaim` | resolved via `Node.Spec.ProviderID` | yes (see edge cases) |
| `nodeTerminationTime` | `*time.Time` | parsed from `nodeclaim-termination-timestamp` annotation on the NodeClaim | yes; `nil` means unbounded graceful drain |

### 2. Sources of `nodeclaim-termination-timestamp`

The annotation encodes a **hard deadline** for graceful drain. Four code paths write it, with different intents:

| Writer | Value | When | Intent |
|---|---|---|---|
| `nodeclaim/lifecycle` | `DeletionTimestamp + spec.terminationGracePeriod` | On NodeClaim delete, only if `spec.terminationGracePeriod` is set | Operator-configured graceful deadline |
| `disruption` queue | `now() + candidate drain bound` | After required replacements are ready, immediately before deletion is requested for a candidate that carries a drain bound (node repair sets one) | Bound the drain of a repaired node without eroding the window while replacements launch |
| `nodeclaim/reboot` | `now() + reboot drain bound` | When a failed reboot escalates to replacement | The replacement inherits the reboot's committed drain bound |
| `node/legacyrepair` (behind `--legacy-node-repair`) | `now()` (RFC3339) | When an unhealthy Node's toleration period has elapsed | Immediate forced termination |

**Invariant (write ordering):**

- `nodeclaim/lifecycle` skips its write if the annotation is already set (it treats "annotation present" as "someone else got here first"). When it does write, it uses `client.MergeFromWithOptimisticLock`, so if `node/health` writes between `lifecycle`'s read and patch, `lifecycle`'s patch fails with a conflict and drops — no retry inside the write path; a subsequent reconcile will see the annotation exists and skip.
- `disruption` and `nodeclaim/reboot` go through `TightenTerminationTimestampAnnotation` (`pkg/utils/nodeclaim`), which skips the write when the existing value is already earlier than the new one.
- `node/legacyrepair` overwrites the annotation with `now()` **unless** the existing value is already in the past. A lifecycle-set future deadline can be replaced with `now()` (tighter); a past time is not re-written on later reconciles.

The same rule is therefore implemented three times (the shared helper, the lifecycle absence check, and the legacy repair check) and upheld by convention. Combined: **the annotation can only get tighter, never looser.** A `nil` deadline may become a concrete time; a future time may be replaced with an earlier one; a past time is never rewritten and never pushed further out.

The `spec.terminationGracePeriod` field itself may be `nil`, set directly on the NodeClaim, or inherited from the NodePool — resolution happens before this controller runs and is not this contract's concern.

### 3. Status condition progression

When a NodeClaim is present, the flow drives three conditions on it. When no NodeClaim is found (see edge cases), no conditions are written — there is nothing to patch.

```
[start of finalize()]
    Drained             : (nil)
       ↓ awaitDrain begins
    Drained             : Unknown (reason: "Draining")
       ↓ drainable pods evicted / force-deleted; MinDrainTime elapsed
    Drained             : True
       ↓ awaitVolumeDetachment begins
    VolumesDetached     : Unknown (reason: "AwaitingVolumeDetachment")
       ↓ VolumeAttachments removed by attach-detach controller
    VolumesDetached     : True
       ↓  (alternative: nodeTerminationTime elapses first)
    VolumesDetached     : False (reason: "TerminationGracePeriodElapsed")
       ↓ awaitInstanceTermination begins
    InstanceTerminating : True
       ↓ cloudprovider reports instance gone
[Node finalizer removed → NodeClaim lifecycle removes NodeClaim finalizer]
```

Only the first of these paths that requeues or errors runs each reconcile. The transitions are monotone: no condition ever moves backwards.

### 4. Guarantees

The flow guarantees the following for every callable path:

1. **PDB respect during graceful drain.** Pods are evicted through the Kubernetes eviction subresource (via the eviction queue), which honors PodDisruptionBudgets.
2. **Priority ordering.** Non-critical/non-DaemonSet pods drain before non-critical DaemonSet pods, which drain before critical/non-DaemonSet, which drain before critical/DaemonSet — matching [Kubernetes graceful node shutdown ordering](https://kubernetes.io/docs/concepts/cluster-administration/node-shutdown/).
3. **`terminationGracePeriod` is a hard deadline.** Once `nodeTerminationTime` has passed, pods whose grace period would extend past it are deleted directly (bypassing PDBs) and the volume-detachment wait is skipped. Termination always makes progress regardless of PDBs or lingering volume attachments.
4. **Delete-eligible pods bypass tier gating.** Pods past the force-delete threshold are enqueued across all priority tiers immediately; only graceful-eviction candidates are gated by tier. A PDB on a non-critical pod cannot hold up a past-deadline `system-critical` pod.
5. **Grace-period clamp is never zero.** When force-deleting a pod, the API call's `gracePeriodSeconds` is clamped to `max(nodeTerminationTime - now, 1)` — never `0`, which would violate at-most-one pod semantics via etcd force-deletion.
6. **Fast-path bypass.** If the Node's `Ready` condition is not `True` and the cloud provider reports the instance as `NotFound`, the finalizer is removed immediately without draining. There is nothing to drain to.
7. **Instance-termination idempotence.** `cloudprovider.Delete` is called repeatedly until it returns `NotFound`. Callers may call `Delete` any number of times without harm.
8. **Node finalizer removal is the last action.** The Node's `karpenter.sh/termination` finalizer is only removed after all three phases complete successfully (or a fast-path bypass fires). The NodeClaim's own finalizer removal is `nodeclaim/lifecycle`'s responsibility once `InstanceTerminating: True` is observed.

### 5. Edge cases

| Condition | Behavior |
|---|---|
| Node's `ProviderID` doesn't resolve to a NodeClaim | Termination proceeds with no NodeClaim; no status conditions are written; `awaitInstanceTermination` is a no-op (there is no NodeClaim to call `cloudprovider.Delete` on). The Node finalizer is still removed. |
| Multiple NodeClaims match one Node | Treated as *no* NodeClaim — there is no longer a single source of truth. Same behavior as above. |
| Instance already gone before drain (fast path) | If `Node.Status.Conditions[Ready] != True` AND `cloudprovider.Get` returns `NotFound`, the finalizer is removed immediately. No drain, no volume wait, no `awaitInstanceTermination`. |
| Pod has `pod.Spec.TerminationGracePeriodSeconds == nil` | K8s defaults this to 30s on admission, so nil is theoretical. If encountered, the force-delete threshold falls back to "is the pod past the node deadline?" only. |
| `nodeTerminationTime == nil` | No force-delete threshold; drain runs unbounded (subject only to `MinDrainTime` and PDB delays). |
| `nodeTerminationTime` in the past | Force-delete grace period clamps to `1s` (never `0`). Volume-detachment wait is skipped. |
| Two writers race to set the annotation | `nodeclaim/lifecycle` writes with an optimistic lock and drops on conflict; the tighten helper only writes an earlier value. The earlier-in-time value wins. |
| Do-not-disrupt pod on a past-deadline node | Force-deleted (the annotation bypasses eviction; the node-deadline bypass overrides pod-level `do-not-disrupt`). |
| Pod is already terminating (has its own `DeletionTimestamp`) | Force-deleted only if `DeletionTimestamp > nodeTerminationTime` (i.e., its existing grace period would extend past the node's deadline). Otherwise left to terminate naturally. |

---

## Non-goals

This RFC does not:

- **Change any observable behavior.** It is a documentation and legibility exercise.
- **Implement the annotation-to-field migration.** The plan is specified below so it can be reviewed here; the code lands in a follow-up PR.
- **Consolidate the writers into one controller.** The migration routes every writer through one helper, which is enough for the contract. Moving ownership into a single controller is orthogonal.
- **Design a `RebootRequest` or `TerminationRequest` API.** Those are tracked in [#3344](https://github.com/kubernetes-sigs/karpenter/issues/3344) and the discussion on [#3029](https://github.com/kubernetes-sigs/karpenter/issues/3029). This RFC only avoids foreclosing them.
- **Redesign the flow.** The flow works; the contract just needs to be legible.

---

## Migrating the annotation to a field

### Proposed shape

A `NodeClaim` status field holding the absolute deadline as a `metav1.Time`. It lives in `status` because only Karpenter controllers write it, and its value is derived from `spec.terminationGracePeriod` and from disruption decisions, so it records no user intent. The annotation is already documented as `InternalOnly` and `docs.Alpha` in `pkg/apis/v1/labels.go`, so there is no documented user-facing contract to preserve.

The tighten-only invariant moves from convention into the API through CRD validation: a transition rule that rejects a new value later than the old one, and a rule on the parent that rejects removing the field once set. Setting it for the first time stays allowed. The exact CEL expressions are verified in the implementation PR.

All writers go through one helper that sets the field and, during the migration window, the annotation.

### Naming

The current name says nothing about what happens at that time. What the value bounds, from the code:

- After the time passes, pods whose grace period would extend past it are deleted directly (bypassing PDBs and `karpenter.sh/do-not-disrupt`), and the volume-detachment wait is skipped.
- It does not bound `cloudprovider.Delete`; instance termination is retried until the provider reports `NotFound` (guarantee 7).

| Candidate | For | Against |
|---|---|---|
| `terminationDeadline` | Absolute-time counterpart of `spec.terminationGracePeriod`, which is documented as the maximum wait before forcefully deleting the pods on a node. Period and deadline read as a pair. | "Termination" can read as covering instance teardown, which has no deadline. |
| `drainDeadline` | Matches how the reboot controller names its `Drain` bound. | Understates the contract: the same time also ends the volume-detachment wait. |
| `terminationTimestamp` (current) | Already in use. | Does not say what happens at that time. |

Proposal: `terminationDeadline`, with the field documentation stating that it ends graceful drain and the volume wait, not instance termination. Listed under open questions.

### Deprecation plan

Release numbers are relative to the release that ships the field. The schedule is a proposal for maintainers to adjust.

| Release | Writers | Termination controller reads | Annotation docs |
|---|---|---|---|
| N | Write the field and the annotation | Field, falling back to the annotation | Marked deprecated |
| N+1 | Write the field only | Field, falling back to the annotation | Deprecated |
| N+2 | Write the field only | Field only | Removed, along with the `wellknown.Annotation` entry |

The field and the annotation are both written in N because a status patch with a field the installed CRD does not know is pruned silently, and a missing deadline means unbounded drain. Dual writing keeps the deadline effective if the controller upgrades before the CRD. Reads fall back to the annotation through N+1 because a NodeClaim that began deleting before the upgrade only carries the annotation, and a deletion can last as long as its `terminationGracePeriod`. Rollback from N+1 to N is supported. Rollback from N+1 to N-1 is not, because N-1 only reads the annotation.

### Compatibility with reboot and request objects

The reboot handoff today is the `Rebooting` condition plus the `karpenter.sh/reboot-termination-grace-period` annotation ([#3344](https://github.com/kubernetes-sigs/karpenter/issues/3344) proposes a first-class `RebootRequest`). The field is shaped so a later request object can resolve into it without changing the termination controller:

1. **Absolute time.** Request-scoped durations such as `reboot-termination-grace-period` or `spec.terminationGracePeriod` stay on whatever object owns the request. The field holds the resolved bound.
2. **One reader.** Termination reads only this field. A future request object writes it through the same helper.
3. **Ratchet in the API.** A new writer cannot loosen the bound, whatever object it comes from.
4. **No assumption that the NodeClaim is already deleting.** The reboot controller computes its drain deadline in memory from the request time and only writes this value when a failed reboot escalates to replacement. The field documentation therefore describes what happens once termination begins and does not tie the value to `deletionTimestamp`.

---

## Where the contract lives

This RFC (`designs/formalize-node-termination-contract.md`) is the primary reference — it captures the *why* and the exhaustive edge-case table, similar in role to Karpenter's other design docs.

A companion, shorter package-level Go doc (`pkg/controllers/node/termination/doc.go`) is proposed as a follow-up PR: a summary that a contributor grepping through the code will discover directly, with a link back to this document for the full contract. Landing the RFC first lets that follow-up be a small, mechanical addition rather than another design conversation.

---

## Follow-ups (not in this RFC)

- Package-level Go doc (`doc.go`) that summarizes this contract next to the code, deferring to this document for the exhaustive form.
- The implementation PR for the migration above: the field, validation, the shared write helper, and the dual-write and read-fallback behavior for release N.
- A separate contract for the `nodeclaim/lifecycle` finalizer removal — currently outside this document's scope but adjacent.

---

## Alternatives considered

**Status quo.** Rejected because contributors keep reconstructing the contract from code, and the resulting review cost and correctness bugs (#3032, #3111, #3063 review history) are ongoing.

**Package-level `doc.go` only, no RFC.** Rejected. `doc.go` is the right home for a concise, code-adjacent reference, but it's not the right home for the motivation, edge-case rationale, or cross-controller invariants — those are RFC material. The best answer is both, and the RFC comes first.

**RFC + code-refactor to make the contract self-evident.** Rejected as scope creep for a first PR here. Refactoring cross-controller coordination is the kind of change that needs the contract *first* to be reviewable. Landing the RFC unlocks that follow-up; bundling them makes both harder to review.

---

## Open questions

- Field name: `terminationDeadline` (proposed), `drainDeadline`, or keep `terminationTimestamp`.
- Release cadence for N, N+1, N+2, and whether Karpenter has a standing deprecation policy this should follow.
- Whether the reboot drain bound should also be stored in this field, or stay request-scoped until #3344 is designed.

---

## References

- [#3029](https://github.com/kubernetes-sigs/karpenter/issues/3029) — this RFC (tracking issue).
- [#3344](https://github.com/kubernetes-sigs/karpenter/issues/3344): Reboot, promote the disruption-to-reboot handoff to a first-class API.
- [#3192](https://github.com/kubernetes-sigs/karpenter/pull/3192) — RFC: Making Node Repair Voluntary; explicitly depends on this contract as a follow-up.
- [#3063](https://github.com/kubernetes-sigs/karpenter/pull/3063) — Consolidate pod force-deletion into the eviction queue; refactor whose review history exposed the murk being formalized here.
- [#3032](https://github.com/kubernetes-sigs/karpenter/issues/3032) — negative pod grace period when Node Repair triggers (fixed).
- [#3111](https://github.com/kubernetes-sigs/karpenter/issues/3111) — grace-period correctness under a related race (open).
- [Kubernetes graceful node shutdown](https://kubernetes.io/docs/concepts/cluster-administration/node-shutdown/) — the ordering invariant guarantee (2) mirrors.
