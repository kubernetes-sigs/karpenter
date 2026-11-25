# Node Repair Under Correlated Failure

## Motivation

The [Node Repair Resiliency RFC](https://github.com/kubernetes-sigs/karpenter/pull/3192) re-cast
node repair as a *voluntary* disruption: it rides the
shared disruption budget (`spec.disruption.budgets` with a repair reason), pre-spins its
replacements (replace-then-terminate), and is vetoable. That work retired the hardcoded 20%
circuit breaker — the budget paces repair and self-clears instead of latching shut, and
ordering (`rank + age/τ`) keeps a genuine fault from being frozen behind a flood of false
positives. The breaker's freeze-then-stampede failure is already solved; this RFC does not
re-open it.

What that RFC deliberately left open is **restraint**. It framed the budget as *"a ceiling,
not a mandate"* — in-budget means repair *may* proceed, not that it *must* — and introduced a
per-NodePool `backoff` term as the "down payment" on smarter restraint. This document designs
that restraint. Because it introduces no new API, it can be iterated on without a breaking
change.

### Use Cases

Baseline for "today" is the post-Resiliency world: budget-paced, pre-spinning repair.

1. **False-positive flood.** A detector false-positive flags a large cohort of healthy GPU
   nodes across a pool. *Today:* the budget paces replacement to (e.g.) 10%/pass but churns
   the whole cohort at that rate regardless — a slow, pointless stampede. *Desired:* repair
   recognizes the correlated burst, launches **one** probe, and — since the replacements come
   up unhealthy (they re-inherit the false positive) — slows down exponentially for those
   nodes, buying time for a cause-specific signal or human veto; a genuinely-faulted node in a
   different (uncorrelated) domain is still repaired promptly.
2. **Bad image rollout.** A new node image boots unhealthy across a NodePool. Replacements
   re-inherit the bad image and come up unhealthy too. *Today:* the budget churns the pool at
   the cap. *Desired:* repair observes the failures and backs the pool off — replacing more
   won't help.
3. **Small pool, single real fault.** A 4-node pool has one bad node. *Desired:* the isolated
   fault is actioned promptly — even when that node is 100% of some failure domain — with no
   percentage that rounds down to zero.
4. **Zonal outage / network partition.** A whole zone's nodes flip unhealthy at once, but the
   nodes (and their workloads) may actually be fine — only unreachable. *Desired:* repair does
   not mass-replace into an impaired zone; it paces or defers cleanly to an authoritative
   zonal-outage signal where a provider exposes one.
 
### Invariants

1. **Start slow on a correlated burst** — when many nodes across a shared domain (zone, image,
   instance type) fail together, probe one first and widen as it learns, rather than acting on
   the whole cohort at once.
2. **Eventually converge to healthy** — never fully stop trying; pause and probe so an
   out-of-band fix (a rolled-back image, a recovered zone) is always eventually picked up.
3. **Speed up when repairs work, back off when they don't** — using the outcome of the repair
   itself as the signal.
4. **Judge success as "Ready and healthy for a while"** — a replacement that comes up and then
   fails shortly after is a failed repair, not a success.
5. **Don't let past success mask a new correlated failure** — a domain that launched healthily
   for days gives no evidence about a correlated event that begins now; correlation is read
   from the current fault population, not launch history.
6. **Stay safe with no memory** — a freshly restarted or crash-looping controller that rebuilds
   no state is still conservative; history is an optimization, never a correctness requirement.

### Non-Goals

- **Re-deciding that repair is voluntary / budgeted / pre-spinning.** Established by the Node
  Repair Resiliency RFC. This design depends on the budget (as its ceiling), pre-spin (which
  makes a replacement observable as a *probe*), and ordering (which keeps the isolated fault
  first). They are dependencies, not re-litigated.
- **Diagnosing the cause.** This design does not decide *why* nodes are unhealthy (bad image
  vs. flaky detector vs. zonal event). It reacts to the *shape* of failure (correlated vs.
  isolated) and to *outcomes* (did the repair work), not to a root-cause classification.
- **Per-reason policy, drain semantics, veto.** Orthogonal; covered by the Resiliency RFC or
  its own follow-ups.

## Proposal

No new configuration. Start with three failure domains: **NodePool**, **Zone**, and **Policy**
(the repair policy / condition being reacted to). Zone is the obvious location domain; NodePool
is a proxy for non-zone domains like machine image or instance family; Policy is a proxy for a
bad detector.

For each failure domain, disruption maintains two dials:

- **Width `L`** — how many *unproven* repairs may be in flight in the domain at once. Starts at
  **1**. Each **proven** success **doubles** it (×2); each failure **cuts** it (reset to 1, or
  halve — see open questions). **Floor 1 — never 0.** Repair admits `min(L, budget_headroom)`,
  so pacing is always at least as restrictive as the operator's budget and can never exceed it.
  Width resets when a domain has no unhealthy nodes, so a new burst starts slow again
  (invariant 5).
- **Cooldown `t`** — how long to wait after an attempt before the next attempt *in that domain*.
  Starts at `t_min`. Success shrinks it toward `t_min`; each failure backs it off exponentially
  with jitter (`t ← min(t_max, t·2) ± jitter`). **Capped at `t_max`**, so repair always
  eventually retries. This generalizes the per-NodePool drift backoff
  ([#3128](https://github.com/kubernetes-sigs/karpenter/pull/3128)).

A repair becomes **proven** only once its replacement has held **Ready *and* Healthy for a
dwell `d`** (invariant 4); a still-pending probe opens neither dial.

### Gathering Evidence

Every repair action's outcome is evidence indicating if additional repair actions will be helpful.
In order to ensure the information gathered is useful, replacements must be restricted to the same 
domain as the original node. For example, if replacing a node in zone A, the replacement must also be in zone A.
This cannot apply to Policy, as all nodes are eligible for a given repair policy. 

### Starting values

All tunable, all with safe defaults; revisit with data.

| Knob | Start | Meaning |
|---|---|---|
| Increase | ×2 | proven success doubles `L` |
| Decrease | reset to 1 | any failure cuts `L` to the floor (halve is an open question) |
| Dwell `d` | 5 min | how long a replacement must hold Ready+Healthy to count as proven |
| `t_min` | 1 min | initial cooldown; matches the drift per-NodePool backoff base delay |
| `t_max` | 10 min | cooldown ceiling, so repair always eventually retries |

### Combining domains

A candidate is in several domains at once (a NodePool *and* a Zone *and* a Policy). Three rules
combine them:

- **Width — take the `min`.** Admit `min(L(nodepool), L(zone), L(policy), …, budget_headroom)`.
  The most-restricted domain sets how many probes run at once.
- **Cooldown — try if *any* domain is ready.** A candidate is eligible as long as at least one
  of its domains is out of cooldown, so a fault in a fresh domain runs right away even when it
  shares a backed-off domain with a flood.
- **Failure — fan out.** A failed probe cuts the width and arms the cooldown on the candidate's
  domains. The domain common to all failures stays throttled; a domain that caught only one
  keeps a short cooldown and stays ready.

Together these route the scarce probes to the faults most likely to be real: a flood, whose
nodes share every failing domain, cools on all of them and paces to a trickle; a
genuinely-broken node in a fresh domain stays eligible through that domain. Because width is a
`min`, adding domains later can only tighten pacing — so new domains are safe follow-ups.

### Behavior on the use cases

- **False-positive flood + one genuine fault.** The flood shares NodePool, Zone, and the
  detector's Policy. The first probe's replacement re-inherits the same detector and is
  re-flagged — it comes up unhealthy, so the probe fails and cools all three shared domains.
  Every flood node now has all its domains cooled, so repair paces them to one probe at a time,
  ever more slowly. A genuinely-broken node matching a *different* Policy stays fresh in that
  domain and is repaired promptly — not frozen behind the flood. This is what invariant 4/5
  exist for: a *systematic* false positive re-flags its own replacement, so it's caught as a
  failed probe with no diagnosis needed.
- **Bad image rollout.** One probe fails → `L` pinned at 1, `t` backs off toward `t_max`.
  Repair keeps probing one at a time, ever more slowly (never halts), but stops churning the
  pool.
- **Small pool, single real fault.** An isolated fault is its own domain → `L=1, t=t_min` →
  repaired immediately; no correlated event, no percentage rounding to zero.
- **Zonal outage / partition.** With the replacement pinned to the impaired zone (see above),
  probes can't come up healthy → `L` pinned at 1, `t` at `t_max`, and an authoritative
  zonal-outage signal (where present) forces the domain to 0, so repair defers to the zonal
  responder rather than adding load.
- **Large genuine fault (e.g. 10k-node fleet, 500 real failures).** Multiplicative `L` reaches
  full budget width in ~log₂(budget) rounds (~1 hr) instead of ~500 rounds (days) under an
  additive `+1` climb.

### Observability

The pacing must be legible without reading the code. Two per-domain gauges, labeled by domain
kind and value, expose the live state of both dials:

```
# HELP karpenter_disruption_repair_restraint_width Current width L (unproven repairs allowed at once) per failure domain.
# TYPE karpenter_disruption_repair_restraint_width gauge
karpenter_disruption_repair_restraint_width{domain_kind="zone",domain="us-west-2a"} 1
karpenter_disruption_repair_restraint_width{domain_kind="nodepool",domain="gpu"} 4
karpenter_disruption_repair_restraint_width{domain_kind="policy",domain="AcceleratorReady"} 1

# HELP karpenter_disruption_backoff_seconds Seconds remaining before the next attempt is allowed in this failure domain (0 = ready).
# TYPE karpenter_disruption_backoff_seconds gauge
karpenter_disruption_backoff_seconds{domain_kind="zone",domain="us-west-2a"} 540
karpenter_disruption_backoff_seconds{domain_kind="nodepool",domain="gpu"} 0
karpenter_disruption_backoff_seconds{domain_kind="policy",domain="AcceleratorReady"} 540
```

A domain pinned at `restraint_width=1, backoff_seconds≈t_max` is one repair is backing off from;
a domain at `backoff_seconds=0` with a high `restraint_width` is one it is confident in.
(`backoff_seconds` is the shared cross-disruption cooldown — the same dial the drift per-NodePool
backoff uses — so it is not repair-specific; `repair_restraint_width` is the repair-only
slow-start dial.)

When restraint holds a node back, repair emits a **`DisruptionDeferred`** event against the node (and
its NodeClaim) carrying the restraint-specific reason — the binding failure domain and whether it
is width-exhausted or in cooldown — so an operator can see *why* a node isn't being repaired
straight from `kubectl describe`. 

Structured log line per defer/act decision carries the full per-domain detail for deeper debugging.

## Alternatives Considered

Every alternative shares one spine — a control variable with a **hard floor of 1**, cold-started
at 1, moved by a **multiplicative rule** (climb geometrically while it works, cut sharply when
it doesn't). They differ only in what the variable is and how richly it is modeled; the chosen
design is the simplest point on that axis that meets every invariant.

- **Token bucket (rate-shaped).** The control variable is a refilling rate rather than a
  concurrency count. Equivalent invariant coverage and maps more literally onto "1 at a time,
  once per 5m," but introduces time-units and two knobs, and concurrency pairs more cleanly with
  pre-spin (you inherently have *in-flight* replacements to count).
- **Two-phase TCP climb (slow-start + congestion-avoidance).** Multiplicative below a remembered
  `ssthresh`, additive above it. The proposal *is* the slow-start half; the additive phase is a
  reasonable refinement if pure ×2 overshoots the budget too coarsely, but the budget already
  caps overshoot and a remembered `ssthresh` is per-domain history (brushing invariant 5 unless
  reset per episode).
- **Thompson sampling / Beta-Bernoulli per domain.** The "principled" explore/exploit version,
  but it needs a discounted variant to meet invariant 5, its sampling is non-deterministic
  (colliding with the RFC's deterministic ordering and hard to reason about in an incident), and
  it's a heavier sell than "one probe, widen on success."
- **Circuit breaker with a mandatory half-open probe.** The breaker with its one fatal bug — the
  zero-latch — removed, plus an outcome signal and a cooldown. Essentially the proposal with the
  increase quantized to "1 or many"; useful as the narrative bridge, not as the chosen mechanism.

Rejected outright: an explicit correlation *detector* (per-domain scoping + cold-start gets
correlation for free), changepoint detection (the cold-start-at-1 spine behaves correctly
without detecting the change), PID / phi-accrual tuning (gains hard to defend), per-condition
budgets (ordering + decline handle it without fragmenting the budget), and any *persisted*
learned state as a correctness requirement (violates invariant 6).

## Graduation Criteria / Follow-ups

1. **KWOK regression suite.** Add a KWOK-based test suite that drives the correlated-failure
   scenarios (false-positive flood, bad-image rollout, small pool, zonal partition) end-to-end,
   so the pacing behavior is guarded against regression as repair and the shared disruption
   machinery evolve.
2. **Pod-level health signals.** Extend the "is it healthy?" judgment beyond the node's
   Ready/Healthy conditions to pod-level signals — confirming the rescheduled workloads actually
   come up and stay running on the replacement — so a repair that yields a Ready node whose pods
   still crash-loop is counted as a failed probe rather than a success.
3. **Apply slow-start to other disruption reasons.** The per-domain width/cooldown restraint is
   repair-only today, but "start slow on a correlated burst, then speed up as replacements prove
   out" applies equally to drift (and consolidation) — a bad image rolled out via drift should
   back off the same way a repair loop does. This is a natural convergence: the cooldown already
   generalizes the drift per-NodePool backoff ([#3128](https://github.com/kubernetes-sigs/karpenter/pull/3128)),
   and it would address the drift launch-failure starvation in
   [#3072](https://github.com/kubernetes-sigs/karpenter/issues/3072) /
   [#3080](https://github.com/kubernetes-sigs/karpenter/issues/3080).
4. **Evaluate whether the constants need to be tunable.** The starting values (×2 increase,
   reset-to-1 decrease, 5-minute dwell, 1–10 minute cooldown bounds) are fixed with safe
   defaults. Before exposing any of them as configuration, evaluate with production data whether
   operators actually need to tune them — adding a knob only if the evidence shows one is required.

## Backward Compatibility

- No API/CRD changes are anticipated. The only operator-facing knob is the disruption budget; the
  pacing logic is automatic and requires no configuration to be safe.

## References

**Foundational / companion designs**

- [#1768](https://github.com/kubernetes-sigs/karpenter/pull/1768) — original Node Auto Repair
  RFC (modeled repair on the interruption controller; source of forceful-always + the 20%
  breaker this design replaces).
- [#3192](https://github.com/kubernetes-sigs/karpenter/pull/3192) — Node Repair Resiliency RFC
  (voluntary / budgeted / pre-spinning); this design is the correlated-failure restraint layer
  it left open.
- [#3203](https://github.com/kubernetes-sigs/karpenter/pull/3203) — Terminate-First Disruption
  (no-headroom fallback for pre-spin; see also
  [#2905](https://github.com/kubernetes-sigs/karpenter/issues/2905) /
  [#2955](https://github.com/kubernetes-sigs/karpenter/pull/2955)).
- [#2310](https://github.com/kubernetes-sigs/karpenter/pull/2310) — NodePool repair policies with
  toleration durations.

**Graduation & the breaker problem this design targets**

- [#2398](https://github.com/kubernetes-sigs/karpenter/issues/2398) — Node repair graduation
  tracking issue (umbrella for the blockers below).
- [#3031](https://github.com/kubernetes-sigs/karpenter/issues/3031) — dynamic / AZ-aware circuit
  breaking (accepted, needs-design) — this RFC is a direct response.
- [#2811](https://github.com/kubernetes-sigs/karpenter/issues/2811) — per-NodePool repair config;
  disruption-budget-style controls + graceful-drain-first.
- [#2042](https://github.com/kubernetes-sigs/karpenter/issues/2042) — repair should honor PDBs /
  `terminationGracePeriodSeconds` (graduation blocker).

**Small-pool / "works at any scale"**

- [#2134](https://github.com/kubernetes-sigs/karpenter/issues/2134),
  [#2321](https://github.com/kubernetes-sigs/karpenter/issues/2321) — a single unhealthy node
  exceeds a percentage breaker in small pools → never repairs (or the breaker is meaningless).
