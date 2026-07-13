# Per-NodePool Exponential Back-off for Drift Disruption

## Summary

Karpenter's drift disruption loop can **indefinitely starve** some NodePools when a
different NodePool's replacements keep failing (most commonly `InsufficientCapacity`
/ ICE, but also un-evictable pods or otherwise-blocked scheduling). Because drift
candidates are sorted strictly by drift age (oldest first) and a failed replacement
is returned to the candidate set with its *original* drift timestamp, the same
doomed candidate is selected first on every reconcile pass. This both blocks younger
NodePools from making any progress and burns a continuous stream of wasted
`CreateFleet`/launch attempts against the cloud provider.

This RFC proposes a **per-NodePool exponential back-off** on unrecoverable drift
replacement failures. While a NodePool is backed off, its candidates are skipped
during drift candidate selection. Once the back-off window elapses the pool becomes
eligible again: a successful replacement resets the back-off, and a failure grows it
exponentially (capped, with jitter). Repeated failures *within* a single window do not
compound — `Fail` is a no-op while the pool is already backed off — so the escalation
tracks failed *windows*, not individual launch attempts. The change is entirely
in-memory, requires no API/CRD changes, and preserves the existing selection contract:
`Drift.ComputeCommands` still returns at most one command per pass and still stops at
the first schedulable candidate.

## Background

During a batched drift rollout, such as changing a shared AMI selector so that many
NodePools drift at once, the drift controller repeatedly picks the globally oldest
drifted candidate. The relevant selection logic sorts candidates strictly by the
`Drifted` status condition's `LastTransitionTime`:

```64:108:pkg/controllers/disruption/drift.go
	sort.Slice(candidates, func(i int, j int) bool {
		return candidates[i].NodeClaim.StatusConditions().Get(string(d.Reason())).LastTransitionTime.Time.Before(
			candidates[j].NodeClaim.StatusConditions().Get(string(d.Reason())).LastTransitionTime.Time)
	})

	emptyCandidates, nonEmptyCandidates := lo.FilterReject(candidates, func(c *Candidate, _ int) bool {
		return len(c.reschedulablePods) == 0
	})

	// Prioritize empty candidates since we want them to get priority over non-empty candidates if the budget is constrained.
	// ...
	for _, candidate := range slices.Concat(emptyCandidates, nonEmptyCandidates) {
		if disruptionBudgetMapping[candidate.NodePool.Name] == 0 {
			continue
		}
		results, err := SimulateScheduling(ctx, d.kubeClient, d.cluster, d.provisioner, d.clock, d.recorder, nil, candidate)
		// ...
		cmd := Command{
			Candidates:          []*Candidate{candidate},
			Replacements:        replacementsFromNodeClaims(results.NewNodeClaims...),
			Results:             results,
			PoolDisruptionCosts: computePoolDisruptionCosts([]*Candidate{candidate}),
		}
		return []Command{cmd}, nil
	}
	return []Command{}, nil
```

After a command is dispatched, the disruption controller requeues immediately, so
this selection runs many rapid passes during a rollout:

```166:183:pkg/controllers/disruption/controller.go
	// Attempt different disruption methods. We'll only let one method perform an action
	for _, m := range c.methods {
		c.recordRun(fmt.Sprintf("%T", m))
		success, err := c.disrupt(ctx, m)
		if err != nil {
			// ...
		}
		if success {
			return reconciler.Result{RequeueAfter: singleton.RequeueImmediately}, nil
		}
	}

	// All methods did nothing, so return nothing to do
	return reconciler.Result{RequeueAfter: pollingPeriod}, nil
```

Replacement orchestration is asynchronous, in the drift/disruption `Queue`. When a
replacement fails unrecoverably (e.g. the replacement NodeClaim was deleted after an
ICE, or the command timed out), the queue tears the command down and unmarks the
candidate for deletion, returning it to the candidate pool unchanged — crucially,
**with its original drift timestamp**:

