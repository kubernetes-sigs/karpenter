# RFC: Disruption Performance Metrics

## Motivation

Operators need to answer four questions when disruption is slow or stuck:

1. Is Karpenter evaluating and taking disruption actions?
2. What is waiting, for how long, and in which NodePool?
3. Why is it blocked?
4. For consolidation, what proven savings are delayed?

Current metrics answer pieces of this: eligible nodes, decision duration, budgets, validation failures, timeouts, decisions, disruption initiation, and termination. They do not provide an end-to-end, per-NodePool view. This RFC adds that view for voluntary disruption, expiration, node repair, and node deletion.

### Use Cases

1. Validate a performance change. Compare latency, candidate coverage, timeouts, actions, and remaining backlog before and after a change. Lower latency with less coverage is not a win.

2. Triage a backlog. Identify the affected NodePool, oldest blocked work, and whether the cause is budget, feasibility, validation, timeout, static-pool limits, or a repair health gate.

3. Operate lifecycle disruption. See expired NodeClaims or unhealthy nodes waiting for action, and actions that take too long to delete a node.

4. Understand consolidation impact. Separate feasible, positive-saving opportunities from source cost that was never proven consolidatable.

### Non-Goals

- Change disruption behavior, budgets, scheduling, validation, or execution.
- Estimate billed or realized cloud savings.
- Run extra scheduling simulations for telemetry.
- Add node, pod, namespace, instance-type, zone, ID, or raw-error labels.

## Proposal

All new metrics include nodepool. Existing metrics are unchanged.

nodepool is the owning NodePool. Standalone NodeClaims use <none>. A shared multi-node operation across pools uses <multiple> for operation metrics; candidate metrics remain attributed to each candidate's pool.

blocker is a bounded label with these initial values: budget, consolidation_disabled, buffer_pods, not_consolidatable, static_replica_excess, static_node_limit, no_feasible_schedule, same_or_more_expensive_replacement, min_values, score, validation_churn, validation_budget, nominated, timeout, nodepool_health_gate, and cluster_health_gate.

### Proposed Spec

This RFC adds metrics only; there is no CRD, configuration, or feature gate.

#### Operator view: progress and latency

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| passes_total | Counter | nodepool, reason, consolidation_type, outcome | Completed method passes: no_candidates, no_command, selected, timed_out, or error. |
| last_evaluated_timestamp_seconds | Gauge | nodepool, reason, consolidation_type | Timestamp of the latest completed pass. Use time() - metric to detect stale data when an earlier method succeeds and prevents later methods from running. |
| candidate_evaluation_duration_seconds | Histogram | nodepool, reason, consolidation_type, stage | Duration of one SimulateScheduling call. stage is evaluate or validate. |
| simulations_total | Counter | nodepool, reason, consolidation_type, stage, outcome | Simulation result: feasible, infeasible, candidate_deleting, timeout, or error. |
| candidate_batch_size | Histogram | nodepool, reason, consolidation_type, stage | Candidates supplied to a simulation; exposes multi-node binary-search batch sizes. |
| validation_duration_seconds | Histogram | nodepool, reason, consolidation_type | Validation delay plus refreshed-candidate and scheduling validation. |
| action_initiation_delay_seconds | Histogram | nodepool, reason, consolidation_type, termination_mode, stage | Time from selection, expiry, or repair eligibility to successful disruption action. |
| action_to_node_deletion_duration_seconds | Histogram | nodepool, termination_mode | NodeClaim deletion timestamp to node-finalizer removal. |

#### Operator view: backlog and blockers

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| eligible_candidates | Gauge | nodepool, reason, consolidation_type | Candidates passing the latest method filter. |
| candidates_total | Counter | nodepool, reason, consolidation_type, outcome, blocker, pricing_status | Candidate outcomes: selected, infeasible, skipped, rejected, timed out, or error. |
| candidates_remaining | Gauge | nodepool, reason, consolidation_type, blocker | Candidates that become actionable when the named blocker changes. |
| oldest_backlog_age_seconds | Gauge | nodepool, reason, consolidation_type, blocker | Oldest blocked work, measured from its relevant condition, expiry, or unhealthy transition. |
| timeout_candidates_total | Counter | nodepool, consolidation_type, state | Candidates evaluated or remaining at a consolidation timeout. |
| expiration_candidates | Gauge | nodepool, state | Managed NodeClaims in configured, due, or deleting state. |
| expiration_lateness_seconds | Histogram | nodepool, termination_mode | Expiry to successful deletion action. |
| node_repair_candidates | Gauge | nodepool, state | Unhealthy managed nodes in within_toleration, eligible, blocked, or deleting state. |
| node_repair_blocked_total | Counter | nodepool, blocker | Deferrals caused by NodePool or cluster health gates. |
| node_repair_unhealthy_duration_seconds | Histogram | nodepool, condition | Unhealthy-condition transition to deletion action. |

#### Operator view: consolidation impact

