# Launch Backoff for Insufficient Capacity

After repeated `InsufficientCapacityError` (ICE), Karpenter retries the affected
offering at a bounded rate. Healthy offerings in the same NodePool are not penalized.
A cluster that never fails a launch is unchanged.

## Motivation

When a workload requests capacity the cloud provider cannot satisfy, Karpenter creates and
destroys NodeClaims in a tight loop for as long as the pods stay pending
([#3198](https://github.com/kubernetes-sigs/karpenter/issues/3198)). The launch path for
ICE deletes the NodeClaim immediately (`Launch.launchNodeClaim`) and relies entirely on
the cloud provider's unavailable-offerings cache for suppression. There is no backoff on
the create path, so the loop's rate is bounded only by controller throughput, and each
iteration drives work through the provisioner, `nodeclaim.lifecycle`, and disruption
controllers. A single NodePool's unsatisfiable demand therefore degrades provisioning and
disruption for every NodePool in the cluster.

This is not a rare edge case. Two production clusters over a 17-day window:

| Cluster     | NodeClaims created | Deleted by ICE | ICE share | Peak create rate |
| ----------- | ------------------ | -------------- | --------- | ---------------- |
| `cluster-a` | 366,724            | 341,522        | 93.1%     | 2.5/s over 2h    |
| `cluster-b` | 404,060            | 308,280        | 76.3%     | 5.4/s over 2h    |

In `cluster-a`, **more than nine out of ten NodeClaims ever created were destroyed by ICE
before launching.** The worst two-hour bucket in `cluster-b` created 39,137 NodeClaims and
destroyed 38,226 of them (97.7%) — roughly 5.4 NodeClaim create/delete cycles per second,
sustained, for two hours. Provisioner scheduling latency in that cluster runs 15–300s against
2–10s in an otherwise comparable cluster.

**Why the provider's ICE cache does not fix this.** The cache is a *scheduling filter*, not a
*provisioning throttle*:

- It adds no delay. It filters the offering set the scheduler considers; as long as the
  NodePool has any offering not currently cached, the scheduler produces a launch decision
  and lifecycle immediately creates another NodeClaim.
- A single ICE marks exactly one `capacityType:instanceType:zone` offering. A broad NodePool
  (GPU families across several AZs and capacity types) has dozens to hundreds of offerings,
  so Karpenter walks them one create → ICE → delete cycle at a time.
- Entries are learned *by failing*. The cost of learning that an offering is bad is itself a
  full NodeClaim churn cycle. The cache only suppresses the *repeat* attempt.
- The TTL (3 minutes in the AWS provider) is far shorter than the lifetime of pending demand,
  so entries continually expire and become eligible again while demand persists.

That last property produces the burst that prompted this RFC: with a few thousand pods queued
against an ICE'd offering, the moment the cache entry expires Karpenter blasts out a large
batch of NodeClaims, all of which fail. As long as capacity is unavailable this repeats on
the cache interval. The existing boolean has no way to express "retry slowly."

### Use Cases

1. **Large partially-unsatisfiable scale-out.** A ~600-node GPU scale-out with thousands of
   pending pods, where most of the requested offerings are ICE'd.
    * Today: sustained NodeClaim churn at controller throughput, saturated workqueues, and degraded provisioning for
   unrelated NodePools.
    * Desired: the unsatisfiable portion is retried at a bounded rate while
   the satisfiable portion launches at full speed.
2. **Burst at cache expiry.** Thousands of pods queued against a single ICE'd offering.
    * Today: a write storm every time the provider's cache entry expires.
    * Desired: recovery is rate-limited, so the first attempts after expiry are probes rather than the full batch.
3. **Zone-scoped shortage in a multi-AZ NodePool.** One AZ is out of an instance type; the
   other three are healthy. What we want is to ensure launches into the healthy AZs are preferred and
   delayed by at most one probe interval. Pods with a zonal spread should give up the
   *instance type* before they give up the *spread* — see
   [Topology spread](#topology-spread) for why that ordering is already what happens and
   what would break it.

### Non-Goals

- **Predicting capacity before the first failure.** Core learns only from observed launch
  outcomes. The first ICE for a previously-healthy offering is not avoidable, nor is
  the first batch after a process restart.
- **Modeling remaining capacity as a scalar.** v1 is a boolean filter (available / backed
  off) plus a rate limit. Binpacking against "3 slots left" is a later RFC; it is not
  required to close [#3198](https://github.com/kubernetes-sigs/karpenter/issues/3198).
- **Replacing `ReservationCapacity` or generalizing `ReservationManager`.** Reservation
  capacity is keyed by reservation ID and shared across instance types. ICE is keyed by
  `instanceType:capacityType:zone`. Those are different numbers. ODCR exhaustion stays on
  the existing reservation path.
- **Replacing provider-side ICE caches.** They remain a fast, provider-local filter. Core
  layers a more conservative filter on top: an offering is usable only if the provider
  reports it available *and* core is not backing it off.
- **Querying cloud capacity APIs** or sharing capacity signals across clusters.
- **Changing what happens to the failed NodeClaim.** It is still deleted on ICE. This RFC
  bounds how often that cycle can happen, not the cycle itself.
- **Making unsatisfiable pods schedulable.** They stay pending. The goal is to stop
  paying for that in cluster-wide controller throughput.

## Proposal

Two pieces of in-memory state, both written only from real launch outcomes:

1. **Per-offering backoff** (`cloudprovider.OfferingKey`: `InstanceType`, `CapacityType`,
   `Zone`). After ICE, core treats that offering as unavailable until a backoff window
   elapses, then eligible again. This filter decides whether an offering may be tried
   at all, while the budget below decides how fast. Applied by
   `FilterUnavailable` to DeepCopy'd instance types at the two scheduling
   `GetInstanceTypes` call sites. See
   [Applying the backoff filter](#applying-the-backoff-filter).
2. **Per-NodePool launch budget**, with two parts. An aggregate budget engages only while
   the NodePool is constrained by a recent ICE: the callers of `CreateNodeClaims` (the
   dynamic provisioner and `static.provisioning`) `Admit` one NodeClaim per `probeInterval`,
   ramping up as probes succeed and returning to one on any failure. A *risky* budget is
   always engaged, even for an unconstrained pool: a NodeClaim whose every compatible
   offering has a failure history is admitted at most `riskyBurst` per `probeInterval`. The
   filter cannot supply that bound itself.
   `CreateNodeClaims` itself is unchanged: it still creates every NodeClaim it is given.
   Disruption only *peeks* with a read-only `IsConstrained`; it never consumes a probe.

An offering with no recorded failures is not in the tracker, and a NodePool with no recorded
ICE is unconstrained, so **a cluster that never hits a launch failure behaves as it does
today.** Entries also expire once they go quiet, so a cluster that recovers returns to that
state rather than carrying failure history forever.

```
                    ICE                         window elapses
  (absent/healthy) ------> (backed off) ------> (probe eligible)
         ^                      |                      |
         |                      | ICE (no-op           | ICE
         | success              |  inside window)      v
         +----------------------+---------------- (level grows, new window)
         |
         |  ...also on expiry: probe-eligible and untouched for maxDelay
         +----------------------------------------------------------------
```

### How It Works

#### Offering backoff

New package `pkg/state/launchbackoff`. Two maps: offering entries keyed by
`cloudprovider.OfferingKey`, pool entries keyed by NodePool UID. Written from
`launchNodeClaim` (`Fail` / `Succeed`) and from the `Admit` callers; read from scheduling
(`FilterUnavailable`, `IsAvailable`, `HasFailed`) and from the disruption `Queue`
(`IsConstrained`).

```go
type offeringEntry struct {
	level int       // failed windows (0 == healthy / absent)
	until time.Time // unavailable before this time; expires at until+maxDelay
}

func (t *Tracker) IsAvailable(cloudprovider.OfferingKey) bool // absent, healthy, or now >= until
func (t *Tracker) HasFailed(cloudprovider.OfferingKey) bool   // an entry exists; see Admit ordering
func (t *Tracker) Fail(cloudprovider.OfferingKey)             // observed ICE; no-op inside window
func (t *Tracker) Succeed(cloudprovider.OfferingKey)          // observed successful launch; reset
func (t *Tracker) NextEligible(cloudprovider.OfferingKey) time.Time
```

Canonical key and ICE attribution live on `cloudprovider`, next to `Offering`. There is one
`OfferingKey` in the repo: `pkg/state/cost.OfferingKey` (`Zone`, `CapacityType`,
`InstanceName`) is replaced with an alias of this type.

```go
// pkg/cloudprovider/types.go
type OfferingKey struct {
	InstanceType, CapacityType, Zone string
}
...
type InsufficientCapacityError struct {
	error
	Keys []OfferingKey
}
```

Variadic `keys` keeps every existing call site compiling. `launchNodeClaim` uses `errors.As`
to get `*InsufficientCapacityError` and reads `.Keys` (nil or empty → `FailPool` only; see
[Recording outcomes](#recording-outcomes)).

Backoff formula, matching the drift RFC so operators see one shape:

```
level  := level + 1
window := min(baseDelay * 2^(level-1), maxDelay)
window := window/2 + rand[0, window/2)   // equal jitter
until  := now + window
```

| Parameter   | Default | Rationale |
| ----------- | ------- | --------- |
| `baseDelay` | `30s`   | First probe after a short delay: fast enough to recover quickly once capacity returns, long enough that a window is a meaningful unit of escalation. Deliberately *not* chosen to outlast a provider cache — see below. |
| `maxDelay`  | `10m`   | Absolute ceiling on a single window. Reached after 6 consecutive failed windows. |

An entry **expires** — is deleted outright, level and all — once it has been continuously
`IsAvailable` for `maxDelay`, i.e. at `until + maxDelay`. No extra field: `until` already
carries the timestamp.

**Core windows do not out-prioritize a provider cache, and are not meant to.** Usability is
the intersection (`provider.Available && IsAvailable`), so the *longer* of the two gates
binds. At levels 1–3 a jittered core window (15s–2m) is shorter than the AWS cache's 3m
TTL, which means the provider decides when a retry is attempted at all and core's window
has already elapsed by the time it happens.

#### Applying the backoff filter

`GetInstanceTypes` often returns cached pointers, and `Offerings` is `[]*Offering`, so a
shallow slice copy still aliases `Available`. **Do not assume something upstream already
copied the offerings.** In particular `nodeoverlay.InstanceTypeStore.ApplyAll` hands back
the caller's own slice when a NodePool has no overlays, which is the common case, so the
decorator cannot be relied on for a copy. The only safe mutation is
`InstanceType.DeepCopy()` on the instance types that will actually change.

A named helper does this. Combined with the provider:

```
usable(offering) = offering.Available && backoff.IsAvailable(key(offering))
```

```go
// FilterUnavailable returns `its` unchanged (same pointer identity) when the
// tracker has no matching entries. Otherwise it DeepCopy()s only instance types
// that have at least one backed-off offering, sets Available=false on those
// offerings, and leaves other instance types aliased. Scheduling copies only —
// do not call this on GetInstanceTypes results used for prices or inventory.
func FilterUnavailable(its []*cloudprovider.InstanceType, t *Tracker) []*cloudprovider.InstanceType
```

Call `FilterUnavailable` **immediately** after `GetInstanceTypes` and **before** anything
that can trigger `AllocatableOfferingsList()` / `Allocatable()` (those `sync.Once` the
filtered `Available` set). Production call sites that feed scheduling, and only those:

1. `Provisioner.NewScheduler` — after `GetInstanceTypes`, before `NewTopology` /
   `scheduler.NewScheduler`. Disruption simulation uses this path
   (`disruption/helpers.go` → `provisioner.NewScheduler`).
2. `disruption.BuildNodePoolMap` — after `GetInstanceTypes`, before the name→type map.
   Candidates use this for compatibility and cost of existing nodes' instance types;
   leaving it unfiltered means disruption will plan replacements onto offerings core is
   backing off.

A provider that reports an offering unavailable still wins, and a provider whose cache entry
has expired does not make the offering usable until core's window has also elapsed.

#### Two NodeClaims, same pool

A NodeClaim carries a set of acceptable instance types and requirements, and the provider chooses the instance.

- **Pod can use a healthy offering.** The NodeClaim's compatible set still contains at
  least one offering that is not backed off. That offering survives the filter and the
  NodeClaim launches immediately if the NodePool is unconstrained. Backed-off siblings in
  the set are simply absent; they are not charged.
- **Every compatible offering is backed off.** The filter leaves nothing. `Solve` does not
  produce a NodeClaim for these pods. They stay pending and are surfaced by
  `karpenter_scheduler_unschedulable_pods_count`. When the earliest `until` elapses, that
  offering becomes eligible and `Solve` starts producing NodeClaims for these pods again —
  potentially many, all targeting the one eligible key. Those NodeClaims are *risky*, and
  the NodePool budget below is what limits how many are admitted per window.

The filter therefore binds for a given pod exactly when every offering it could land on is
backed off. The NodePool budget is coarser and can briefly delay a satisfiable NodeClaim: a
pool constrained by an ICE elsewhere rate-limits *all* of its launches until a success
releases it. Admit ordering keeps that delay to roughly one `probeInterval` by spending each
window on the NodeClaims most likely to succeed — see
[Admit ordering](#per-nodepool-launch-budget).

#### Per-NodePool launch budget

The NodePool budget is a *rate limit*, not a window. A boolean window is a poor fit here:
when it elapsed it would admit the full `Solve`, reproducing the cache-expiry thundering
herd at NodePool scope.

```go
type poolEntry struct {
	// Aggregate budget. Engaged only while the pool is constrained by a recent ICE.
	constrained bool
	burst       int       // ceiling: admits allowed per window at the current recovery level
	remaining   int       // allowance left in the current window; reset to burst on rollover
	nextAdmit   time.Time

	// Risky budget. Always engaged, including while unconstrained.
	riskyRemaining int
	nextRiskyAdmit time.Time
}

func (t *Tracker) Admit(nodePoolUID types.UID, risky bool) bool
func (t *Tracker) IsConstrained(nodePoolUID types.UID) bool // read-only; never consumes
func (t *Tracker) FailPool(nodePoolUID types.UID)
func (t *Tracker) SucceedPool(nodePoolUID types.UID)
func (t *Tracker) NextAdmit(nodePoolUID types.UID, risky bool) time.Time // latest applicable gate
```

`burst` and `remaining` are **two different numbers** and collapsing them breaks both
mechanisms: consumption would erode the recovery ceiling, and
`karpenter_nodepools_launch_burst` would graph a per-window sawtooth instead of the ramp it
is documented to show. `burst` changes only on `SucceedPool` / `FailPool`; `remaining`
changes only on `Admit` and window rollover.

A NodeClaim is **risky** when every *usable* offering in its compatible set has a tracker
entry, meaning there is no offering it can actually land on without a recent capacity failure.
The caller already computes this predicate to order admissions (below), so passing it to
`Admit` costs nothing extra.

**"Usable" is load-bearing.** The predicate must consider only offerings still marked
`Available` after `FilterUnavailable` — the probe-eligible and healthy ones — and must ignore
offerings the provider reports unavailable. Evaluating it over the full compatible set instead
makes the risky budget silently inert: any never-failed offering has no tracker entry, so a
single permanently-unavailable one (a retired instance type in one zone, say) would mark every
NodeClaim in the pool non-risky forever. The partial-recovery burst returns with nothing in the
metrics to indicate why. Concretely, `risky` is "every compatible offering with
`Available == true` satisfies `HasFailed`", which is well-defined because `FilterUnavailable`
has already cleared `Available` on everything inside its window.

- **Unconstrained and not risky** (absent entry, or `constrained == false`): `Admit` returns
  true. The caller passes the full set into `CreateNodeClaims`, which fans out as today.
  A risky `Admit` against an *absent* pool entry creates one, because offering entries are
  shared across NodePools: a pool that has never failed a launch itself can still have risky
  NodeClaims if another pool's ICE armed the offerings they depend on.
- **`FailPool`:** set `constrained = true`, `burst = 1`, `remaining = 1`,
  `nextAdmit = now + probeInterval`. No-op if already constrained and `now < nextAdmit`.
- **`Admit` evaluates every applicable gate before consuming any of them.** Roll over any
  window whose deadline has passed (`remaining = burst` when `now >= nextAdmit`;
  `riskyRemaining = riskyBurst` when `now >= nextRiskyAdmit`). Then test the gates that apply:
  `remaining > 0` if the pool is constrained, and `riskyRemaining > 0` if the NodeClaim is
  risky. If any applicable gate is closed, return false and consume **nothing**. Otherwise
  decrement every applicable allowance and return true, so a risky admit from a constrained
  pool debits both.
- **`SucceedPool`:** double `burst`, which takes effect at the next window rollover. Once it
  would exceed `burstMax`, clear `constrained` and the aggregate budget disengages. The
  risky budget does not ramp and is not released — see below.
- **`NextAdmit` mirrors `Admit`'s gates.** It takes the same `risky` argument and returns the
  *latest* of the gates that apply, because waking at an earlier gate that was not what
  blocked this NodeClaim buys a reconcile that admits nothing.
- **Entry cleanup:** delete the pool entry when the NodePool is deleted, and idle-expire it
  when it is unconstrained and `now >= nextRiskyAdmit + maxDelay`. Deleting an idle entry is
  safe because a subsequent risky admit recreates it with a fresh window, which still admits
  one.

**Recovery ramps; it does not jump.** Clearing `constrained` on the *first* success would
admit the entire `Solve` batch on the next loop. Doubling `burst` bounds the overshoot to
`burstMax` doomed NodeClaims while still recovering quickly: successes within one window
compound, so a recovered pool reaches full speed in two or three windows rather than one,
avoiding a thundering herd when a launch succeeds. Release is not the end of the bound:
once `constrained` clears, any NodeClaim still backed only by failed offerings falls to the
risky budget, so overshoot stays bounded in both regimes rather than only while
constrained.

When a window's admits return a mix of successes and failures, the resulting `burst` depends
on the order the outcomes land, and a `SucceedPool` that releases the pool may be followed
by a `FailPool` that re-constrains it at `burst = 1`. That is acceptable: every ordering
converges to "some success ramps up, any failure clamps down," and the idempotence rule
keeps a burst of failures from one window from compounding.

**Admit ordering.** `Admit` is per-NodePool, so in a mixed pool the *choice* of which
NodeClaim gets the window's allowance decides whether the window is spent usefully. Walking
the `Solve` result in arbitrary order can spend window after window on NodeClaims that can
only land on a probe-eligible (recently backed-off) offering, while a NodeClaim that could
land on a never-failed offering waits behind them. That would weaken use case 1's promise
that the satisfiable portion launches at full speed into "launches at full speed once a
satisfiable launch happens to be admitted and succeed."

So partition the `Solve` result before admitting: NodeClaims whose compatible offerings
include at least one key with no tracker entry go first, then the rest. The first group is
likely to succeed, and successes are what ramp the pool back up. Note this is a different
question from `IsAvailable`, which is also true for a probe-eligible key whose window just
elapsed — the partition needs "never failed," not "allowed to try," so the tracker exposes
entry presence separately.

The second group *is* the risky set, so this single partition supplies both the ordering and
the `risky` argument to `Admit`; there is no separate pass. It has to run for unconstrained
pools too, because the risky budget applies to them, but the whole walk is skipped when the
tracker holds no offering entries — the cluster that never fails a launch still pays
nothing.

**Admit is a caller concern, not a `CreateNodeClaims` concern.** That function is a shared
create fan-out; `Queue.createReplacementNodeClaims` requires `len(names) ==
len(replacements)` after nodes may already be cordoned. Silently omitting inside
`CreateNodeClaims` would fail that check.

| Caller | Behavior |
| ------ | -------- |
| `Provisioner.Reconcile` | `Admit` each `Solve` NodeClaim (consume), keyed by `NodeClaimTemplate.NodePoolUUID`, passing `risky` from the partition above. Pass only the admitted set into `CreateNodeClaims`. Increment `karpenter_nodepools_launch_throttled_total` once per omitted NodeClaim. |
| `static.provisioning` | Same consume-then-create with `risky = false`; see [Static capacity](#static-capacity). |
| Disruption `Queue` | **Never *consume* `Admit`.** Peek with read-only `IsConstrained` before `markDisrupted` and skip the command while any involved pool is constrained. If `Create` itself ICE's, the existing `StartCommand` error path applies. |

Static passes `risky = false` because it has no per-NodeClaim offering set to evaluate — it
sizes a replica gap against a NodePool template and never builds a scheduler. That is
sufficient rather than a gap: the aggregate budget only disengages after a *success*, and a
static NodeClaim does not pin a zone (the provider picks), so a static pool whose launches
keep failing never accumulates the successes that would release it.

`IsConstrained` answers "is this pool constrained right now" without touching `remaining` or
`nextAdmit`, so a disruption peek never steals a probe from the provisioner.

**Which NodePools it checks.** A `Command` carries `Candidates []*Candidate` and
`Replacements []*Replacement`, and multi-node consolidation deliberately builds commands
whose candidates span NodePools. The check is keyed on the **replacement** pools —
`Replacement.NodeClaim.NodeClaimTemplate.NodePoolUUID` — because those are the pools that
would launch. Specifically:

- A command with no replacements (pure deletion: empty, expiration, single-node
  consolidation to zero) is **never** gated. It creates nothing, and blocking it would let a
  capacity shortage prevent cluster cleanup.
- A command with replacements is skipped if **any** replacement's pool is constrained. All
  of a command's replacements must be created together to satisfy `len(names) ==
  len(replacements)`, so partial admission is not an available outcome and the check has to
  be all-or-nothing.
- Candidate pools are **not** consulted. Draining a node in a constrained pool is fine and
  often desirable; what must be throttled is the launch.

The check must run **before** `markDisrupted`, which cordons the candidates ahead of
launching replacements (`queue.go`); checking afterwards would leave a cordoned node whose
replacement was refused. Skipping a command is already a supported outcome — it is simply
not started — whereas *consuming* would tempt an implementation to omit inside
`CreateNodeClaims` and break the `len(names) == len(replacements)` contract below.

| Parameter       | Default | Rationale |
| --------------- | ------- | --------- |
| `probeInterval` | `30s`   | One window per 30s per NodePool, for both budgets. Independent of offering `baseDelay` so a pool with 200 backed-off offerings cannot emit 200 probes when their windows line up. |
| `burstMax`      | `8`     | Ceiling on admits per window before the aggregate budget disengages. Caps wasted launches after a spurious recovery at 8, and reaches full speed in ~3 windows (`1 → 2 → 4 → 8`) when capacity is genuinely back. |
| `riskyBurst`    | `1`     | Launches per window whose entire offering set has a failure history, regardless of `constrained`. |

#### Recording outcomes

`Launch.launchNodeClaim` is the single place a launch's fate is known:

- **ICE.** `errors.As` to `*cloudprovider.InsufficientCapacityError`. For each key in
  `.Keys`, `tracker.Fail(key)`. Always `FailPool` for the NodeClaim's NodePool UID. The
  NodeClaim is still deleted and
  `NodeClaimsDisruptedTotal{reason=insufficient_capacity}` still incremented — unchanged.
  Empty `Keys` (today's constructor, every unmodified provider) is FailPool-only.
- **Success.** Derive `cloudprovider.OfferingKey` from the created NodeClaim's resolved
  labels (`LabelInstanceTypeStable`, `CapacityTypeLabelKey`, `LabelTopologyZone`) and
  `tracker.Succeed(key)` plus `SucceedPool`.
- **Other launch errors** (`NodeClassNotReadyError`, generic `CreateError`) do not touch
  either tracker. They are not capacity signals, and `NodeRegistrationHealthy` already
  covers the misconfiguration case. See [Open Questions](#open-questions).

Populating `Keys` is a **provider change**, not a core one. Alpha in core ships the field
and FailPool-only behaviour. Use cases 1 and 3 (healthy-AZ isolation, mixed-pool admit
ordering) need attributed keys; the AWS provider follow-up should fill `Keys` from the same
CreateFleet override `(InstanceType, AvailabilityZone)` the unavailable-offerings cache
already uses. KWOK populates `Keys` in-tree so core tests do not depend on AWS. See
[Backward Compatibility](#backward-compatibility) and [Graduation Criteria](#graduation-criteria).

#### Attributing unschedulability to unavailable offerings

The requeue below has to tell "these pods have nowhere to go until capacity frees up" apart
from "these pods are unschedulable for a reason no delay will fix." That judgement is
reported with a typed error, in the shape of the existing `ReservedOfferingError`:

```go
// pkg/controllers/provisioning/scheduling
type OfferingsUnavailableError struct {
	error
	NextEligible time.Time // when it is worth trying again
}

func NewOfferingsUnavailableError(err error, nextEligible time.Time) OfferingsUnavailableError
func IsOfferingsUnavailableError(err error) bool // errors.As, as IsReservedOfferingError does
func (e OfferingsUnavailableError) Unwrap() error

// mirrors Results.ReservedOfferingErrors / Results.DRAErrors
func (r Results) OfferingsUnavailableErrors() map[*corev1.Pod]error
```

**Where it is produced.** In `filterInstanceTypesByRequirements`. That function deliberately
does not short-circuit, so it can explain failures, and accumulates per-reason bookkeeping in
`InstanceTypeFilterError`. The discriminator is:

```go
len(remaining) == 0 && err.requirementsMet && !err.hasOffering
```

Read: at least one instance type matched the pod's requirements, and *no* instance type had a
usable compatible offering. That is exactly the offering-blocked case, and both fields are
already maintained by the existing loop.


**Deliberately layer-agnostic.** The error does *not* distinguish an offering core is
backing off from one the provider reported unavailable, and an earlier draft's
`HasFailed && !IsAvailable` predicate was wrong to try. Both are transient capacity states
with the same remedy, and the distinction creates a hole: after a core window elapses while
a provider's longer cache entry persists, `HasFailed` is true and `IsAvailable` is *also*
true, so that predicate goes false and the provisioner returns to spinning `Solve` every
loop for the remainder of the provider's TTL — the exact CPU cost the requeue exists to
remove. Since core cannot know a provider's TTL, it also cannot compute a precise wake time
for that case, which is why the requeue is capped at `probeInterval` regardless.

**`NextEligible`** is therefore a hint, not a deadline: `min(NextEligible(key))` over the
rejected keys that *do* have tracker entries, falling back to `now + probeInterval` when
none do (pure provider suppression). Where a NodeClaim was rejected by more than one gate,
the hint is the *latest* of the gates that actually blocked it, since an earlier one waking
first buys nothing. The tracker reference the scheduler needs is only for
that hint — the classification comes from `InstanceTypeFilterError`. `Solve` still makes
decisions by reading `Available` alone, so the filter stays transparent for scheduling and
becomes visible only for *explaining* a failure. That is a genuine addition to the
scheduler's surface, called out here rather than buried, since "existing call sites do not
change" is otherwise one of this design's selling points.

**Both directions of error are safe.** An offering may be filtered when the instance type
would have failed on resources anyway, so a pod can be labelled offering-blocked when a
delay is not strictly the cause; the cost is bounded to one `probeInterval` of sleep on a
pod that was not going to schedule regardless. Missing the attribution yields today's
immediate requeue. Neither direction can cause a pod to be dropped or delayed
indefinitely.

`Record` treats it as it treats `ReservedOfferingError` — no error-level log or event per
loop — since an unavailable offering is an expected transient state and the affected pods
are already counted by `karpenter_scheduler_unschedulable_pods_count`.

**It must not short-circuit preference relaxation.** This is the one place the
`ReservedOfferingError` analogy must *not* be carried through, and it needs stating because
an implementer following the analogy would carry it through by reflex. `trySchedule` returns
early for a reserved-offering error instead of relaxing:

```go
if IsReservedOfferingError(err) {
	return err
}
```

`OfferingsUnavailableError` does **not** get added to that check. The reasoning behind the
reserved-offering case does not transfer: a reservation can be released by another NodeClaim
*within the same* `Solve`, so declining to relax costs sub-second latency and may avoid
discarding a preference unnecessarily. A backoff window is 15s to `maxDelay` and cannot
resolve inside the loop, so declining to relax would hold a pod pending for minutes against
an explicit `ScheduleAnyway` or `preferred...` statement that it would rather run than keep
the preference. Adding the short-circuit would make soft constraints behave like hard ones
whenever capacity is short — see [Topology spread](#topology-spread).

Leaving relaxation alone also makes the attribution *stronger*. `podErrors[pod]` is only
populated after `trySchedule` has exhausted every relaxation, so the discriminator is
evaluated against the fully-relaxed pod. `OfferingsUnavailableError` therefore means "no
usable offering even with every preference dropped," and a pod that could have escaped by
relaxing never reaches the requeue path at all.

#### Topology spread

Use case 3 says pods with a zonal spread should give up the instance type before the spread.
That is already the behaviour, and it falls out of ordering rather than policy:
`nextDomainTopologySpread` returns the *single* least-loaded eligible domain, so the pod
carries `zone in [dead-az]` as a requirement *before* instance types are filtered.
`filterInstanceTypesByRequirements` then drops only the instance types with no usable
offering in that zone, and price ordering ranks whatever survives. Price can never trade the
zone away, because every candidate already satisfies the zone requirement. Under the default
`PreferencePolicyRespect` a `ScheduleAnyway` spread is treated as required for that first
pass, which is what pins the zone at all.

So the concession order for a soft zonal spread is:

1. A different (possibly pricier) instance type in the desired zone. Automatic, inside
   `filterInstanceTypesByRequirements`, no relaxation involved.
2. Required node-affinity terms, then preferred pod affinity, pod anti-affinity, and node
   affinity — the order in `Preferences.Relax`.
3. The spread itself. `removeTopologySpreadScheduleAnyway` is **last** in that list, so the
   pod only lands in a healthy zone once nothing else can be given up.

A `DoNotSchedule` spread is never relaxed, so those pods wait for their zone's window and are
the population the requeue sleep exists for.

The interaction the filter does **not** change is that a fully backed-off zone remains a
registered topology domain. `buildDomainGroups` derives domains from
`InstanceType.Requirements`, not from `Offerings.Available()`, so clearing `Available` leaves
the zone in `t.domains` with a count of 0. Topology keeps electing it as least-loaded, every
affected pod pays the full relaxation loop on every pass, and for `DoNotSchedule` the global
`min` stays 0, so `count - min <= maxSkew` caps each healthy zone at `maxSkew` while the dead
zone can never be filled. This is pre-existing — the provider's ICE cache clears the same
field and produces the same result for its TTL — but core's windows reach `maxDelay`, so the
RFC makes it last longer. Pruning the zone from `InstanceType.Requirements` would fix it and
is deliberately **not** in scope: those requirements feed label resolution on the resulting
NodeClaim, so pruning changes what the node advertises. See
[Open Questions](#open-questions).

#### Provisioner requeue

Launch throttling does not by itself fix provisioner CPU. Today, zero `NewNodeClaims`
still requeues immediately, so thousands of pending pods against fully backed-off
offerings still run `Solve` every loop.

When the gate is on:

```
created, omitted := ... // after Admit + CreateNodeClaims

if len(created) > 0 {
    return RequeueImmediately  // other pools / remaining pods still need a loop
}

// Every pending pod must be accounted for by a delay we can wait on. There are
// two populations, because a pod on a NodeClaim that Admit omitted never
// reached Results.PodErrors at all:
//   1. Solve produced no NodeClaim for it   -> OfferingsUnavailableError
//   2. Solve produced one, Admit omitted it -> pod is on an omitted NodeClaim
for pod := range pendingPods {
    if IsOfferingsUnavailableError(results.PodErrors[pod]) || omitted.Has(pod) {
        continue
    }
    return RequeueImmediately  // affinity, resources, minValues, etc.
}

// Per blocked NodeClaim or pod, the wake time is the latest gate that actually
// blocked it; across them, the earliest such time.
wake := min(
    NextAdmit(pool, risky)  for each omitted NodeClaim,
    err.NextEligible        for pods blocked on unavailable offerings,
)
return RequeueAfter: min(wake, probeInterval)
```

#### Static capacity

Static never builds a scheduler, so `FilterUnavailable` does not run on this path. The pool budget
**is** the mechanism. "Throttled identically" is not true of today's
`static.provisioning.Reconcile`: it sizes a replica-gap slice, calls `CreateNodeClaims`
directly, uses `HasSynced() || Synced()`, runs `MaxConcurrentReconciles: 10`, requeues 1m
on success, and retriggers immediately on NodeClaim delete (the ICE path).

Offering `Fail` still runs from `launchNodeClaim`. Isolation across AZs for static depends
on provider `Keys`; without `Keys` you only get the pool throttle. That is enough to bound
the create/delete loop, including the ODCR-static ICE in [#3198](https://github.com/kubernetes-sigs/karpenter/issues/3198).

#### Worked example

A NodePool with 200 GPU offerings across 4 AZs, 3,000 pending pods, and no capacity
anywhere. Defaults `baseDelay=30s`, `maxDelay=10m`, `probeInterval=30s`.

1. **t=0.** No tracker entries. The filter is a no-op, NodePool unconstrained — identical to
   today. The provisioner batches and creates NodeClaims. The first launches fail.
2. **First failures (t=0+ε).** Each ICE names the keys the provider attempted. With
   attribution populated, a handful of `CreateFleet` failures moves most of the 200
   offerings to `level=1, until≈now+30s` (jittered). Further ICE from the in-flight batch
   is a no-op. `FailPool` constrains the NodePool; `burst=1`, `nextAdmit≈now+30s`.
3. **Steady state.** The filter removes every offering until `until`. `Solve` produces no
   launch for these pods; they remain pending. Once *both* gates open — core's window and
   the provider's own ICE cache entry, whichever is longer, so ~3m at this level on AWS —
   `Admit` allows one NodeClaim: a single probe, and one either way, since a NodeClaim
   backed only by failed offerings is risky and `riskyBurst` is 1. If it ICEs, that key's
   level grows (`until≈now+1m`), `burst` resets to 1, and both `nextAdmit` and
   `nextRiskyAdmit` move forward 30s. Sustained cost is ~2 NodeClaim cycles/minute instead
   of ~325.
4. **Capacity returns.** A probe succeeds. That offering leaves the tracker and
   `SucceedPool` doubles `burst` to 2. The next window admits 2, preferring NodeClaims that
   can land on a never-failed offering; both succeed, compounding `burst` to 8. The window
   after that admits 8, and its first success pushes `burst` past `burstMax` and releases
   the pool to full speed — ~3 windows, ~90s. Had capacity only marginally returned, the
   overshoot would have been capped at 8 doomed NodeClaims rather than the whole pending
   batch.
5. **Partial recovery.** If only one AZ recovers, only that AZ's keys `Succeed`. The other
   three stay backed off and are filtered out of the scheduler's set, so newly admitted
   NodeClaims flow to the healthy AZ. The healthy AZ's successes release the pool, and its
   NodeClaims are not risky (their sets include never-failed keys), so they launch at full
   speed. Pods *pinned* to a dead AZ (topology spread, zonal affinity) become the risky set:
   as each dead-AZ window elapses they are admitted one per `probeInterval`, rather than as
   an unbounded batch on a pool that is no longer constrained. This is the case the risky
   budget exists for; see Edge Cases.

#### Invariants

If any of these stops holding, the corresponding part of the design must be revisited.

- **Mutations happen only on real launches.** `Fail` / `Succeed` run from
  `Launch.launchNodeClaim`. `Admit` — the only budget-consuming call — runs from
  `Provisioner.Reconcile` and `static.provisioning.Reconcile` for NodeClaims that will
  actually be created, never from `CreateNodeClaims` itself and never from disruption.
  Everything else reads: scheduling simulation (`Solve`, disruption packing) via
  `FilterUnavailable` /
  `IsAvailable` / `HasFailed`, and the disruption `Queue` via `IsConstrained`. A read must
  never touch `remaining`, `riskyRemaining`, `nextAdmit`, or `nextRiskyAdmit`.
- **The filter is `FilterUnavailable`, before first precompute, at the two scheduling call
  sites.** It `DeepCopy()`s only instance types that have a backed-off offering; other
  pointers stay aliased. `fits` reads the memoized `allocatableOfferings`, not live
  `Offerings`, so it must run before anything triggers `AllocatableOfferingsList()`
  on a copy. An empty tracker must return the provider slice with pointer identity
  intact. See [Applying the backoff filter](#applying-the-backoff-filter).
- **`Fail` is idempotent inside a window.** A burst of in-flight ICE from the first batch
  arms backoff once; subsequent failures for the same key or NodePool before the window
  elapses are no-ops. Escalation tracks failed *windows*, not individual attempts — the
  same rule as [per-NodePool drift backoff](./drift-per-nodepool-backoff.md).
- **`Admit` is an atomic compare-and-consume.** Many NodeClaims from one NodePool land their
  outcomes in `launchNodeClaim` concurrently while a provisioner calls `Admit` for that same
  pool, so a read of `nextAdmit` followed by a separate write would race and overshoot the
  budget. `Admit` takes the write lock once and does the window rollover, both allowance
  decrements, and the decision under it.
- **No launch escapes both budgets.** Every path that creates a NodeClaim is covered by the
  aggregate budget while its pool is constrained, by the risky budget when its offering set
  is entirely previously-failed, or by `IsConstrained` for disruption. A launch that is
  neither constrained nor risky is *intended* to be unthrottled — that is the "cluster that
  never fails behaves as today" property — so any new create path must be classified
  explicitly rather than defaulting to unthrottled.
- **Escalation state decays.** Offering entries expire at `until + maxDelay` and pool
  entries idle-expire or are deleted with the NodePool, so `level` and `burst` always
  describe *recent* history. Without this, a single old failure permanently changes how an
  offering is treated.
- **In-memory state is sufficient.** Nothing is persisted. Discarding the trackers on
  restart at worst re-attempts a failing pool once before backing off again.

### Interaction with Existing Features

- **Drift and consolidation.** Both simulate through `provisioner.NewScheduler` (and
  disruption candidates through `BuildNodePoolMap`), so they see the offering filter.
  Replacement *creates* still go through `CreateNodeClaims`, which does **not** call
  `Admit` — a constrained pool's drift replacement still creates exactly
  `len(replacements)` NodeClaims, or the command is not started. A drift rollout into an
  ICE'd offering is the failure mode behind
  [#3080](https://github.com/kubernetes-sigs/karpenter/issues/3080), and the intended
  division of labour is that drift backoff gates *candidate selection* while this RFC gates
  *offering usability* on the simulation side. Consolidation's use of `Available` to avoid
  optimistic cost estimates keeps working, and now reflects learned unavailability too
  because `BuildNodePoolMap` and `NewScheduler` both run `FilterUnavailable`.

- **Disruption budgets** are unrelated and evaluated independently; nothing here changes
  how many nodes may be disrupted.
- **Capacity buffers** drive provisioning through the dynamic provisioner and are
  throttled by the same `Admit` + requeue rules as other pending pods.
- **Static capacity** does not use the scheduler; the pool budget is the throttle. See
  [Static capacity](#static-capacity).
- **Topology spread.** Unchanged in mechanism, but the filter makes an existing interaction
  fire more often: soft spreads concede the instance type before the spread, and a fully
  backed-off zone stays a topology domain. See [Topology spread](#topology-spread).
- **`minValues`.** If filtering backed-off offerings drops an instance-type family below a
  `minValues` requirement, scheduling fails through the existing `minValues` path rather
  than silently launching something non-compliant. Under `MinValuesPolicyBestEffort` it
  relaxes as it does today.

### Observability

Signals are event-driven or 0/1 snapshots. Nothing here is a continuous function of wall
time (no refilling token float, no decreasing "seconds remaining" gauge). `IsAvailable` /
`constrained` at scrape time is a clock comparison against `until` / `nextAdmit` and does
not mutate tracker state.

Emit series only for keys and NodePools that exist in the tracker (previously failed). Do
not duplicate the provider's per-offering availability gauge for healthy offerings.

| Signal | Type | Purpose |
| ------ | ---- | ------- |
| `karpenter_offerings_launch_failures_total` | counter, by `instance_type`, `capacity_type`, `zone` | Attributed launch failures. Shows *where* capacity is short, which the current NodePool-labelled metric cannot. Increment in `launchNodeClaim` on ICE, once per key in `Keys` (once with empty labels if `Keys` is empty). |
| `karpenter_offerings_unavailable` | gauge 0/1, by `instance_type`, `capacity_type`, `zone` | 1 while `!IsAvailable(key)` for a tracker entry. Deleted when the entry is reset on success **or expires** at `until + maxDelay`, so the series set really is "currently backed-off offerings" rather than every offering that has ever failed. |
| `karpenter_nodepools_launch_constrained` | gauge 0/1, by `nodepool` | 1 while the pool's aggregate budget is constrained. The "is this NodePool being throttled" signal. Note a released pool can still be throttling risky launches, which is why the counter below is labelled by reason. |
| `karpenter_nodepools_launch_burst` | gauge, by `nodepool` | The `burst` *ceiling* at the current recovery level — not `remaining`. Deleted when the pool is released. Distinguishes "recovering" (rising) from "stuck at the floor" (pinned at 1); graphing consumption here instead would show a per-window sawtooth and hide the ramp. |
| `karpenter_nodepools_launch_throttled_total` | counter, by `nodepool`, `reason` (`constrained`, `risky`) | Incremented by the `Admit` caller (`Provisioner.Reconcile` or `static.provisioning`) once per NodeClaim not created because `Admit` returned false. Distinguishes "throttled" from "nothing to do," and aggregate throttling from risky-probe throttling on a released pool. Not incremented from `CreateNodeClaims`. |

The primary success metric needs no new instrumentation: the ratio of
`karpenter_nodeclaims_disrupted_total{reason=insufficient_capacity}` to
`karpenter_nodeclaims_created_total` is the churn share in the motivation table, and it
should fall from >90% to near zero.

### Edge Cases

- **Provider does not populate `Keys` on the ICE error.** Core cannot attribute the
  failure to an offering, so it `FailPool`s only. The aggregate throttle still engages and
  the incident is still bounded; only the per-offering isolation is lost. This is alpha
  behaviour for every unmodified provider, including AWS until the follow-up PR lands.
- **Attribution is wrong.** A provider may report keys it did not actually attempt. An
  incorrectly backed-off offering is probed when its window elapses; a success clears it.
  Wrong keys plus `FailPool` rate-limit *every* launch from that pool to one per
  `probeInterval`, not just the misattributed offering — that is why admit ordering prefers
  NodeClaims backed by never-failed keys (so a wrongly-floored pool recovers on its next
  window rather than burning windows on bad probes), why the first success doubles `burst`
  immediately, and why `Keys` are values rather than a guess at `*Offering` identity.
- **First batch and restart.** State is in-memory, so all offerings return to available
  and all pools to unconstrained. Worst case is one burst of doomed launches before the
  trackers re-arm — the same tradeoff accepted by `PreviouslyUnseenNodePools` and the
  drift backoff. Expected first-batch size is whatever `Solve` already produces today
  (pending pods packed onto new NodeClaims), not the offering catalog size. Persisting to
  NodePool status was considered and rejected: the offering key is cloud-scoped and shared
  across NodePools, so it does not belong in any single NodePool's status.
- **Mixed pool, zonal pods.** "The NodePool is released after a healthy-AZ success ramp"
  plus "the filter only removes backed-off keys" means pods pinned to a dead AZ keep
  probing as that AZ's windows elapse, on a pool that is no longer constrained. Their rate
  is `riskyBurst` per `probeInterval` per NodePool — the offering window alone does not
  bound it, since any number of NodeClaims may target the same probe-eligible key. Admit
  ordering compounds this while the pool is constrained, admitting healthy-AZ NodeClaims
  ahead of dead-AZ probes. The residual gap is that all dead AZs share one risky budget, so
  a pool with three dead AZs probes them round-robin rather than one each; a
  `(NodePool, zone)` risky budget would close it and is an
  [open question](#open-questions), not v1.
- **Offering set churn.** Providers add and remove instance types and zones. Absent keys
  are available, so a new offering is never penalised. Entries expire at `until + maxDelay`
  regardless of whether the key still appears in `GetInstanceTypes`, which covers both
  directions: a withdrawn offering's entry goes away, and so does the entry for an offering
  that is still advertised but no longer requested. Keying expiry on "unseen" alone would
  not, because an offering the provider still lists is seen on every `GetInstanceTypes` call
  whether or not any pod wants it.
- **Spot and on-demand are independent.** `capacityType` is part of the key, so a spot
  shortage never throttles on-demand for the same instance type and zone, and vice versa.
  Spot-to-spot consolidation is unaffected when only on-demand is short.
- **Shared offerings across NodePools.** Two NodePools selecting the same instance type in
  the same zone share the per-offering entry — correct, since they contend for the same
  cloud capacity — but have independent budgets, so one NodePool's thundering herd does not
  constrain another's. The sharing does reach across pools in one direction: a pool that has
  never failed a launch itself can find its NodeClaims classified risky because a *different*
  pool's ICE armed the offerings they depend on. That is intended. The launches really are
  aimed at capacity another pool just failed to get, and the bound is per-pool, so each pool
  still gets its own probe.
- **Reserved offerings with an exhausted reservation.** Provider reports the offering
  unavailable / `ReservationCapacity == 0`; it is filtered; strict mode defers and
  fallback mode falls through to on-demand/spot. Unchanged from today. ICE on a reserved
  offering additionally arms the backoff for that key.

## Alternatives Considered

### Alternative 1: Replace `Available` with a learned count (token bucket)

Model each offering as a token bucket: ICE zeroes tokens and halves a ceiling; success
doubles the ceiling and refills; the scheduler binpacks against `min(provider, learned)`.
Collapse `ReservationCapacity` into the same field and generalize `ReservationManager` to
every finite-count offering.

Rejected for v1. [#3198](https://github.com/kubernetes-sigs/karpenter/issues/3198) is a
churn-throttle problem; a remaining-capacity integer is a harder capacity model. The
bucket's `tokens + elapsed/refillInterval` is a continuous function of time, which makes
honest gauges expensive (scrape-time mutation or a ticker goroutine). Speculatively
debiting every compatible offering does not scale to hundreds of GPU offerings, and
it fights partial recovery: price ordering sends the next probe at the cheapest refilled
offering, not the AZ that just succeeded. `ReservationCapacity` is also the wrong number
to collapse — it is keyed by reservation ID, not by offering.

### Alternative 2: Availability as a probability

Model each offering's availability as a probability, decayed by failures and restored by
successes and time. Rejected: any probability still collapses into a launch/don't-launch
decision, and it is harder to explain in an event, harder to graph, and harder to tune.

### Alternative 3: Keep the boolean, back off per NodePool only

Apply the drift backoff pattern to the provisioning path: on ICE, back the whole NodePool
off exponentially. Simpler, no offering tracker, no provider attribution. Rejected as the
*sole* mechanism because the granularity is wrong for use case 3 — it penalises healthy
zones and instance types in a NodePool where only one offering is short. This design keeps
the useful half as the per-NodePool admit, layered on the per-offering backoff.

### Alternative 4: Keep the ICE'd NodeClaim in a backed-off state

Instead of deleting on ICE, leave the NodeClaim in a backed-off state so the provisioner
does not regenerate it. Rejected because doomed NodeClaims are user-visible objects consuming
NodePool limits and cluster state, and their presence still costs provisioner scheduling
work on every loop. With a launch budget, the delete-and-recreate cycle is bounded anyway.


### What we already tried: cluster-state sync

We first suspected cluster-state sync was the bottleneck: `Cluster.Synced` treats a
NodeClaim without a resolved provider ID as unsynced, which gates the provisioning loop,
and ICE churn produces a steady stream of such NodeClaims. We shipped a patch that skips
the provider-ID sync check and deployed it to both clusters in the motivation table. It
worked as intended — `karpenter_cluster_state_unsynced_time_seconds` drops from a
0.01–0.2s baseline to identically zero at the deploy — and it changed nothing else. ICE
share held at 92.5% in `cluster-a`, and `cluster-b` got worse (63.5% → 84.8%), with the
single largest churn event in the entire window landing five days *after* the deploy.
Sync latency is a symptom of the churn, not its cause; the fix has to be on the create
path.

## Backward Compatibility

- **No CRD or user-facing API changes.** No NodePool or NodeClaim field is added, and
  existing YAML is unaffected. `Offering.Available` and `Offering.ReservationCapacity`
  stay fields.
- **`NewInsufficientCapacityError` gains a variadic `keys ...OfferingKey` parameter** and
  `InsufficientCapacityError` gains a `Keys` field. Existing call sites compile unchanged.
  `pkg/state/cost.OfferingKey` is aliased to `cloudprovider.OfferingKey` (field rename
  `InstanceName` → `InstanceType` in that package).
- **Behaviour with the gate off:** core records no outcomes, filters nothing, and does
  not rate-limit. Launch rate and offering selection match today.
- **Behaviour with the gate on and an unmodified provider:** throttling at NodePool
  granularity only (`FailPool` / `Admit`). That is enough to bound the [#3198](https://github.com/kubernetes-sigs/karpenter/issues/3198)
  create/delete loop, including static. Use cases 1 and 3 are **not** met until the
  provider populates `Keys`.
- **AWS (and other providers) `Keys` is a separate PR**, not this RFC. Alpha in core does
  not block on it. Beta does: healthy-AZ isolation and mixed-pool probe preference require
  attributed ICE. KWOK fills `Keys` in-tree for core tests.

## Graduation Criteria

**Alpha (`LaunchBackoff=false` by default).** The mechanism changes provisioning rate
under failure, so it needs real-cluster exposure before becoming default. Ships in core
with: `cloudprovider.OfferingKey` and `InsufficientCapacityError.Keys`; `FilterUnavailable` at
`NewScheduler` and `BuildNodePoolMap`; the NodePool budget including the `burst` ramp, the
risky budget, entry expiry, and admit ordering, consumed by the provisioner and static
controller *not* by `CreateNodeClaims`; the read-only `IsConstrained` peek in the disruption
`Queue`, keyed on replacement NodePools;
[`OfferingsUnavailableError`](#attributing-unschedulability-to-unavailable-offerings) in
`filterInstanceTypesByRequirements`; metrics; static `RequeueAfter: NextAdmit` with the
matching `ReleaseNodeCount`; and singleton provisioner `RequeueAfter`, capped at
`probeInterval`, only when every remaining pending pod is either offering-blocked or on an
omitted NodeClaim. Provider `Keys` population is out of scope for this alpha.

Test coverage, beyond unit tests of the window and `burst` transitions against a fake clock:

- **The filter actually filters.** Assert a backed-off offering is absent from
  `AllocatableOfferingsList()` on the `FilterUnavailable` copy **and** from `BuildNodePoolMap`'s type
  map, so the `sync.Once` ordering hazard cannot regress silently.
- **The filter is free when idle.** Assert an empty tracker returns the provider's instance
  types with pointer identity intact, and that a shortage in one instance-type family
  `DeepCopy`s only that family. Assert the provider's cached `Offering.Available` is
  unchanged after `FilterUnavailable` (mutating without `DeepCopy` must fail this test).
- **`CreateNodeClaims` never omits.** A constrained pool's drift replacement still
  creates exactly `len(replacements)` NodeClaims, or the command is skipped before
  cordon — never a name-count mismatch in `createReplacementNodeClaims`.
- **Churn is bounded.** A `suite_test` reproduction of the scenario from
  [#3198](https://github.com/kubernetes-sigs/karpenter/issues/3198), asserting a bounded
  number of launches per interval, covering both the dynamic provisioner and
  `static.provisioning` (ICE delete must `RequeueAfter` rather than spin).
- **Recovery does not blast.** After a single successful probe against an otherwise dead
  pool, assert at most `burstMax` NodeClaims are created before the next failure re-arms
  backoff.
- **Release does not blast either.** This is the regression test for the partial-recovery
  hole. Drive a pool to *released* (`constrained == false`) via repeated healthy-AZ
  successes while a dead AZ's offerings stay in the tracker, then advance the clock past
  those offerings' windows with thousands of pods pinned to the dead AZ. Assert at most
  `riskyBurst` NodeClaims per `probeInterval`, not one per eligible offering and not the
  whole pending batch. Run the same assertion with the offering window expired but the fake
  provider still reporting `Available=false`, which is the provider-cache-expiry shape of
  the same scenario.
- **Mixed pools make progress.** A constrained pool with one healthy AZ admits the healthy
  NodeClaims ahead of dead-AZ probes and is released within a bounded number of windows.
  Assert healthy-AZ NodeClaims are **not** charged to the risky budget once their offerings
  clear, so recovery is not itself rate-limited. A mixed `Solve` (healthy pool A +
  constrained pool B) does **not** `RequeueAfter` the singleton.
- **Offering-blocked vs other unschedulable.** A pod that fails on affinity, on resources,
  or on `minValues` is *not* wrapped in `OfferingsUnavailableError`. A pod that fails only
  because no usable offering remained **is**, whether the offering was cleared by
  `FilterUnavailable` or by the provider — the layer-agnostic case is the one an earlier
  draft got wrong, so assert it explicitly. Assert the wrapping happens when
  `filterInstanceTypesByRequirements` empties the instance-type set, i.e. on a path that
  never reaches `offeringsToReserve`.
- **`risky` is scoped to usable offerings.** The silent-failure test for the predicate. Give a
  NodePool one never-failed offering that the *provider* reports unavailable alongside a set of
  probe-eligible failed ones, then assert NodeClaims are still classified risky and still
  throttled. A predicate written over the full compatible set passes every other test in this
  list and fails only this one.
- **Attribution fires at all.** The silent-failure test for the discriminator. Assert a pod
  whose offerings are all unavailable is wrapped in `OfferingsUnavailableError` and that the
  provisioner sleeps. Keying off `requirementsAndFits` would make this unreachable while
  leaving every "does not wrap" assertion green, so this test has to fail if the discriminator
  regresses to that field.
- **`Admit` consumes nothing when it rejects.** A risky `Admit` refused because
  `riskyRemaining == 0` leaves `remaining` unchanged on a constrained pool. Follow it with a
  non-risky `Admit` in the same window and assert that one is admitted.
- **Throttled pods are still accounted for.** A loop where `Solve` produced NodeClaims and
  `Admit` omitted *all* of them must `RequeueAfter`, not spin, even though those pods never
  appear in `Results.PodErrors`.
- **Soft constraints still relax.** With one AZ fully backed off and a pod carrying a
  `ScheduleAnyway` zonal spread, assert the pod schedules into a healthy AZ in the *same*
  `Solve` rather than being held pending — i.e. `OfferingsUnavailableError` did not get added
  to `trySchedule`'s reserved-offering short-circuit. Assert the same pod prefers a pricier
  instance type in its own AZ over relaxing, when one with a usable offering exists, so the
  concession order is instance type before spread. A `DoNotSchedule` spread is *not* relaxed
  and does wait for the window.
- **State decays.** An entry untouched past `until + maxDelay` is gone: assert its metric
  series is deleted, `HasFailed` is false, and a subsequent first failure starts at `level=1`
  with a `baseDelay` window rather than jumping to `maxDelay`. Assert a deleted NodePool's
  pool entry is removed.
- **Static reservations do not leak.** A constrained static NodePool that admits fewer
  NodeClaims than its replica gap releases the difference: the pool's reserved count
  settles at the number actually created, and repeated constrained reconciles do not walk
  the pool up to its `spec.limits` node limit.
- **A capped sleep cannot strand pods.** With an offering backed off at `maxDelay`, assert
  the provisioner requeues after `probeInterval` rather than the 10m window, so a NodePool
  created during the sleep is acted on within one interval.
- **Disruption peeks, never consumes.** A constrained pool's `remaining` and `nextAdmit` are
  unchanged after a disruption `IsConstrained`, and a command skipped for a constrained pool
  is never cordoned.
- **Disruption gating is keyed correctly.** A multi-node consolidation command whose
  candidates span pools is skipped when *any replacement's* pool is constrained, is admitted
  when only a *candidate's* pool is constrained, and a replacement-free command (empty node
  or expiration) is never gated at all.

**Beta (default on).** Requires evidence from clusters that reproduce the issue that the
ICE share of created NodeClaims drops substantially, **and** that pods which did get a
NodeClaim do not wait measurably longer to reach `Launched` than a same-cluster gate-off
baseline. **Blocked on the AWS provider PR that populates `Keys`** (or equivalent in the
provider under test): without it, use cases 1 and 3 cannot be evaluated.

**GA.** Remove the gate. Resolve whether providers should keep their own ICE caches or
defer to core's backoff. A scalar remaining-capacity follow-up is out of scope for this
gate.

## Open Questions

1. **Is the offering key granular enough?** ICE can be narrower than
   `cloudprovider.OfferingKey` (`InstanceType`, `CapacityType`, `Zone`) — subnet, placement
   group, or NodeClass-specific constraints. Adding NodeClass to the key would isolate those
   but fragments the signal and loses cross-NodePool sharing. Proposed: start with the
   three-tuple, matching the granularity providers' own ICE caches already use.
2. **Should the risky budget be keyed by `(NodePool, zone)` as well?** The rate is bounded
   either way; what zone-keying would change is *fairness* between dead AZs, which currently
   share one budget and are therefore probed round-robin rather than one probe each per
   window. Proposed: NodePool-only for v1, since the bound is the correctness property and
   the fairness gap only delays discovering that one dead AZ recovered before another.
3. **Is a per-NodePool risky budget the right scope, or should it also be capped
   cluster-wide?** The aggregate guarantee is `riskyBurst` per `probeInterval` *per
   NodePool*, so a cluster with 100 simultaneously-shorted NodePools still sees ~200 doomed
   launches per minute. That is two orders of magnitude better than the observed incident and
   it keeps one tenant's shortage from throttling another's probes, which is why it is
   proposed for v1 — but the scaling is linear in NodePool count and worth watching in beta.
4. **Should a fully backed-off zone stop being a topology domain?** Today it does not:
   `buildDomainGroups` reads `InstanceType.Requirements`, not `Offerings.Available()`, so the
   dead zone keeps a count of 0, keeps getting elected as least-loaded, and — under
   `DoNotSchedule` — holds the global `min` at 0 so every healthy zone is capped at
   `maxSkew`. Pruning the zone from `InstanceType.Requirements` inside `FilterUnavailable`
   would close it, but those requirements feed label resolution for the launched NodeClaim,
   so pruning changes what the node advertises and risks a mismatch with the offering the
   provider actually picks. Proposed: out of scope for v1, on the grounds that the provider's
   own ICE cache already produces this behaviour and the RFC only extends its duration.
   Revisit if beta clusters show spread-constrained workloads stalling rather than skewing.
5. **Is `burstMax = 8` the right release threshold, and should `burst` decrease
   multiplicatively rather than resetting to 1?** A halving decrease would preserve more of
   a large pool's ramp across an isolated failure, at the cost of a second knob and a
   slower clamp when capacity genuinely disappears mid-recovery. Proposed: reset to 1,
   matching the "escalation tracks failed windows" rule on the offering side, revisited if
   beta clusters show recovery is too slow for large scale-outs.
6. **Should non-ICE launch failures feed the trackers?** A NodeClass misconfiguration
   produces the same churn shape but is not a capacity problem, and
   `NodeRegistrationHealthy` already covers it — imperfectly, since it triggers on
   registration timeout rather than launch failure. Proposed: ICE only for v1.
7. **Should registration and initialization timeouts count as failures?** A launch that
   succeeds but never registers consumed real capacity, so it is not an ICE, but it is a
   NodePool we want to slow down. The drift backoff RFC chose to treat timeouts uniformly
   with ICE. Proposed: exclude for v1, since `liveness.go` already has timeout handling.

## References

- Issue: [Karpenter NodeClaim churn for Insufficient Capacity degrades reconciliation
  (#3198)](https://github.com/kubernetes-sigs/karpenter/issues/3198)
- Maintainer discussion: `#karpenter-dev` — ICE as a scalar with bucket refill versus a
  boolean. v1 takes the boolean (unavailable until `until`, never locked out) and leaves
  a scalar as a possible follow-up behind the same tracker surface.
- Related: [Per-NodePool exponential backoff for drift disruption](./drift-per-nodepool-backoff.md)
  and [#3080](https://github.com/kubernetes-sigs/karpenter/issues/3080) — the disruption-side
  counterpart to this provisioning-side change. Offering windows reuse that RFC's
  level / until / idempotent-`Fail` shape.
- Wiring precedent for shared in-memory state: `nodepoolhealth.State`, constructed once in
  `pkg/controllers/controllers.go` and injected by pointer into several independently
  concurrent controllers. Only that injection pattern transfers; its ring-buffer API does
  not.
- [NodeRegistrationHealthy status condition](./noderegistrationhealthy-status-condition.md)
  — the existing signal for launch and registration misconfiguration, which this RFC
  deliberately does not duplicate. See [Open Questions](#open-questions).
- Unchanged reservation path: [Capacity reservations](./capacity-reservations.md) and
  `pkg/controllers/provisioning/scheduling/reservationmanager.go`.