```142:181:pkg/controllers/disruption/queue.go
func (q *Queue) Reconcile(ctx context.Context, nodeClaim *v1.NodeClaim) (reconcile.Result, error) {
	// ...
	if err := q.waitOrTerminate(ctx, cmd); err != nil {
		// If recoverable, re-queue and try again.
		if !IsUnrecoverableError(err) {
			return reconcile.Result{RequeueAfter: queueBaseDelay}, nil
		}
		// If the command failed, bail on the action.
		// ...
		log.FromContext(ctx).Error(multiErr, "failed terminating nodes while executing a disruption command")
	} else {
		log.FromContext(ctx).V(1).Info("command succeeded")
		cmd.Succeeded = true
	}
	q.CompleteCommand(cmd)
	return reconcile.Result{}, nil
}
```

```413:424:pkg/controllers/disruption/queue.go
// CompleteCommand fully clears the queue of all references of a hash/command
func (q *Queue) CompleteCommand(cmd *Command) {
	if !cmd.Succeeded {
		q.cluster.UnmarkForDeletion(lo.Map(cmd.Candidates, func(c *Candidate, _ int) string { return c.ProviderID() })...)
	}
	// Remove all candidates linked to the command
	q.Lock()
	defer q.Unlock()
	for _, c := range cmd.Candidates {
		delete(q.ProviderIDToCommand, c.ProviderID())
	}
}
```

**Why it starves.** Nothing about the failed candidate's candidacy changes: it is
still the oldest, still within its NodePool's budget, and no longer in the queue. On
the next (immediate) pass it is rebuilt, re-sorted oldest-first, and re-selected. A
NodePool whose oldest candidates keep failing fast therefore monopolizes the single
per-pass command forever, and younger NodePools are never serviced.

Notably, there is a precedent in the codebase for per-NodePool fairness state kept in memory
on a disruption method: `SingleNodeConsolidation.PreviouslyUnseenNodePools` tracks
NodePools skipped due to a per-pass timeout and prioritizes them on the next pass:

```37:51:pkg/controllers/disruption/singlenodeconsolidation.go
// SingleNodeConsolidation evaluates one node at a time for consolidation.
type SingleNodeConsolidation struct {
	consolidation
	PreviouslyUnseenNodePools sets.Set[string]
	validator                 Validator
}
```

This RFC follows the same pattern: a small, in-memory, per-NodePool state that
influences candidate selection, with no persistence and no API surface.

## Goals

- Stop a NodePool with persistently failing drift replacements from monopolizing the
  single per-pass drift command.
- Guarantee periodic retries so a backed-off NodePool recovers automatically (within
  one back-off cycle) once capacity returns.
- Preserve the existing `Drift.ComputeCommands` contract: ≤ 1 command per pass, stop
  at the first schedulable candidate.
- No API/CRD changes; no persisted state.

## Invariants

These are the assumptions this design relies on. If any of them stops holding, the corresponding part of the
design below must be revisited.

- **A drift command targets exactly one NodePool.** Today `Drift.ComputeCommands`
  emits a command with a single candidate, and every candidate belongs to exactly one
  NodePool, so a command maps unambiguously to one NodePool
  (`cmd.Candidates[0].NodePool.Name`). Back-off keys and `Fail`/`Reset` calls depend on
  this.
- **A drift command's outcome is observed exactly once**, in `Queue.Reconcile` via
  `CompleteCommand`, and is unambiguously either success (`cmd.Succeeded == true`) or
  unrecoverable failure. All `level`/`until` transitions happen only there (via
  `Fail`/`Reset`). The synchronous selection path in `Drift.ComputeCommands` only
  *reads* back-off state (`IsBackedOff`); it never mutates it.
