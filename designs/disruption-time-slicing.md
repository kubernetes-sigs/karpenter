# CFS-Based Disruption Method Scheduling

## Motivation

The disruption controller runs as a singleton that sequentially evaluates five methods in priority order: Emptiness, StaticDrift, Drift, MultiNodeConsolidation, SingleNodeConsolidation. When any method succeeds, the loop restarts from the top:

```go
for _, m := range c.methods {
    success, err := c.disrupt(ctx, m)
    if success { return RequeueImmediately }  // restarts entire loop
}
```

This means consolidation can be indefinitely starved when higher-priority methods continuously find work.

Users running continuous drift (e.g., frequent AMI updates, config changes) see consolidation never run. Drift processes one node per `ComputeCommands` call, the loop restarts, and consolidation never gets a turn.

### Use cases
1. We have AMI changes every few days and a large cluster with large node pools. This means we have pretty inconsistent consolidations when large number of nodes are drifted causing wastages when consolidation are effectively paused for 20 minutes to an hour.
2. Day to day we rely on Drift to cycle AMI, Node Class and Node Pool changes across our fleet and we don't need this to propagate quickly but we still need consolidation to run.
3. Some times we want fast Drift to rollout, such as during emergencies, in which case we could turn off consolidations and give Drift full budget. (We have enough control already for this)
4. This solution allows a way to reason about about changes to consolidation timeout. https://github.com/kubernetes-sigs/karpenter/issues/1733 

## Key Constraints

### Existing

1. **Each disruption action mutates cluster state.** Any disruption removing nodes changes utilization, pod placement, and scheduling feasibility. Consolidation evaluating against stale state produces suboptimal or invalid decisions. Methods should evaluate settled state, not mid-mutation state.
2. **Drift must repeat.** Unlike Emptiness and StaticDrift which batch all their work into a single call, Drift returns exactly one disruption per call. A loop that runs Drift once per cycle reduces drift throughput to ~1 node per cycle.
3. **Consolidation is slow.** MultiNode consolidation has a 1-minute timeout. SingleNode consolidation has a 3-minute timeout.
4. **Budgets are globally coupled.** `BuildDisruptionBudgetMapping` counts ALL `MarkedForDeletion()` nodes as "disrupting" regardless of reason. A drift budget of 10% means 10% of ALL nodes can be disrupting, and those disrupting nodes count against consolidation's budget too.
5. **Singleton controller, no parallelism.** The disruption controller runs as a single reconciler. Only one method runs at a time.

### New

1. **Node disruption budget and time must be balanced.** Node disruption budgets limit how many nodes can be disrupting simultaneously. Consolidation methods consume minutes per evaluation while drift takes seconds. A scheduling policy must account for both.

## Current Behavior

The disruption controller reconciles every **10 seconds** (`pollingPeriod = 10s`). Each reconciliation runs the method list sequentially and restarts the loop on the first success. This means the maximum throughput for any method is bounded by the 10s cycle time plus the method's own execution cost.

### Methods and their throughput characteristics


| Method      | Commands/call                  | Timeout | Throughput per cycle        |
| ----------- | ------------------------------ | ------- | --------------------------- |
| Emptiness   | 1 (batches all empty nodes)    | None    | All empty nodes             |
| StaticDrift | Multiple (1 per candidate)     | None    | All static-drifted nodes    |
| Drift       | 1 (first successful candidate) | None    | 1 node                      |
| MultiNode   | 1 (covers multiple nodes)      | 1min    | 1 multi-node consolidation  |
| SingleNode  | 1 (single node)                | 3min    | 1 single-node consolidation |


### Drift throughput

In the current loop, drift gets at most **1 node per 10s cycle** because a successful disruption restarts the loop. With continuous drift work, this means ~6 nodes/minute. This is adequate for small clusters but slow for large clusters with hundreds of drifted nodes. This is the motivation for time-capped drift in the proposed design, which allows drift to process multiple candidates within a single time-capped window.

