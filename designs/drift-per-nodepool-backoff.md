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
during drift candidate selection. After the back-off expires, exactly one candidate
is allowed to "probe"; a successful replacement resets the back-off, and a failure
grows it exponentially (capped). The change is entirely in-memory, requires no
API/CRD changes, and preserves the existing selection contract: `Drift.ComputeCommands`
still returns at most one command per pass and still stops at the first schedulable
candidate.

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
  (`cmd.Candidates[0].NodePool.Name`). Back-off keys, `Fail`/`Reset` calls, and the
  probe cap all depend on this.
- **A drift command's outcome is observed exactly once**, in `Queue.Reconcile` via
  `CompleteCommand`, and is unambiguously either success (`cmd.Succeeded == true`) or
  unrecoverable failure. `level`/`until` transitions happen only there. The synchronous
  selection path in `Drift.ComputeCommands` mutates back-off state only by atomically
  claiming or releasing the single probe slot (`probing`), never `level`/`until`.
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
   NodePool is currently backed off. Allow a single probing candidate once the
   back-off window has elapsed.
3. **Reset on success.** A successful drift replacement clears the NodePool's
   back-off level, window, and reserved probe slot.

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
	level   int       // number of consecutive unrecoverable failures (0 == healthy)
	until   time.Time // no candidates selectable before this time
	probing bool      // a single probe replacement has been claimed and not yet resolved
}
```

- `level == 0` means the NodePool is healthy: normal drift selection applies.
- `until` is the earliest time the pool may be selected again.
- After `until` has passed while `level > 0`, the pool is in a **probe** state: it may
  select **at most one** in-flight drift replacement until the next success or
  failure resolves it. `probing` is the tracker's *own* record of that single in-flight
  probe: `TryClaim` sets it under the tracker's lock and returns a claim, so the cap is
  self-contained and does not require inspecting queue state.

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

- **Success** (`cmd.Succeeded == true`): `backoff.Reset(nodePool)` — clears level,
  window, and the reserved probe slot.
- **Unrecoverable failure**: `backoff.Fail(nodePool)` — grows the window and clears the
  reserved probe slot (a new probe may be claimed after the new window).
- **Failed to launch** (`StartCommand` errors, so the command never enters the queue
  and no success/failure will be observed): `backoff.Fail(nodePool)` for a drift
  command, so the reserved probe slot is released rather than leaked. `StartCommand`
  already performs cleanup (untaint, clear condition) on this path; releasing the probe
  slot belongs alongside it.

Per the single-NodePool-per-command [invariant](#invariants), the NodePool key is
`cmd.Candidates[0].NodePool.Name`. We guard with
`cmd.Reason() == v1.DisruptionReasonDrifted` so consolidation/emptiness commands do
not touch drift back-off state.

Recoverable failures (which already `RequeueAfter: queueBaseDelay` and retry the same
command) do **not** change back-off state — the command is still in flight.

### Where back-off is enforced

In `Drift.ComputeCommands`, extend the per-candidate skip logic. Today it skips a
candidate only when the NodePool's disruption budget is exhausted; the back-off gate
slots in immediately after — `TryClaim` returns whether the candidate may be selected,
plus a `claim` for the reserved probe slot:

```go
if disruptionBudgetMapping[candidate.NodePool.Name] == 0 {
	continue
}
// NEW back-off gate: healthy pools pass through claiming nothing; a backed-off pool is
// skipped; a pool whose window has expired reserves its single probe slot here.
claim, ok := d.backoff.TryClaim(candidate.NodePool.Name)
if !ok {
	continue
}
// ... existing SimulateScheduling + schedulability checks, unchanged ...
```

The claim carries one obligation: any path that abandons the candidate before it
becomes a command — unschedulable pods, `errCandidateDeleting`, or another simulation
error — must `claim.Release()` to free the reserved slot. A candidate that is returned
as a command keeps its slot reserved until the queue resolves it (see
[Where failures/successes are observed](#where-failuressuccesses-are-observed)).

`TryClaim(nodePool)` decides selectability as follows:

- **Selectable, nothing reserved** if `level == 0` (healthy).
- **Not selectable** if `now < until` (backing off).
- **Not selectable** if a probe is already in flight for the pool.
- **Selectable, reserves the single probe slot** if the window has expired
  (`now >= until`, `level > 0`) and no probe is in flight.

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

The tracker exposes three lock-guarded operations over the `backoffEntry` defined in
[Back-off state](#back-off-state). The state transitions are the design-relevant part;
the procedural details (locking, the `claim` handle) are left to the implementation PR.

- **`Fail(nodePool)`** — on an unrecoverable drift failure: increment `level`, recompute
  `until` per the [back-off formula](#back-off-formula) (exponential, clamped to
  `maxDelay`, then equal-jittered), and clear `probing`. `level` stops growing once the
  window saturates at `maxDelay`, so the exponent cannot overflow.
- **`Reset(nodePool)`** — on a successful drift replacement: delete the entry, returning
  the pool to healthy (`level == 0`, no window, no reserved probe).
- **`TryClaim(nodePool) → (claim, ok)`** — during selection: decide selectability and,
  for a pool whose window has expired, atomically set `probing` to reserve the single
  probe slot. The returned `claim` exposes `Release()` to free a reserved slot (a no-op
  for a healthy pool). Its decision is specified by the table in
  [Where back-off is enforced](#where-back-off-is-enforced).

`Fail` and `Reset` are the only transitions of `level`/`until`; `TryClaim` and `Release`
are the only transitions of `probing`.

Sequence for a persistently failing pool (`spark`) alongside a healthy younger pool
(`ingress`), with defaults `baseDelay = 1m`:

1. Pass N: `spark` oldest candidate selected → replacement launched.
2. Queue: ICE → unrecoverable → `Fail("spark")` → `level=1`, `until=now+1m`.
3. Passes N+1..: `spark` skipped (backing off). `ingress` candidates are now reached
   and serviced normally.
4. At `now+1m`: `spark` probes one candidate. If ICE again → `Fail` → `level=2`,
   `until=now+2m`. If it succeeds → `Reset("spark")`, pool healthy again.
5. The window grows `1m, 2m, 4m, 8m, 10m, 10m…` (clamped at `maxDelay`)
   until capacity returns; each actual window is equal-jittered to `[½w, w)`, so the
   values above are centers, not exact times.

This removes both symptoms: `spark` no longer holds the head of line every pass, and
launch attempts drop from "every pass" to "one per back-off window."

### Interaction with budgets and the existing timeout

- **Disruption budgets** are unchanged and still evaluated first; back-off is an
  additional skip condition layered on top.
- **Command timeout** in `waitOrTerminate` (`GetMaxRetryDuration`, `10m`–`1h`) already
  wraps a timed-out command as unrecoverable. Such timeouts will also trigger
  back-off, which is desirable: a pool whose replacements never initialize is exactly
  a pool we want to back off. If we want to distinguish ICE from timeouts later, the
  `Fail` call site can branch on error type; this RFC treats all unrecoverable drift
  failures uniformly for simplicity.

## Observability

- **Metric (counter):** `karpenter_voluntary_disruption_drift_backoffs_total`,
  labeled by NodePool — incremented on each `Fail`. Lets operators see which pools are
  backing off and how often.
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
  failures is unchanged (`level` stays `0`, `TryClaim` always returns selectable with
  nothing reserved).
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

- **Unit (tracker):** `Fail` growth/cap, `Reset`, and `TryClaim`/`Release` transitions
  (healthy → backing off → probe → recovered), driven by a fake `clock.Clock`. Inject a
  seeded `*rand.Rand` so jitter is deterministic; assert each window lands in `[½w, w)`
  and that two pools failed at the same instant with the same `level` get *different*
  `until` values (de-synchronization).
- **Unit (tracker, probe cap):** after the window expires, a first `TryClaim` succeeds
  and reserves the probe; a second `TryClaim` for the same pool returns `false` until
  the claim is `Release`d or a `Fail`/`Reset` resolves it. Assert a claimed-but-released
  slot is re-claimable. This exercises the single-probe concurrency invariant.
- **Unit (`Drift.ComputeCommands`):** with a backed-off NodePool, assert its
  candidates are skipped and a younger NodePool's candidate is selected instead;
  assert exactly one probe is emitted after expiry; and assert that a candidate
  abandoned after claiming (unschedulable / `errCandidateDeleting`) releases the probe
  slot so the pool is not wedged.
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
