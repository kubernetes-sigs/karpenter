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
  unrecoverable failure. All back-off state transitions happen only there; the
  synchronous selection path in `Drift.ComputeCommands` only *reads* back-off state.
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
   back-off level and window.

### Back-off state

```go
// NodePoolBackoff tracks per-NodePool drift replacement back-off.
type NodePoolBackoff struct {
	mu    sync.Mutex
	clock clock.Clock
	state map[string]*backoffEntry // keyed by NodePool name
}

type backoffEntry struct {
	level int       // number of consecutive unrecoverable failures (0 == healthy)
	until time.Time // no candidates selectable before this time
}
```

- `level == 0` means the NodePool is healthy: normal drift selection applies.
- `until` is the earliest time the pool may be selected again.
- After `until` has passed while `level > 0`, the pool is in a **probe** state: it may
  select **at most one** in-flight drift replacement until the next success or
  failure resolves it.

### Back-off formula

On each unrecoverable failure, grow the window exponentially and clamp it to a single
absolute ceiling (`maxDelay`):

```
level := level + 1
until := now + min(baseDelay * 2^(level-1), maxDelay)
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

- **Success** (`cmd.Succeeded == true`): `backoff.Reset(nodePool)`.
- **Unrecoverable failure**: `backoff.Fail(nodePool)`.

Per the single-NodePool-per-command [invariant](#invariants), the NodePool key is
`cmd.Candidates[0].NodePool.Name`. We guard with
`cmd.Reason() == v1.DisruptionReasonDrifted` so consolidation/emptiness commands do
not touch drift back-off state.

Recoverable failures (which already `RequeueAfter: queueBaseDelay` and retry the same
command) do **not** change back-off state — the command is still in flight.

### Where back-off is enforced

In `Drift.ComputeCommands`, extend the per-candidate skip logic. Today it skips a
candidate only when the NodePool's disruption budget is exhausted. We add a back-off
check immediately after:

```go
for _, candidate := range slices.Concat(emptyCandidates, nonEmptyCandidates) {
	if disruptionBudgetMapping[candidate.NodePool.Name] == 0 {
		continue
	}
	// NEW: skip pools that are backed off, or already have a probe in flight.
	if !d.backoff.Selectable(candidate.NodePool.Name, d.inflightDriftReplacements(candidate.NodePool.Name)) {
		continue
	}
	results, err := SimulateScheduling(...)
	// ... unchanged ...
	return []Command{cmd}, nil
}
```

`Selectable(nodePool, inflight)` returns:

- `true` if `level == 0` (healthy).
- `false` if `now < until` (backing off).
- Probe: if `now >= until` and `level > 0`, return `true` only when `inflight == 0`
  (cap the pool to a single in-flight drift replacement until it recovers).

`inflightDriftReplacements(nodePool)` counts drift commands currently in the `Queue`
for that NodePool. The `Queue` already indexes in-flight commands by providerID
(`ProviderIDToCommand`); so it can expose a small helper to count in-flight drift
replacements per NodePool. (The existing `queue.HasAny` check in `NewCandidate`
already prevents re-selecting a node that is actively in the queue; the probe cap is
about limiting *how many distinct* candidates from a recovering pool can be in flight
at once — one.)

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

Pseudocode for the tracker:

```go
func (b *NodePoolBackoff) Fail(nodePool string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	e := b.entry(nodePool)
	e.level++
	window := baseDelay << (e.level - 1) // baseDelay * 2^(level-1)
	if window > maxDelay || window <= 0 {
		window = maxDelay
	}
	e.until = b.clock.Now().Add(window)
	// Once the window has saturated at maxDelay, stop growing level so the shift
	// above can never overflow on a pool that fails indefinitely.
	if window == maxDelay {
		e.level = maxLevel
	}
}

func (b *NodePoolBackoff) Reset(nodePool string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.state, nodePool)
}

// Selectable reports whether a candidate from nodePool may be selected this pass,
// given the number of drift replacements currently in flight for that pool.
func (b *NodePoolBackoff) Selectable(nodePool string, inflight int) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	e, ok := b.state[nodePool]
	if !ok || e.level == 0 {
		return true // healthy
	}
	if b.clock.Now().Before(e.until) {
		return false // backing off
	}
	return inflight == 0 // probe: at most one in flight
}
```

Sequence for a persistently failing pool (`spark`) alongside a healthy younger pool
(`ingress`), with defaults `baseDelay = 1m`:

1. Pass N: `spark` oldest candidate selected → replacement launched.
2. Queue: ICE → unrecoverable → `Fail("spark")` → `level=1`, `until=now+1m`.
3. Passes N+1..: `spark` skipped (backing off). `ingress` candidates are now reached
   and serviced normally.
4. At `now+1m`: `spark` probes one candidate. If ICE again → `Fail` → `level=2`,
   `until=now+2m`. If it succeeds → `Reset("spark")`, pool healthy again.
5. Window grows `1m, 2m, 4m, 8m, 10m, 10m…` (clamped at `maxDelay`) until capacity
   returns.

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
  failures is unchanged (`level` stays `0`, `Selectable` always returns `true`).
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

- **Unit (tracker):** `Fail` growth/cap, `Reset`, `Selectable` transitions
  (healthy → backing off → probe → recovered), driven by a fake `clock.Clock`.
- **Unit (`Drift.ComputeCommands`):** with a backed-off NodePool, assert its
  candidates are skipped and a younger NodePool's candidate is selected instead;
  assert the probe allows exactly one in-flight replacement after expiry.
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
- **Fairness ordering (least-recently-serviced NodePool first) instead of / in
  addition to back-off.** Deliberately excluded from this RFC's scope. A maintainer
  raised a specific concern that fairness ordering could let disruption budgets be
  consumed by NodeClaims that are slower to disrupt (e.g. `do-not-disrupt` with
  termination-grace-period enabled), effectively trading one head-of-line problem for
  another. As of this writing the issue thread has not settled whether fairness
  should become a second, separate RFC/PR — the issue author raised a
  counter-scenario (a large, healthy NodePool with fast replacements starving a
  smaller one) suggesting fairness may still be needed for that case. This RFC
  intentionally stands alone as the back-off half of the two originally proposed
  fixes; fairness ordering is left to a follow-up RFC pending that discussion, so as
  not to block back-off (which addresses the reported production issue on its own)
  on an unresolved design question.

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