## Design Options

### Option A: CFS-Based Adaptive Scheduling (Recommended)

The loop runs in two phases each cycle:

**Phase 1**: Emptiness and StaticDrift always run unconditionally. They are important and less complicated to process.

**Phase 2**: CFS scheduling selects one of Drift, MultiNode, or SingleNode based on druntime. Each disruption method tracks a `druntime` (disruption runtime consumed). The scheduler always picks the eligible method with the lowest druntime, guaranteeing fairness and priority.

When Drift is selected, it runs inside a **time-capped loop** (`disruptWithTimeCap`) rather than a single `disrupt()` call. Because Drift only processes one node per `ComputeCommands` call, a single call would waste most of its allotted time. The time cap lets Drift process multiple candidates back-to-back within a bounded window (e.g., 10s), matching its throughput to the time share CFS gives it. MultiNode and SingleNode do not need this because they already handle multiple candidates internally (binary search and linear scan respectively).

#### How the Loop Works in Practice

1. **Score Calculation**: Phase 1 completes (Emptiness, StaticDrift). Phase 2 begins. The controller picks the eligible disruption method (has budget > 0) with the lowest `druntime`.
2. **Execute & Measure**: The selected method runs. The controller records actual elapsed time.
3. **druntime Update**: `druntime += elapsed_seconds / weight`, **regardless of whether the method succeeded or not**. Controller time was consumed either way. A method that spends 180s calling SimulateScheduling on hundreds of candidates and finds nothing consolidatable should be charged the same as one that found work. Higher-weight methods accumulate druntime slower and get more turns. Because the selected method just consumed time, its druntime increased. Meanwhile, the other methods' druntimes stayed the same. Whichever is now lowest gets picked next.
4. **Adapt**: When a disrupt method returns false (no work), it's marked inactive and leaves the eligible set **in addition to being charged druntime for the time it consumed**. The druntime charge prevents a method that does expensive but fruitless work (e.g., SingleNode scanning hundreds of candidates via SimulateScheduling before timing out) from immediately getting another turn on reentry. On wakeup, the method gets `druntime = max(its current druntime, min(all druntimes))`. If it was recently charged a large druntime from a failed expensive evaluation, it keeps that charge rather than resetting to min.

#### Why This Solves the Problem

The disruption controller must balance three competing concerns: **node disruption budget** (limited number of nodes can be disrupting simultaneously), **time consumed per disruption request** (consolidation methods can take minutes per evaluation while drift takes seconds), and the **singleton constraint** (only one method runs at a time). CFS naturally balances all three:

- **Prevents disruption method starvation.** Consolidation is mathematically guaranteed to get a turn because its `druntime` will eventually be the lowest among eligible methods, even if Drift always has candidates.
- **Prevents time starvation.** No disruption method gets blocked forever by consecutive long-running evaluations, because a method's druntime increases proportionally to its execution time, naturally yielding the scheduler to others. Methods that consume more time per request are naturally scheduled less frequently.
- **Adapts to cluster conditions.** When a disruption method exhausts its candidates, it leaves the eligible set and remaining turns go to whoever is next lowest. When budget frees up, the starved method reenters with one fair turn.
- **Prevents idle monopolization.** A disruption method with no candidates is marked inactive and removed from the eligible set. Without this, a method with the lowest druntime but no work would be picked repeatedly every cycle, blocking other disruption methods. On reentry it gets one fair turn via the wakeup mechanism.

#### Detailed Design

**Per-method state:**

```go
type MethodState struct {
    druntime float64       // disruption runtime consumed (lower = higher priority)
    weight   float64       // higher weight = druntime grows slower = more turns
    active   bool          // was this method eligible last cycle
}
```

**Weights** follow CFS convention (like K8s cpu.weight): higher weight = more controller time = higher priority. The weight states how much controller time a method should get when all methods are contending.

