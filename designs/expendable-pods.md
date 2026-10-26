# Expendable Pods

Let cluster operators mark pods below a priority threshold as expendable. Karpenter never launches capacity for expendable pods, and they don't stop consolidation.

## Motivation

Cluster Autoscaler (CA) has `--expendable-pods-priority-cutoff`, default `-10`, as the [pod-preemption design](https://github.com/kubernetes/design-proposals-archive/blob/main/scheduling/pod-preemption.md#interactions-with-cluster-autoscaler) intended ([kubernetes/autoscaler#411](https://github.com/kubernetes/autoscaler/pull/411)). Pods below it "don't trigger scale-ups" and "don't prevent scale-downs" ([CA FAQ](https://github.com/kubernetes/autoscaler/blob/master/cluster-autoscaler/FAQ.md#how-does-cluster-autoscaler-work-with-pod-priority-and-preemption)). That makes a common pattern cheap: run low-priority work, often called backfill, in the gaps that bin-packing leaves on existing nodes, and let higher-priority pods preempt it when they need the room.

Karpenter ignores priority when deciding whether a pod needs capacity (`IsProvisionable`, `IsReschedulable`). As a result, for a backfill pod:

- if the kube-scheduler can't place it, Karpenter launches a node for it, and the operator pays for capacity to run work that could have waited for spare room;
- when Karpenter drains its node, the replacement is sized to include it;
- consolidation can't remove its node unless it fits elsewhere, which backfill rarely does;
- each time it binds to a node or leaves it, the node's `consolidateAfter` timer restarts, so a node running churning backfill never becomes consolidatable.

This RFC addresses [#1396](https://github.com/kubernetes-sigs/karpenter/issues/1396).

### Use Cases

1. **Batch backfill.** An IndexedJob at priority `-100` runs in bin-packing gaps, and preempted indexes re-run later. Today every index that doesn't fit launches a node, adding cost for work that could have run later in spare capacity.
2. **Migrating from Cluster Autoscaler.** CA's cutoff is on by default at `-10`. A cluster that moves to Karpenter silently starts provisioning for every pod below `-10`, and there is no configuration to restore the old behavior.

### Non-Goals

- **A per-NodePool cutoff.** It could be layered on the global one later without conflicting with it.

## Proposal

Add a global setting for a priority cutoff. A pod whose `spec.priority` is strictly below the cutoff is **expendable**, and Karpenter:

1. never provisions capacity for it;
2. never sizes replacement capacity for it when a node is replaced (drift, repair, expiration, interruption, or a consolidation that replaces a node);
3. does not need it to fit elsewhere for consolidation to remove its node, and treats a node whose only non-DaemonSet pods are expendable as empty;
4. does not restart a node's `consolidateAfter` timer when it binds to the node or leaves it;
5. treats the room it holds as free when simulating where a removed node's pods will go.

Separately, Karpenter keeps a node the kube-scheduler has nominated a preempting pod to, and the room on it, until the pod binds or the nomination changes: an existing bug fix, proposed to ship first ([item 6](#6-nominated-pods-hold-their-node)).

The rule for which pods are expendable is the same as Cluster Autoscaler's: a strict comparison against `spec.priority`, a nil priority is never expendable, and the pod's `preemptionPolicy` is ignored.

### Proposed Spec

| Setting | CLI Flag | Helm value | Default | Description |
| --- | --- | --- | --- | --- |
| `EXPENDABLE_PODS_PRIORITY_CUTOFF` | `--expendable-pods-priority-cutoff` | `settings.expendablePodsPriorityCutoff` | `""` (unset) | Pods below this priority are expendable. Unset disables the behavior. Must be an int32, or Karpenter refuses to start. Set `-10` to match Cluster Autoscaler. |

Unset is equivalent to the int32 minimum (`-2147483648`), since no priority is below it.

### How It Works

A *candidate* is a node that consolidation is considering removing, and *moved pods* are the pods that would have to find a new place if a candidate or a node being drained went away.

#### 1. No provisioning for pending expendable pods

Karpenter ignores pending expendable pods: it doesn't launch capacity for them or post `Nominated` or `FailedScheduling` Events on them, and the [metric below](#observability) counts them instead. The kube-scheduler places them as usual when capacity frees up.

#### 2. No replacement capacity for expendable pods

Consolidation, drift and repair size replacements through `SimulateScheduling`, and the provisioner (`Provisioner.Schedule`) reschedules pods from nodes already being deleted, for example after expiration or interruption. Both drop expendable pods, so replacement NodeClaims are sized for non-expendable pods only.

#### 3. Expendable pods don't need to reschedule

- Expendable pods aren't added to a candidate's simulation or counted for emptiness, so a node running only expendable pods and DaemonSets is empty (edge cases [1](#edge-case-1) and [2](#edge-case-2)).
- Draining doesn't change: expendable pods are still evicted through the Eviction API, and PDBs and `karpenter.sh/do-not-disrupt` still apply ([Interaction with Existing Features](#interaction-with-existing-features)).

#### 4. Expendable pods don't restart `consolidateAfter`

The pod-events controller doesn't update a NodeClaim's `lastPodEventTime` when an expendable pod binds, starts terminating or goes terminal, just as it already ignores DaemonSet pods. Otherwise churning backfill keeps the node from becoming `Consolidatable`, stops consolidation from moving pods onto it, and makes commands that target it fail the 15-second validation.

#### 5. Expendable pods don't block consolidation elsewhere

Karpenter's scheduling simulations don't model preemption. This RFC adds it only for expendable pods, because they're the easy case: they don't need to reschedule ([item 3](#3-expendable-pods-dont-need-to-reschedule)), so when Karpenter simulates where moved pods go, it treats the resources held by expendable pods on existing nodes as free, unless the moved pod's `preemptionPolicy` is `Never` ([edge case 5](#edge-case-5)).

Pending pods still see this room as occupied: a pending pod reaches Karpenter only after the kube-scheduler has declined to preempt for it, and treating the room as free would leave it Pending forever, a known CA bug ([#6227](https://github.com/kubernetes/autoscaler/issues/6227)).

#### 6. Nominated pods hold their node

**An existing bug, fixed independently of the cutoff and proposed to ship first.**

- **The bug:** when the kube-scheduler preempts for a pending pod, it sets `status.nominatedNodeName` and waits for the victims to exit. Karpenter tracks only bound pods, so a node left with only terminating victims or expendable pods looks empty, and consolidation deletes it. It shows only when victims outlast `consolidateAfter` (default `0s`) and the 15-second validation, so it's rarely noticed; the fix's unit tests fail on `main`. It dates from [aws/karpenter-provider-aws#1051](https://github.com/aws/karpenter-provider-aws/pull/1051), which stopped provisioning for nominated pods without protecting their node ([aws/karpenter-provider-aws#1050](https://github.com/aws/karpenter-provider-aws/issues/1050)).
- **The fix:** Karpenter counts a nominated pod against its node, as Cluster Autoscaler does, and blocks consolidation and drift of that node. Repair is exempt, since a pod may never bind to an unhealthy node; forced expiration and interruption are unaffected.
- **How long it lasts:** until the pod binds, is deleted or starts terminating, or the kube-scheduler clears or moves the nomination. No timeout is needed: the kube-scheduler holds a nomination only while victims terminate, and resolves it on its next retry, at least every 5 minutes. Since Kubernetes 1.35 ([KEP-5278](https://github.com/kubernetes/enhancements/tree/master/keps/sig-scheduling/5278-nominated-node-name-for-expectation)) it also nominates a pod while it waits to bind, and the block protects that too. A nomination sticks only if its scheduler stops handling the pod, such as an uninstalled secondary scheduler.

### Interaction with Existing Features

- **Lost backfill work.** Because expendable pods are evicted from candidates and preempted on the nodes that receive moved pods, they restart more often than today. Work that can't checkpoint, such as an IndexedJob index, starts again. That trade is intended.
- **PDBs and `karpenter.sh/do-not-disrupt`.** Still honored for expendable pods, since an explicit per-pod instruction wins over a cluster-wide default, so either can block consolidation of their node ([Open Question 2](#open-question-2)).
- **`CapacityBuffer`.** Buffer virtual pods aren't subject to the cutoff, since a buffer explicitly requests capacity; they run at the lowest priority, so Karpenter applies the cutoff before adding them. Room held by expendable pods counts as buffer headroom ([item 5](#5-expendable-pods-dont-block-consolidation-elsewhere)) if the buffer's pod template could preempt them (its `preemptionPolicy` isn't `Never` and its PriorityClass isn't expendable); otherwise backfill would keep filling the headroom, and Karpenter would keep launching nodes to restore it. A real pod using this headroom waits for the backfill to exit.

### Observability

- The `karpenter_scheduler_pending_expendable_pods_count` gauge (alpha) counts pending expendable pods that Karpenter isn't provisioning for ([item 1](#1-no-provisioning-for-pending-expendable-pods)). It's separate from `karpenter_scheduler_ignored_pods_count`, which counts pods that fail Karpenter's validation.
- When the cutoff is set, Karpenter logs it at startup. At `V(1)` it also logs the pending expendable pods it isn't provisioning for ([item 1](#1-no-provisioning-for-pending-expendable-pods)), grouped per scheduling run and at most once per pod every 24 hours, like its other per-pod scheduling warnings. When a disruption command's replacement leaves out expendable pods ([item 2](#2-no-replacement-capacity-for-expendable-pods)), the command's log line includes how many.
- Karpenter doesn't start its pending-time clock for expendable pods, so they don't appear in `karpenter_pods_provisioning_scheduling_undecided_time_seconds` and don't inflate scheduling-latency alerts built on it.
- When a nomination blocks a node's disruption, Karpenter publishes the existing `DisruptionBlocked` event on the node, naming the pod, so operators can see why the node stays ([item 6](#6-nominated-pods-hold-their-node)).

### Edge Cases

With the cutoff set to `-10`, production pods at priority `0` or above, and backfill at `-100`:

1. <a id="edge-case-1"></a>**Production and backfill on one node.** A node runs 3 production pods and 5 backfill pods, and the production pods fit elsewhere. The node can be consolidated. All 8 pods are evicted, and the 5 backfill pods go `Pending` without being provisioned for. If the production pods don't fit elsewhere, the replacement is sized for 3 pods, not 8.
2. <a id="edge-case-2"></a>**Backfill-only node.** A node runs only backfill pods and DaemonSets. Consolidation treats it as empty. When the setting is first turned on, every such node becomes empty at once, and Empty disruption budgets limit how many are removed at a time; a temporary Empty budget can pace this first wave ([Open Question 3](#open-question-3)).
3. <a id="edge-case-3"></a>**Overprovisioning pause pods.** Pause pods conventionally run at priority `-10`, the lowest priority that still triggers scaling in CA. The comparison is strict, so these pods aren't expendable and still trigger provisioning, as they do under CA.
4. <a id="edge-case-4"></a>**Backfill with `preemptionPolicy: Never`.** This is common, since backfill rarely needs to preempt anything. The pod is still expendable: `preemptionPolicy` controls whether a pod may preempt *others*, not whether it may *be* preempted. (CA did otherwise from 1.30 until [#8314](https://github.com/kubernetes/autoscaler/pull/8314) reverted [#6577](https://github.com/kubernetes/autoscaler/pull/6577).)
5. <a id="edge-case-5"></a>**Moved pod preempts backfill.** A 16 CPU production pod runs on a candidate, and the only room for it is on a 32 CPU node holding a 30 CPU backfill pod. Consolidation deletes the candidate without a replacement, and once evicted, the production pod preempts the backfill pod and waits for it to exit. If the production pod's PriorityClass sets `preemptionPolicy: Never`, the candidate is kept.

## Alternatives Considered

### A pod annotation (`karpenter.sh/do-not-provision`)

This is explicit, but it needs changes to every workload, doesn't match Cluster Autoscaler, and adds a second signal that can disagree with the pod's priority. CA's maintainers discouraged the same request, pointing to a DaemonSet or the priority cutoff instead ([#3843](https://github.com/kubernetes/autoscaler/issues/3843)). Rejected; it could complement the cutoff later.

## Backward Compatibility

The setting is off by default and adds no API fields: unset, Karpenter behaves as today, and removing the setting and restarting restores today's behavior immediately. [Item 6](#6-nominated-pods-hold-their-node) is the exception: it's always on, and downgrading removes it.

## Graduation Criteria

No feature gate, since the setting is opt-in. [Item 6](#6-nominated-pods-hold-their-node) also ships without a gate, because it only delays deleting a node the kube-scheduler is using. The validation bar before merge is:

- unit tests for items [1](#1-no-provisioning-for-pending-expendable-pods) to [6](#6-nominated-pods-hold-their-node), including both `preemptionPolicy` cases of [edge case 5](#edge-case-5);
- kwok e2e tests for items [1](#1-no-provisioning-for-pending-expendable-pods) to [6](#6-nominated-pods-hold-their-node), covering edge cases [1](#edge-case-1) to [5](#edge-case-5);
- no regression in the existing scheduling benchmarks (`pkg/controllers/provisioning/scheduling/scheduling_benchmark_test.go`) with the setting off, and the cost with it on measured and reported in the implementation PR;
- documentation that states the cutoff, the strict comparison and the value for overprovisioning pods in one place, and recommends a short `terminationGracePeriodSeconds` for backfill ([edge case 5](#edge-case-5)) and no blocking PDBs or `do-not-disrupt` on it ([Open Question 2](#open-question-2)).

## Open Questions

1. <a id="open-question-1"></a>**Default:** stay disabled permanently, or adopt Cluster Autoscaler's `-10` as the default in a future major version, once the feature has matured? A hardcoded default was rejected: it would silently change behavior for users who rely on provisioning for negative-priority pods, and CA itself moved from `0` to `-10` only after learning pause pods must keep triggering scale-ups ([#1037](https://github.com/kubernetes/autoscaler/pull/1037)).
2. <a id="open-question-2"></a>**`do-not-disrupt` and PDBs on expendable pods:** honor them, or ignore them as CA's "killed without any consideration" suggests? Honoring them is safer, but then a single annotation can undo the feature for that node. Proposed: honor them.
3. <a id="open-question-3"></a>**Emptiness:** should a node with only expendable pods be Empty, Underutilized, or configurable, for operators who want backfill-only nodes removed more slowly than truly empty ones? Proposed: Empty, so it uses the Empty reason and budgets.
4. <a id="open-question-4"></a>**Naming:** reuse CA's name (`expendable-pods-priority-cutoff`) so operators migrating recognize it, or choose something that reads naturally in Karpenter (for example `--provisioning-priority-floor`)? Proposed: reuse CA's name.
5. <a id="open-question-5"></a>**Modeling preemption in simulations:** [item 5](#5-expendable-pods-dont-block-consolidation-elsewhere) treats expendable pods' room as free without modeling which node the kube-scheduler picks, victim-eligibility plugins, or pod groups preempted as a whole (PDBs don't matter: preemption honors them only on a best-effort basis). Is that close enough? Proposed: yes. A one-off misprediction costs one extra node; a systematic one, such as a plugin that always protects the backfill, repeats and evicts the moved pod each cycle, bounded only by `consolidateAfter` and disruption budgets.