- **`Drift.ComputeCommands` emits ≤ 1 command per pass and stops at the first
  schedulable candidate.** Back-off is layered as an additional per-candidate skip; it
  never walks past the first schedulable candidate nor batches commands. (This is the
  contract preserved in [Goals](#goals), restated here as a dependency of the
  enforcement logic.)
- **The `Drift` method instance and the shared `NodePoolBackoff` persist for the
  controller's lifetime.** Karpenter runs a single active disruption reconciler
  (singleton), so there is exactly one reader and no cross-replica coordination. In-
  memory state is therefore sufficient and authoritative for the running process. This is a performance/fairness
  optimization, not correctness-critical; discarding it (e.g. on restart) at worst
  re-attempts a failing pool once before backing off again.

## Proposal

Introduce a **per-NodePool drift back-off tracker**. Three interaction points:

1. **Observe outcomes (async, in the `Queue`).** When a drift command completes,
   record success or unrecoverable failure for the command's NodePool.
2. **Enforce back-off (sync, in `Drift.ComputeCommands`).** Skip candidates whose
   NodePool is currently backed off (`level > 0` and the window has not yet elapsed).
   Once the window elapses the pool is eligible again like any other.
3. **Reset on success.** A successful drift replacement clears the NodePool's
   back-off state, returning it to healthy.

### Back-off state

```go
// NodePoolBackoff tracks per-NodePool drift replacement back-off.
type NodePoolBackoff struct {
	mu    sync.Mutex
	clock clock.Clock
	rand  *rand.Rand // injectable so jitter is deterministic in tests
	state map[string]*backoffEntry // keyed by NodePool name
}

type backoffEntry struct {
	level int       // number of failed back-off windows (0 == healthy)
	until time.Time // pool is skipped during selection before this time
}
```

- `level == 0` means the NodePool is healthy: normal drift selection applies.
- `until` is the earliest time the pool may be selected again.
- While `level > 0` and `now < until`, the pool is **backed off** and all its
  candidates are skipped during selection.
- Once `until` elapses the pool is eligible again with no special handling: normal
  oldest-first selection resumes, bounded by the pool's existing disruption budget. If
  those attempts fail again, the *first* failure of the new cycle arms the next window
  and any concurrent failures from the same cycle are no-ops (see
  [Where failures/successes are observed](#where-failuressuccesses-are-observed)). This
  is what keeps `level` counting failed *windows* rather than individual attempts, and
  it means the tracker never needs to inspect queue state.

### Back-off formula

On each unrecoverable failure, grow the window exponentially, clamp it to a single
absolute ceiling (`maxDelay`), and apply randomized **jitter**:

```
level  := level + 1
window := min(baseDelay * 2^(level-1), maxDelay)
window := window/2 + rand[0, window/2)   // equal jitter: keeps a floor of window/2
until  := now + window
```

Proposed defaults (mirroring the existing retry-duration bounds in `queue.go`, which
already use `10m`–`1h`):

| Parameter        | Default | Rationale |
| ---------------- | ------- | --------- |
| `baseDelay`      | `1m`    | First retry after a short delay; fast enough to recover quickly if ICE clears, slow enough to stop the storm. |
| `maxDelay` (cap) | `10m`   | Absolute ceiling on the back-off window. Reached after ~4 consecutive failures (`1m → 2m → 4m → 8m → 10m`). |

These are proposed as **package constants** initially (consistent with
`minRetryDuration`/`maxRetryDuration` in `queue.go`), with the option to promote them
to controller flags later if operators need to tune them. No NodePool API field is
proposed.

### Where failures/successes are observed

The `Queue.Reconcile` unrecoverable/success branches are the single authoritative
place where a drift command's fate is known. We hook there, gated on the command's
reason so only drift is affected:

- **Success** (`cmd.Succeeded == true`): `backoff.Reset(nodePool)` — returns the pool to
  healthy.
- **Unrecoverable failure**: `backoff.Fail(nodePool)` — **no-op if the pool is already
  backed off** (`level > 0` and `now < until`); otherwise increment `level` and arm the
  next window. The no-op is what prevents a burst of failures from one cycle (e.g. every
  attempt a just-recovered pool made before the first failure landed) from over-inflating
  `level`.
- **Failed to launch** (`StartCommand` errors, so the command never enters the queue
  and no success/failure will be observed): also `backoff.Fail(nodePool)` for a drift
  command, so a launch failure arms back-off the same as a post-launch failure. Because
  `Fail` is idempotent within a window, no bookkeeping is needed to avoid double-counting.

Per the single-NodePool-per-command [invariant](#invariants), the NodePool key is
`cmd.Candidates[0].NodePool.Name`. We guard with
`cmd.Reason() == v1.DisruptionReasonDrifted` so consolidation/emptiness commands do
not touch drift back-off state.

Recoverable failures (which already `RequeueAfter: queueBaseDelay` and retry the same
command) do **not** change back-off state — the command is still in flight.

### Where back-off is enforced

In `Drift.ComputeCommands`, extend the per-candidate skip logic. Today it skips a
candidate only when the NodePool's disruption budget is exhausted; the back-off gate
slots in immediately after as a single read-only check:

```go
if disruptionBudgetMapping[candidate.NodePool.Name] == 0 {
	continue
}
// NEW back-off gate: skip candidates whose NodePool is currently backed off. Healthy
// pools and pools whose window has elapsed fall through to the unchanged logic below.
if d.backoff.IsBackedOff(candidate.NodePool.Name) {
	continue
}
// ... existing SimulateScheduling + schedulability checks, unchanged ...
```

`IsBackedOff(nodePool)` returns `true` iff `level > 0` and `now < until`; otherwise
`false` (healthy, or the window has elapsed). It is purely a read — selection never
mutates back-off state, so there is no cleanup obligation on the abandon paths
(`errCandidateDeleting`, unschedulable pods, simulation errors). The
number of attempts a just-eligible pool makes before its next failure re-arms the
window is naturally bounded by the pool's disruption budget.

### Wiring / ownership

The tracker must be updated by the `Queue` and read by the `Drift` method. Both are
constructed in `pkg/controllers/controllers.go`:

```97:105:pkg/controllers/controllers.go
	deviceAllocationController := deviceallocation.NewController(kubeClient)

	evictionQueue := terminator.NewQueue(kubeClient, recorder)
	disruptionQueue := disruption.NewQueue(kubeClient, recorder, cluster, clock, p)
	controllers := []controller.Controller{
		p,
		disruption.NewController(clock, kubeClient, p, cloudProvider, recorder, cluster, disruptionQueue, clusterCost),
```

Proposed approach: construct a single `*NodePoolBackoff` in `controllers.go` and
inject it into both `disruption.NewQueue(...)` and `disruption.NewController(...)`.
`NewController` threads it through `NewMethods` into `NewDrift`. This keeps a single
shared instance and avoids the `Drift` method needing a back-reference to the
`Queue`. The `Drift` struct gains a `backoff *NodePoolBackoff` field alongside its
existing `clock`, `cluster`, etc.:

```38:55:pkg/controllers/disruption/drift.go
// Drift is a subreconciler that deletes drifted candidates.
type Drift struct {
	kubeClient  client.Client
	cluster     *state.Cluster
	provisioner *provisioning.Provisioner
	recorder    events.Recorder
	clock       clock.Clock
}
```

The `Drift` method instance is created once (via `NewMethods` inside `NewController`)
and persists for the controller's lifetime, so the shared tracker is stable across
passes — the same property `PreviouslyUnseenNodePools` relies on.

## Detailed design

The tracker exposes two mutating operations plus a read, over the `backoffEntry`
defined in [Back-off state](#back-off-state).

- **`Fail(nodePool)`** — on an unrecoverable drift failure. **No-op if the pool is
  already backed off** (`level > 0` and `now < until`). Otherwise increment `level` and
  recompute `until` per the [back-off formula](#back-off-formula) (exponential, clamped
  to `maxDelay`, then equal-jittered). `level` stops growing once the window saturates
  at `maxDelay`, so the exponent cannot overflow.
- **`Reset(nodePool)`** — on a successful drift replacement: delete the entry, returning
  the pool to healthy (`level == 0`, no window).
- **`IsBackedOff(nodePool) → bool`** — read-only, called during selection: `true` iff
  `level > 0` and `now < until`.

`Fail` and `Reset` are the only state transitions; `IsBackedOff` never mutates.

Sequence for a persistently failing pool (`spark`) alongside a healthy younger pool
(`ingress`), with defaults `baseDelay = 1m`:

1. Pass N: `spark` oldest candidate selected → replacement launched.
2. Queue: ICE → unrecoverable → `Fail("spark")` → `level=1`, `until=now+~1m`.
3. Passes N+1..: `spark` skipped (backing off). `ingress` candidates are now reached
   and serviced normally.
4. At `until`: `spark` is eligible again. Over the next few passes it may launch up to
   its disruption budget worth of replacements before the first one fails. The **first**
   failure of this cycle → `Fail` → `level=2`, `until=now+~2m`; any concurrent failures
   from the same burst hit the no-op and do not further inflate `level`. If any
   replacement succeeds → `Reset("spark")`, pool healthy again.
5. The window grows `1m, 2m, 4m, 8m, 10m, 10m…` (clamped at `maxDelay`)
   until capacity returns; each actual window is equal-jittered to `[½w, w)`, so the
   values above are centers, not exact times.

This removes both symptoms: `spark` no longer holds the head of line every pass (it is
skipped for the whole window, so younger pools are serviced), and launch attempts drop
from "every pass" to "at most one disruption-budget's worth per back-off window."

### Interaction with budgets and the existing timeout

- **Disruption budgets** are unchanged and still evaluated first; back-off is an
  additional skip condition layered on top. The budget also bounds the wasted work per
  window: once a pool becomes eligible again, at most a disruption-budget's worth of
  doomed replacements can be in flight before the first failure re-arms the back-off.
- **Command timeout** in `waitOrTerminate` (`GetMaxRetryDuration`, `10m`–`1h`) already
  wraps a timed-out command as unrecoverable. Such timeouts will also trigger
  back-off, which is desirable: a pool whose replacements never initialize is exactly
  a pool we want to back off. If we want to distinguish ICE from timeouts later, the
  `Fail` call site can branch on error type; this RFC treats all unrecoverable drift
  failures uniformly for simplicity.

## Observability

- **Metric (counter):** `karpenter_voluntary_disruption_drift_backoffs_total`,
  labeled by NodePool — incremented each time a pool *enters or escalates* back-off (an
  effective `Fail`, i.e. not the within-window no-ops). Lets operators see which pools
  are backing off and how often.
- **Metric (gauge):** `karpenter_nodepool_drift_backoff_seconds` (or reuse the
  `nodepool` subsystem) — seconds remaining in the current back-off window per
  NodePool; `0` when healthy.
- **Event:** emit a NodePool/NodeClaim event when a candidate is skipped due to
  back-off (rate-limited), so `kubectl describe` surfaces "drift back-off until T
  (level N)" rather than silent no-ops. This mirrors the existing `disruptionevents.Blocked`
  pattern already used in `Drift.ComputeCommands`.
- **Logs:** a `V(1)` log line when a pool enters back-off and when it is reset,
  analogous to the single-node-consolidation "prioritizing nodepools" log.

## Backward compatibility & failure modes

- **No API/CRD changes.** Behavior for clusters that never hit unrecoverable drift
  failures is unchanged (`level` stays `0`, `IsBackedOff` always returns `false`).
- **Controller restart.** State is in-memory; a restart clears it. Worst case, the
  loop briefly re-attempts a failing pool once before backing off again — a
  transient blip, identical to how `PreviouslyUnseenNodePools` resets on
  restart.
- **NodePool deletion / rename.** Stale entries are pruned on `Reset`, and can be
  lazily garbage-collected (drop entries whose NodePool no longer appears in the
  candidate set for some interval). Stale entries are otherwise inert.
- **HA / multiple replicas.** Karpenter runs a single active disruption reconciler
  (singleton); there is no cross-replica coordination concern.

## Testing plan

- **Unit (tracker):** `Fail` growth/cap, `Reset`, and `IsBackedOff` transitions
  (healthy → backing off → eligible → recovered), driven by a fake `clock.Clock`.
  Assert that repeated `Fail` calls *within* one window increment `level` only once (the
  no-op), and that a `Fail` after `until` elapses increments again. Inject a seeded
  `*rand.Rand` so jitter is deterministic; assert each window lands in `[½w, w)` and that
  two pools failed at the same instant with the same `level` get *different* `until`
  values (de-synchronization).
- **Unit (`Drift.ComputeCommands`):** with a backed-off NodePool, assert its candidates
  are skipped and a younger NodePool's candidate is selected instead; assert the pool
  becomes selectable again once its window elapses.
- **Queue integration:** simulate an unrecoverable failure (replacement NodeClaim
  deleted, as in the ICE path) and assert `Fail` is invoked for the drift command's
  NodePool and *not* for consolidation commands.
- **Starvation regression (suite_test):** reproduce the issue scenario — several
  NodePools drifted at once, the oldest pool's replacements always ICE — and assert
  the youngest pool makes drift progress and that launch attempts for the failing
  pool are bounded per back-off window rather than per pass.

## Alternatives considered

- **Bump the failed candidate's drift `LastTransitionTime`.** Re-stamping the
  condition would deprioritize the candidate, but it mutates a user-visible status
  timestamp, is persisted, and pollutes drift-age semantics used elsewhere.
- **Finer-grained back-off buckets (per-NodeClaim / per-NodeClass, not just
  per-NodePool).** Raised in maintainer discussion on the issue: NodeClaim, NodePool,
  and NodeClass failures can each stem from a distinct class of coordinated failure
  (e.g. an AZ-scoped ICE affecting only some NodeClaims in a NodePool vs. a
  NodeClass-wide misconfiguration affecting every NodePool that references it).
  Deferred in favor of NodePool-level granularity for this RFC: it minimizes
  complexity, mirrors the existing `PreviouslyUnseenNodePools` precedent, and
  addresses the observed real-world failure modes (batched ICE, blocked scheduling)
  without additional state dimensions. Can be revisited if a use case emerges where
  NodePool-level tracking is too coarse (e.g. a single large NodePool spanning
  multiple AZs where only one AZ is capacity-constrained).

## Open questions

1. Should back-off parameters (`baseDelay`, `maxDelay`) be package
   constants, controller flags, or both? Proposed: constants now, flags if requested.
2. Should timeouts and ICE failures be weighted differently, or is uniform
   unrecoverable-failure handling sufficient for v1? Proposed: uniform for v1.
3. Should the tracker live on the `Queue` (as the failure observer) with `Drift`
   holding a reference, or be a standalone object injected into both? Proposed:
   standalone `*NodePoolBackoff` injected into both from `controllers.go`.
4. Is a per-NodePool skip event too noisy at scale, warranting rate-limiting or
   V(1)-only logging instead? Proposed: rate-limited event + V(1) log.

## Future work

- **Rotation within equivalence classes.** Today drift always selects the oldest
  candidate. If the oldest candidate in a pool is blocked for a candidate-specific
  reason, back-off keys on the whole NodePool and every retry re-selects that same
  doomed node. Rotating/randomizing the selected candidate *within an equivalence
  class* (candidates with interchangeable scheduling requirements) would let a pool's
  other candidates make progress while one stays stuck.
- **Cap probes to exactly one per window.** This RFC lets a just-eligible pool attempt
  up to a disruption-budget's worth of replacements before the first failure re-arms
  back-off. Capping that to a single "probe" per window would further cut wasted
  `CreateFleet` calls (from `≤ budget` to exactly 1) and shrink the transient
  global-concurrency and cordon blast radius — most valuable for large-budget pools,
  slow (timeout-class) failures that hold budget for the whole command timeout, or
  providers sensitive to launch throttling. It was deliberately dropped from v1 because
  it requires reserving a single in-flight probe slot (a `probing` flag plus a
  claim/release handle threaded through every non-dispatch path in `ComputeCommands` and
  the launch-failure path in the `Queue`), which is substantial complexity for a bounded,
  per-window efficiency gain. The window + no-op-`Fail` already provide the correctness
  guarantees (no indefinite starvation; escalation counts failed windows). Worth
  reconsidering if back-off metrics show the per-window bursts are problematic in
  practice.

## References

- Issue: [Drift disruption can indefinitely starve NodePools under sustained
  replacement failures (#3080)](https://github.com/kubernetes-sigs/karpenter/issues/3080)
- Interactive model: `karpenter-drift-animation.html` (attached to the issue) — the
  "Back-off only" mode corresponds to this proposal.
- Precedent: `SingleNodeConsolidation.PreviouslyUnseenNodePools`
  (`pkg/controllers/disruption/singlenodeconsolidation.go`).
- Related: [#2927 — RFC for time slicing disruption](https://github.com/kubernetes-sigs/karpenter/pull/2927),
  a broader, longer-term proposal to restructure disruption around fairness; this
  RFC is a narrower, interim fix that does not depend on it.