Priority order: Drift > Multi > Single.


| Method              | Weight | Time share |
| ------------------- | ------ | ---------- |
| Drift (time-capped) | 3      | 50%        |
| MultiNode           | 2      | 33%        |
| SingleNode          | 1      | 17%        |


**Execution time analysis:**

The timeout values (Multi 1min, Single 3min) are worst-case bounds, not typical execution times. Both methods call `SimulateScheduling` per iteration. The difference is algorithmic:

- **MultiNode**: Binary search over candidates (capped at 100). ~7 SimulateScheduling calls max (log2(100)).
- **SingleNode**: Linear scan, returns on first valid candidate. Best case: 1 call. Worst case: scans all candidates before timeout. Only hits the 3-minute timeout on large clusters. The code has explicit recovery for this. `PreviouslyUnseenNodePools` tracks nodepools skipped due to timeout and prioritizes them next reconciliation.

**Typical-case analysis (10-minute window):**


| Method     | Weight | Time share | Time (s) | Example exec/turn | Turns in 10min |
| ---------- | ------ | ---------- | -------- | ----------------- | -------------- |
| Drift      | 3      | 50%        | 300s     | ~10s              | ~30            |
| MultiNode  | 2      | 33%        | 200s     | ~15s              | ~13            |
| SingleNode | 1      | 17%        | 100s     | ~15s              | ~6             |


**Worst-case analysis (all methods hitting timeouts):**


| Method     | Weight | Time (s) | Worst-case exec/turn | Turns in 10min |
| ---------- | ------ | -------- | -------------------- | -------------- |
| Drift      | 3      | 300s     | ~10s                 | ~30            |
| MultiNode  | 2      | 200s     | ~60s                 | ~3             |
| SingleNode | 1      | 100s     | ~180s                | ~0.56          |


**CFS self-corrects between these extremes.** When Single finishes fast (5s), its druntime barely increases and it gets another turn soon. When Single hits the 180s timeout, its druntime spikes, naturally pushing Drift and Multi to the front for many subsequent cycles. The controller shifts to cheaper methods automatically. Fast completion is rewarded with more turns. Slow completion is penalized with fewer.

**Cold start handling** (following Linux CFS):

- On controller startup: all druntimes initialize to 0. Ties broken by priority (Drift > Multi > Single).
- When a method transitions from ineligible to eligible (wakeup): `druntime = max(current druntime, min(all druntimes))`. If the method was recently charged a large druntime from a failed expensive evaluation, it keeps that charge. If it has been inactive long enough that its druntime is below the current min (due to normalization), it gets bumped up to min for one fair turn without a burst of accumulated "credit."
- When a method has no work (`disrupt()` returns false): charge druntime for elapsed time, then set `active = false`. The method leaves the eligible set. Next cycle, the cached budget mapping determines reentry. If the method's reason still has budget > 0 in any NodePool, it becomes eligible again and wakeup logic applies the max rule above. The druntime charge from the failed evaluation carries forward, so a method that burned significant time finding nothing will naturally be deprioritized relative to methods that haven't run recently. For consolidation methods, the existing `IsConsolidated()` gate provides additional protection: if no cluster state has changed, consolidation skips evaluation entirely regardless of CFS scheduling.

**Eligibility gating** (pre-check before scoring):

`BuildDisruptionBudgetMapping` is expensive: it scans all cluster nodes and makes an API call to list NodePools. Calling it separately for each disruption method's reason every cycle would triple the cost. Instead, compute budget mappings once per cycle and share the results:

1. At the start of Phase 2, compute `BuildDisruptionBudgetMapping` once for each distinct reason across the disruption methods (Drifted, Underutilized). Cache the results.
2. For each disruption method, check the cached mapping for its reason. A method is eligible if any NodePool in the mapping has budget > 0.
3. If zero eligible methods: return, requeue after polling period.

