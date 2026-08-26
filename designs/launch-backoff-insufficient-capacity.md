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
   pending pods, where most of the requested offerings are ICE'd. Today: sustained NodeClaim
   churn at controller throughput, saturated workqueues, and degraded provisioning for
   unrelated NodePools. Desired: the unsatisfiable portion is retried at a bounded rate while
   the satisfiable portion launches at full speed.
2. **Burst at cache expiry.** Thousands of pods queued against a single ICE'd offering. Today:
   a write storm every time the provider's cache entry expires. Desired: recovery is
   rate-limited, so the first attempts after expiry are probes rather than the full batch.
3. **Zone-scoped shortage in a multi-AZ NodePool.** One AZ is out of an instance type; the
   other three are healthy. Desired: launches into the healthy AZs are preferred and
   delayed by at most one probe interval.

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
   elapses, then one probe is allowed. Applied by `FilterUnavailable` to *DeepCopy'd*
   instance types at the two scheduling `GetInstanceTypes` call sites. See
   [Applying the backoff filter](#applying-the-backoff-filter).
2. **Per-NodePool launch budget.** While a NodePool is constrained, the *callers* of
   `CreateNodeClaims` (the dynamic provisioner and `static.provisioning`) `Admit` one
   NodeClaim per `probeInterval`, ramping up as probes succeed and returning to one on any
   failure. `CreateNodeClaims` itself is unchanged: it still creates every NodeClaim it is
   given. Disruption only *peeks* with a read-only `CanAdmit`; it never consumes a probe.
   Without the budget, a wide NodePool's hundreds
   of independently expiring offering windows sum back up to the churn rate we are trying
   to eliminate — see [Why both scopes are needed](#why-both-scopes-are-needed).

An offering with no recorded failures is not in the tracker, and a NodePool with no recorded
ICE is unconstrained, so **a cluster that never hits a launch failure behaves as it does
today.**

```
                    ICE                         window elapses
  (absent/healthy) ------> (backed off) ------> (probe eligible)
         ^                      |                      |
         |                      | ICE (no-op           | ICE
         | success              |  inside window)      v
         +----------------------+---------------- (level grows, new window)
```

### How It Works

#### Offering backoff

New package `pkg/state/launchbackoff`. Two maps: offering entries keyed by
`cloudprovider.OfferingKey`, pool entries keyed by NodePool UID. Written from
`launchNodeClaim` (`Fail` / `Succeed`) and from the `Admit` callers; read from scheduling
(`FilterUnavailable`, `IsAvailable`, `HasFailed`) and from the disruption `Queue`
(`CanAdmit`).

```go
type offeringEntry struct {
	level int       // failed windows (0 == healthy / absent)
	until time.Time // unavailable before this time
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
| `baseDelay` | `30s`   | First probe after a short delay. Fast enough to recover if ICE clears; slow enough to stop the thundering herd. Shorter than the AWS ICE cache's 3m TTL so core, not the provider cache, owns retry spacing. |
| `maxDelay`  | `10m`   | Absolute ceiling. Reached after ~6 consecutive failed windows (`30s → 1m → 2m → 4m → 8m → 10m`). |

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

A provider that reports an offering unavailable still wins. A provider whose cache entry
has expired does *not* make the offering usable until core's window elapses. This is what
prevents the TTL-expiry thundering herd.

#### Two NodeClaims, same pool

A NodeClaim carries a set of acceptable instance types and requirements, and the provider chooses the instance.

- **Pod can use a healthy offering.** The NodeClaim's compatible set still contains at
  least one offering that is not backed off. That offering survives the filter and the
  NodeClaim launches immediately if the NodePool is unconstrained. Backed-off siblings in
  the set are simply absent; they are not charged.
- **Every compatible offering is backed off.** The filter leaves nothing. `Solve` does not
  produce a NodeClaim for these pods. They stay pending and are surfaced by
  `karpenter_scheduler_unschedulable_pods_count`. When the earliest `until` elapses, one
  offering becomes probe-eligible; the NodePool budget (below) decides whether that probe
  is admitted this loop.

The filter therefore binds for a given pod exactly when every offering it could land on is
backed off. The NodePool budget is coarser and can briefly delay a satisfiable NodeClaim: a
pool constrained by an ICE elsewhere rate-limits *all* of its launches until a success
releases it. Admit ordering keeps that delay to roughly one `probeInterval` by spending each
window on the NodeClaims most likely to succeed — see
[Admit ordering](#per-nodepool-launch-budget).

#### Per-NodePool launch budget

The NodePool budget is a *rate limit*. A boolean window is a poor fit here, when it elapses and
then admits the full `Solve`, the batch would reproduce the cache-expiry thundering herd at NodePool
scope.

```go
type poolEntry struct {
	constrained bool
	burst       int       // admits allowed per window; 1 after a failure, doubled per success
	nextAdmit   time.Time
}

func (t *Tracker) Admit(nodePoolUID types.UID) bool
func (t *Tracker) FailPool(nodePoolUID types.UID)
func (t *Tracker) SucceedPool(nodePoolUID types.UID)
func (t *Tracker) NextAdmit(nodePoolUID types.UID) time.Time
```

- **Unconstrained** (absent or `constrained == false`): `Admit` always returns true.
  The caller passes the full set into `CreateNodeClaims`, which fans out as it does today.
- **`FailPool`:** set `constrained = true`, `burst = 1`, `nextAdmit = now + probeInterval`.
  No-op if already constrained and `now < nextAdmit`.
- **`Admit` while constrained:** admit up to `burst` NodeClaims per window. If the window's
  allowance is spent and `now < nextAdmit`, return false; otherwise consume one and, when the
  allowance is exhausted, set `nextAdmit = now + probeInterval`.
- **`SucceedPool`:** double `burst`. Once it would exceed `burstMax`, clear `constrained` and
  the pool is back to full speed.

**Recovery ramps; it does not jump.** Clearing `constrained` on the *first* success would
admit the entire `Solve` batch on the next loop. Doubling `burst` bounds the overshoot to
`burstMax` doomed NodeClaims while still recovering quickly: successes within one window
compound, so a recovered pool reaches full speed in two or three windows rather than one,
avoiding a thundering herd when a launch succeeds.

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

So for a constrained pool, partition the `Solve` result before admitting: NodeClaims whose
compatible offerings include at least one key with no tracker entry go first, then the rest.
The first group is likely to succeed, and successes are what ramp the pool back up. Note
this is a different question from `IsAvailable`, which is also true for a probe-eligible key
whose window just elapsed — the partition needs "never failed," not "allowed to try," so the
tracker exposes entry presence separately. The walk runs only for pools that are actually
constrained; an unconstrained pool admits everything and never pays for it.

**Admit is a caller concern, not a `CreateNodeClaims` concern.** That function is a shared
create fan-out; `Queue.createReplacementNodeClaims` requires `len(names) ==
len(replacements)` after nodes may already be cordoned. Silently omitting inside
`CreateNodeClaims` would fail that check.

| Caller | Behavior |
| ------ | -------- |
| `Provisioner.Reconcile` | `Admit` each `Solve` NodeClaim (consume), keyed by `NodeClaimTemplate.NodePoolUUID`. Pass only the admitted set into `CreateNodeClaims`. Increment `karpenter_nodepools_launch_throttled_total` once per omitted NodeClaim. |
| `static.provisioning` | Same consume-then-create; see [Static capacity](#static-capacity). |
| Disruption `Queue` | **Never *consume* `Admit`.** Peek with read-only `CanAdmit` before `markDisrupted` and skip the candidate while the pool is constrained. If `Create` itself ICE's, the existing `StartCommand` error path applies. |

`CanAdmit` answers "is this pool constrained right now" without decrementing `burst`, so a
disruption peek never steals a probe from the provisioner. It must run **before**
`markDisrupted`, which cordons the candidates ahead of launching replacements
(`queue.go`); checking afterwards would leave a cordoned node whose replacement was
refused. Skipping a candidate is already a supported outcome — the command is simply not
started — whereas *consuming* would tempt an implementation to omit inside
`CreateNodeClaims` and break the `len(names) == len(replacements)` contract below.

| Parameter       | Default | Rationale |
| --------------- | ------- | --------- |
| `probeInterval` | `30s`   | One window per 30s per constrained NodePool. Independent of offering `baseDelay` so a pool with 200 backed-off offerings cannot emit 200 probes when their windows line up. |
| `burstMax`      | `8`     | Ceiling on admits per window before the pool is released outright. Caps wasted launches after a spurious recovery at 8, and reaches full speed in ~3 windows (`1 → 2 → 4 → 8`) when capacity is genuinely back. |

#### Why both scopes are needed

The per-offering backoff is the right *filter* — it isolates a bad zone from healthy ones
and reuses `Available`. It is not, by itself, a sufficient *throttle*. Consider the
observed incident: one NodePool, ~200 compatible offerings, all ICE'd. Each window elapses
independently. Even with exponential backoff, if many keys share a `until` (they often
will: they failed in the same first batch), expiry admits one NodeClaim per offering per
loop, which is the blast again.

Per-offering granularity is what makes the model correct for use case 3; a per-NodePool
admit of one per `probeInterval` is what makes the aggregate bounded. With one probe per
30s per NodePool, a fully-constrained NodePool emits ~240 NodeClaims over two hours
instead of 39,137, a ~160× reduction. A NodePool that can still launch onto a healthy
offering ramps back to full speed within a few windows, and admit ordering ensures those
healthy launches are what the windows are spent on.

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

#### Attributing unschedulability to backoff

The requeue below has to tell "these pods have nowhere to go until a window elapses" apart
from "these pods are unschedulable for a reason no delay will fix." `FilterUnavailable` alone cannot
answer that: it sets `Available = false`, which downstream is indistinguishable from an
offering the *provider* reported unavailable. So the judgement is made where offerings are
selected, and reported with a typed error mirroring the existing `ReservedOfferingError`:

```go
// pkg/controllers/provisioning/scheduling
type BackoffUnschedulableError struct {
	error
	NextEligible time.Time // earliest `until` across the offerings that were filtered
}

func NewBackoffUnschedulableError(err error, nextEligible time.Time) BackoffUnschedulableError
func IsBackoffUnschedulableError(err error) bool // errors.As, as IsReservedOfferingError does
func (e BackoffUnschedulableError) Unwrap() error

// mirrors Results.ReservedOfferingErrors / Results.DRAErrors
func (r Results) BackoffUnschedulableErrors() map[*corev1.Pod]error
```

**Where it is produced.** The reserved-offering path already makes the same *shape* of
judgement — compatible offerings existed, but none were usable — and reports it with a
typed error rather than a generic one:

```go
if hasCompatibleOffering && len(reservedOfferings) == 0 {
	return nil, NewReservedOfferingError(...)
}
```

Backoff attribution is the direct analogue, in that same offering-selection path. While
filtering a NodeClaim's offerings, count those rejected because
`HasFailed(key) && !IsAvailable(key)` — meaning `FilterUnavailable` cleared `Available`, not the
provider. If nothing survives and that count is non-zero, the pod failed *because of*
backoff and the error carries `min(until)` over those keys. If the count is zero, nothing
changes: the pod gets today's error and the requeue reads it as "spin, do not sleep."

**What it costs.** The scheduler needs a read-only tracker reference, because
`HasFailed` / `IsAvailable` is the only thing that can say *why* an offering is
unavailable. The filter stays transparent for scheduling *decisions* — `Solve` still just
reads `Available` — but is no longer transparent for *explaining* a failure. That is
consistent with the [Invariants](#invariants) (simulation reads tracker state, never
debits), but it is a genuine addition to the scheduler's surface, called out here rather
than buried, since "existing call sites do not change" is otherwise one of this design's
selling points.

**Both directions of error are safe.** An offering may be filtered when the instance type
would have failed on resources anyway, so a pod can be labelled backoff-blocked when a
delay is not strictly the cause; the cost is bounded to one `probeInterval` of sleep on a
pod that was not going to schedule regardless. Missing the attribution yields today's
immediate requeue. Neither direction can cause a pod to be dropped or delayed
indefinitely.

`Record` treats it as it treats `ReservedOfferingError` — no error-level log or event per
loop — since a backed-off offering is an expected transient state and the affected pods are
already counted by `karpenter_scheduler_unschedulable_pods_count`.

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
if !allPendingPodsAreBackoffBlocked(results) {
    return RequeueImmediately  // affinity, resources, minValues, etc.
}
wake := min(NextAdmit / BackoffUnschedulableError.NextEligible over pools that blocked)
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
3. **t=30s, steady state.** The filter still removes every offering until `until`. `Solve`
   produces no launch for these pods; they remain pending. At ~30s the first offering
   window elapses and the NodePool `Admit` allows one NodeClaim: a single probe. If it
   ICEs, that key's level grows (`until≈now+1m`), `burst` resets to 1, and the pool's
   `nextAdmit` moves forward 30s. Sustained cost is ~2 NodeClaim cycles/minute instead
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
   NodeClaims flow to the healthy AZ. Pods *pinned* to a dead AZ (topology spread, zonal
   affinity) still wait on that AZ's backoff windows; see Edge Cases.

#### Invariants

If any of these stops holding, the corresponding part of the design must be revisited.

- **Mutations happen only on real launches.** `Fail` / `Succeed` run from
  `Launch.launchNodeClaim`. `Admit` — the only budget-consuming call — runs from
  `Provisioner.Reconcile` and `static.provisioning.Reconcile` for NodeClaims that will
  actually be created, never from `CreateNodeClaims` itself and never from disruption.
  Everything else reads: scheduling simulation (`Solve`, disruption packing) via
  `FilterUnavailable` /
  `IsAvailable` / `HasFailed`, and the disruption `Queue` via `CanAdmit`. A read must never
  decrement `burst` or move `nextAdmit`.
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
  budget. `Admit` takes the write lock once: re-check the window, decrement the remaining
  burst, return the decision.
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
| `karpenter_offerings_unavailable` | gauge 0/1, by `instance_type`, `capacity_type`, `zone` | 1 while `!IsAvailable(key)` for a tracker entry. Deleted when the entry is reset on success. Cardinality is "currently backed-off offerings," not the full instance-type catalog. |
| `karpenter_nodepools_launch_constrained` | gauge 0/1, by `nodepool` | 1 while the pool's budget is constrained. The "is this NodePool being throttled" signal. |
| `karpenter_nodepools_launch_burst` | gauge, by `nodepool` | Admits allowed per window while constrained. Deleted when the pool is released. Distinguishes "recovering" (rising) from "stuck at the floor" (pinned at 1). |
| `karpenter_nodepools_launch_throttled_total` | counter, by `nodepool` | Incremented by the `Admit` caller (`Provisioner.Reconcile` or `static.provisioning`) once per NodeClaim not created because `Admit` returned false. Distinguishes "throttled" from "nothing to do." Not incremented from `CreateNodeClaims`. |

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
  plus "the filter only removes backed-off keys" means pods pinned to a dead AZ still
  probe as that AZ's windows elapse. Their rate is one probe per offering window (capped
  at `maxDelay`), not the unconstrained create path. Admit ordering helps here too: while
  the pool is constrained, healthy-AZ NodeClaims are admitted ahead of dead-AZ probes. A
  `(NodePool, zone)` budget would close the remaining gap; it is an
  [open question](#open-questions), not v1.
- **Offering set churn.** Providers add and remove instance types and zones. Absent keys
  are available, so a new offering is never penalised. Entries whose keys stop appearing
  in `GetInstanceTypes` are inert; garbage-collect them when they have been `IsAvailable`
  (window elapsed or succeeded) and unseen for `maxDelay` so metric series do not leak.
- **Spot and on-demand are independent.** `capacityType` is part of the key, so a spot
  shortage never throttles on-demand for the same instance type and zone, and vice versa.
  Spot-to-spot consolidation is unaffected when only on-demand is short.
- **Shared offerings across NodePools.** Two NodePools selecting the same instance type in
  the same zone share the per-offering entry — correct, since they contend for the same
  cloud capacity — but have independent NodePool budgets, so one NodePool's thundering herd
  does not rate-limit another's healthy launches beyond the shared backoff.
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
`NewScheduler` and `BuildNodePoolMap`; the NodePool budget including the `burst` ramp and
admit ordering, consumed by the provisioner and static controller *not* by
`CreateNodeClaims`; the read-only `CanAdmit` peek in the disruption `Queue`;
[`BackoffUnschedulableError`](#attributing-unschedulability-to-backoff); metrics; static
`RequeueAfter: NextAdmit` with the matching `ReleaseNodeCount`; and singleton provisioner
`RequeueAfter`, capped at `probeInterval`, only when every remaining pending pod is
backoff-blocked. Provider `Keys` population is out of scope for this alpha.

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
- **Mixed pools make progress.** A constrained pool with one healthy AZ admits the healthy
  NodeClaims ahead of dead-AZ probes and is released within a bounded number of windows.
  A mixed `Solve` (healthy pool A + constrained pool B) does **not** `RequeueAfter` the
  singleton.
- **Backoff-blocked vs other unschedulable.** A pod that fails on affinity is *not*
  wrapped in `BackoffUnschedulableError`; a pod that fails only because `FilterUnavailable`
  removed its offerings is. A pod whose offerings were cleared by the *provider* rather
  than by `FilterUnavailable` is also not wrapped, since `HasFailed` is false for those
  keys.
- **Static reservations do not leak.** A constrained static NodePool that admits fewer
  NodeClaims than its replica gap releases the difference: the pool's reserved count
  settles at the number actually created, and repeated constrained reconciles do not walk
  the pool up to its `spec.limits` node limit.
- **A capped sleep cannot strand pods.** With an offering backed off at `maxDelay`, assert
  the provisioner requeues after `probeInterval` rather than the 10m window, so a NodePool
  created during the sleep is acted on within one interval.
- **Disruption peeks, never consumes.** A constrained pool's `burst` is unchanged after a
  disruption `CanAdmit`, and a candidate skipped for a constrained pool is never cordoned.

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
2. **Should the NodePool budget be keyed by `(NodePool, zone)` as well?** That would close
   the mixed-pool hole for zonal pods. Proposed: NodePool-only for v1; add zone if beta
   clusters still show dead-AZ probe storms after a healthy AZ has unconstrained the pool.
3. **Is `burstMax = 8` the right release threshold, and should `burst` decrease
   multiplicatively rather than resetting to 1?** A halving decrease would preserve more of
   a large pool's ramp across an isolated failure, at the cost of a second knob and a
   slower clamp when capacity genuinely disappears mid-recovery. Proposed: reset to 1,
   matching the "escalation tracks failed windows" rule on the offering side, revisited if
   beta clusters show recovery is too slow for large scale-outs.
4. **Should non-ICE launch failures feed the trackers?** A NodeClass misconfiguration
   produces the same churn shape but is not a capacity problem, and
   `NodeRegistrationHealthy` already covers it — imperfectly, since it triggers on
   registration timeout rather than launch failure. Proposed: ICE only for v1.
5. **Should registration and initialization timeouts count as failures?** A launch that
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