pricing_status is a diagnostic label: available, unavailable, or not_applicable. Unavailable pricing does not block evaluation, but Karpenter must not calculate savings from it.

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| consolidation_observed_savings_usd_per_hour | Histogram | nodepool, consolidation_type, decision, disposition, pricing_status | Positive savings from a feasible command. disposition is selected, score rejected, validation rejected, or queue failed. |
| consolidation_known_savings_backlog_usd_per_hour | Gauge | nodepool, consolidation_type, blocker, pricing_status | Feasible positive-saving commands blocked in the latest pass. It answers what known saving is blocked now, not historical or total potential savings. |
| consolidation_source_cost_upper_bound_usd_per_hour | Gauge | nodepool, consolidation_type, blocker, pricing_status | Source cost of pre-simulation-blocked candidates; an upper bound, never a savings estimate. |

The known-savings gauge is a latest-pass snapshot. It is set from the completed pass and reset to zero when the next pass finds none; it does not retain candidate identity.

### How It Works

Candidate construction and disruption methods record outcomes where they are known. The SimulateScheduling wrapper records duration, result, and batch size without extra work. Validation records its full duration. The initiator records action delay; the termination path records action-to-node-deletion duration.

| Path | Primary signals |
|---|---|
| Empty | Budget/validation backlog and exact deletion savings; no simulation. |
| Static drift | Backlog, budget, replica excess, node limits, and action delay. |
| Drift | Backlog age, simulations to first feasible action, and budget blocking. |
| Multi/single consolidation | Simulation throughput, timeout coverage, blockers, and known savings. |
| Expiration | Due backlog, expiry lateness, action delay, and deletion duration. |
| Node repair | Unhealthy backlog, health-gate blocks, action delay, and deletion duration. |

#### metrics.disruption controller

A singleton metrics.disruption controller computes forceful-path gauges: expiration_candidates, node_repair_candidates, and forceful oldest_backlog_age_seconds. Every five seconds it derives the complete metric set and calls metrics.Store.ReplaceAll, following the existing metrics.node pattern and clearing stale series.

It reads cluster state plus cached managed NodeClaims. Both are required because expiration can act on a NodeClaim before it has a registered Node. It does not simulate, call pricing APIs, mutate resources, or make disruption decisions. Voluntary latest-pass gauges remain owned by the disruption controller because only it knows simulation outcomes.

### Interaction with Existing Features

Existing metrics remain the baseline and are not renamed or relabelled: decision evaluation duration, eligible nodes, budgets, timeouts, validation failures, queue failures, balanced-consolidation metrics, decisions_by_nodepool_total, disruption initiation, and node/NodeClaim termination metrics.

Budgets remain authoritative through existing per-NodePool budget gauges. Events retain node-specific detail; these metrics provide the aggregate entry point.

### Observability

The dashboard has four views: progress, backlog and age, blockers, and impact/execution.

Start voluntary diagnosis with time() - last_evaluated_timestamp_seconds: a later method may be stale because an earlier method already succeeded. Then inspect eligible_candidates, candidates_remaining, and oldest_backlog_age_seconds. Slice candidate outcomes by blocker before using events or logs for resource-level detail.

For cluster totals, aggregate away nodepool; retain it to diagnose a pool. Histogram quantiles must aggregate buckets first:

    histogram_quantile(0.95,
      sum by (le, reason, consolidation_type) (
        rate(karpenter_voluntary_disruption_candidate_evaluation_duration_seconds_bucket[15m])
      )
    )

Do not sum <multiple> with per-NodePool series for a pool total: it is one shared operation.

### Edge Cases

- Early success does not classify later, unexamined candidates as infeasible; freshness shows that lower-priority methods may be stale.
- Partial empty-node validation records only dropped candidates as validation blocked.
- pricing_status=unavailable appears on candidate metrics; savings metrics only observe finite positive available prices.
- Counters describe activity. Gauges are current snapshots and are reset by a later pass or metrics.disruption scan.

## Backward Compatibility

No API, behavior, or existing metric changes. New dashboards must tolerate absent new series during rollout.

## Graduation Criteria

All metrics introduced by this RFC launch as alpha and graduate across Karpenter
release versions. Graduation is based on metric stability and user experience,
not on a feature gate.

### Alpha

- Ship the metrics as alpha in the first Karpenter release containing them.
- Metric names, labels, and semantics may change while operators validate them.
- Unit and integration tests cover label vocabulary, gauge cleanup, pricing,
  blockers, forceful backlog, and lifecycle timing.
- Benchmarks show no material controller regression.

### Beta

- Graduate in a later Karpenter release after at least one release as alpha.
- No unresolved user-reported correctness, usability, cardinality, or performance
  issues.
- Dashboards demonstrate that operators can distinguish budget, feasibility,
  validation, timeout, and repair-gate backlogs.
- Names, labels, and semantics are expected to remain stable.

### GA

- Graduate in a later Karpenter release after at least one release as beta.
- No unresolved user-reported issues with the beta metrics.
- Maintainers have operational confidence in metric accuracy, cost, and stale
  series cleanup at supported cluster scales.
- Names, labels, and semantics follow Karpenter's stable metric compatibility
  guarantees.