The cached mapping is also passed to the selected method's `disrupt()` call, so `BuildDisruptionBudgetMapping` is not recomputed inside `disrupt()`. This reduces the per-cycle cost from 3 budget computations (one per method) to at most 2 (one per distinct reason), while `GetCandidates` and `ComputeCommands` still run only for the selected method.

**druntime normalization**:

- After each cycle: subtract `min(all druntimes)` from all methods, so the lowest method resets to 0. This prevents unbounded growth. Without normalization, druntime values increase every cycle indefinitely. Since druntime is `float64`, large values cause precision loss in comparisons. A difference of 3.3 vs 30.0 is meaningful, but 1000003.3 vs 1000030.0 risks floating point noise affecting scheduling decisions. Normalization keeps values bounded. The maximum spread between any two methods is the largest single-turn increment (180 for SingleNode worst case at weight 1). Linux CFS solves this differently with `u64` nanosecond counters and signed wraparound arithmetic, but normalization is simpler and sufficient for our 3-method scheduler.

**This design is purely for time fairness.**

druntime measures controller time consumed, not nodes disrupted. A method disrupting 5 nodes in 10s gets `druntime += 10/3.0 = 3.3` (Drift, weight 3). A method disrupting 1 node in 60s gets `druntime += 60/2.0 = 30` (Multi, weight 2). The node counts don't factor in. This is intentional. Adding node consumption to druntime would conflate time fairness with budget concerns. A method could be penalized for consuming nodes even if it ran quickly. It also couples druntime to cluster size (disrupting 5 nodes in a 100-node cluster differs from 5 in a 1000-node cluster). Time already serves as a natural proxy because disrupting more candidates within `disruptWithTimeCap` takes more wall-clock time, so druntime increases proportionally.

#### Implementation

**1. Add method state to Controller** (`controller.go`):

```go
type Controller struct {
    // ... existing fields
    methodState map[string]*MethodState  // keyed by method type name
}
```

**2. Replace the sequential loop** (`controller.go:163-176`):

```go
// Phase 1: Always run Emptiness and StaticDrift (fast, batch)
for _, m := range []Method{c.emptiness, c.staticDrift} {
    c.recordRun(fmt.Sprintf("%T", m))
    success, err := c.disrupt(ctx, m)
    if err != nil { ... }
    if success { anySuccess = true }
}

// Phase 2: CFS scheduling for Drift, Multi, Single
// Compute budget mappings once per cycle, shared across methods
budgetsByReason := c.computeBudgetMappings(ctx)  // one BuildDisruptionBudgetMapping per distinct reason
eligible := c.getEligibleMethods(budgetsByReason)  // check cached mappings for budget > 0
if len(eligible) == 0 {
    if anySuccess { return RequeueImmediately }
    return RequeueAfter(pollingPeriod)
}

// Pick method with lowest druntime (ties broken by priority)
picked := c.pickLowestDruntime(eligible)
start := c.clock.Now()

var success bool
if picked == c.drift {
    success, err = c.disruptWithTimeCap(ctx, picked, driftTimeBudget)
} else {
    success, err = c.disrupt(ctx, picked)
}

elapsed := c.clock.Since(start).Seconds()
state := c.methodState[fmt.Sprintf("%T", picked)]

// Always charge druntime. Controller time was consumed regardless of outcome.
// A method that spends 180s on SimulateScheduling and finds nothing should not
// get a free pass to immediately try again.
state.druntime += elapsed / state.weight

if success {
    anySuccess = true
} else {
    // No work. Leave eligible set; wakeup next cycle via budget check
    state.active = false
}

c.normalizeDruntimes()
```

**3. Add `disruptWithTimeCap`** that calls `disrupt()` in a loop until the time budget is exhausted or no more candidates:

```go
func (c *Controller) disruptWithTimeCap(ctx context.Context, m Method, budget time.Duration) (bool, error) {
    anySuccess := false
    deadline := c.clock.Now().Add(budget)
    for c.clock.Now().Before(deadline) {
        success, err := c.disrupt(ctx, m)
        if err != nil { return anySuccess, err }
        if !success { break }
        anySuccess = true
    }
    return anySuccess, nil
}
```

**4. Budget computation and eligibility** in `computeBudgetMappings` and `getEligibleMethods`:

```go
// Compute budget mappings once per cycle, one per distinct reason.
func (c *Controller) computeBudgetMappings(ctx context.Context) map[v1.DisruptionReason]map[string]int {
    reasons := map[v1.DisruptionReason]bool{}
    for _, m := range []Method{c.drift, c.multiNode, c.singleNode} {
        reasons[m.Reason()] = true
    }
    result := map[v1.DisruptionReason]map[string]int{}
    for reason := range reasons {
        mapping, err := BuildDisruptionBudgetMapping(ctx, c.cluster, c.clock, c.kubeClient, c.cloudProvider, c.recorder, reason)
        if err != nil { ... }
        result[reason] = mapping
    }
    return result
}

// Check cached budget mappings and apply wakeup logic.
func (c *Controller) getEligibleMethods(budgetsByReason map[v1.DisruptionReason]map[string]int) []Method {
    var eligible []Method
    for _, m := range []Method{c.drift, c.multiNode, c.singleNode} {
        state := c.methodState[fmt.Sprintf("%T", m)]
        hasBudget := c.hasBudgetForMethod(budgetsByReason, m)
        if hasBudget {
            if !state.active {
                // Wakeup: preserve druntime charge from expensive failed evaluations.
                // Only bump up to min if current druntime has fallen below (due to normalization).
                state.druntime = max(state.druntime, c.minDruntime())
                state.active = true
            }
            eligible = append(eligible, m)
        } else {
            state.active = false
        }
    }
    return eligible
}

// Check if any NodePool has budget > 0 for this method's reason.
func (c *Controller) hasBudgetForMethod(budgetsByReason map[v1.DisruptionReason]map[string]int, m Method) bool {
    mapping := budgetsByReason[m.Reason()]
    for _, budget := range mapping {
        if budget > 0 {
            return true
        }
    }
    return false
}
```

### Option B: Static Multi-Rate Cadences

Model the disruption loop as a multi-rate scheduler with fixed frequencies. Emptiness and StaticDrift always run first. The remaining slot is allocated based on elapsed time since each method last ran:

```
Every cycle (~10s): Empty → StaticDrift → Drift (time-capped, e.g. 10s)
Every ~30s:         Empty → StaticDrift → Multi (uses its existing 1min timeout)
Every ~60s:         Empty → StaticDrift → Single (uses its existing 3min timeout)
```

If the scheduled method has no work, the slot falls through to the next eligible method (work-conserving).

- 👍 Simple to implement with timestamp tracking for cadences
- 👍 Consolidation gets dedicated, uninterrupted time at a sustainable cadence
- 👍 No budget or API changes needed
- 👎 Fixed cadences do not adapt to changing cluster conditions (e.g., when a method finishes early, others still wait for their cadence)
- 👎 Cadence parameters need tuning per cluster size

### Interaction with Existing Mechanisms

- **`IsConsolidated()` gate**: Still functions. Consolidation skips evaluation if nothing changed since its last run. Combined with CFS scheduling, consolidation only does expensive evaluation when (a) CFS picks it and (b) cluster state changed.
- **Disruption budgets**: No changes to budget computation logic. `BuildDisruptionBudgetMapping` is computed once per distinct reason at the start of Phase 2 and cached. The cached mapping is used for both eligibility gating (which methods can run) and passed to the selected method's `disrupt()` call to avoid redundant recomputation.
- **Metrics**: `recordRun()` and `EvaluationDurationSeconds` continue to work per-method. New metrics could track per-method druntime, selection frequency, and time utilization.
