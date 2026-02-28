# AZ-Sequential Disruption Budget for Drift

## Motivation

Karpenter's existing disruption budgets control how many nodes can be terminating simultaneously
across a NodePool, but they have no awareness of topology. When a NodePool spans multiple
availability zones (or any topology domain), all zones can be disrupted concurrently. This is
acceptable for consolidation, but it creates availability risk for Drift — when a new node image
or configuration rolls out, every zone can begin cycling nodes at the same time.

Operators running stateful or zone-sensitive workloads need a way to roll drifted nodes
**one zone at a time**, verifying that the first zone stabilises before the next one begins.
This mirrors how rolling deployments work but applied at the node level and scoped to topology
domains.

Related issues:
- https://github.com/kubernetes-sigs/karpenter/issues/924
- https://github.com/kubernetes-sigs/karpenter/issues/753

## API Design

Two new optional fields are added to the existing `Budget` struct:

```go
// Budget defines when Karpenter will restrict the
// number of Node Claims that can be terminating simultaneously.
type Budget struct {
    // Reasons is a list of disruption methods this budget applies to.
    // +optional
    Reasons []DisruptionReason `json:"reasons,omitempty"`

    // Nodes dictates the maximum number of NodeClaims that can be terminating at once.
    // +kubebuilder:default:="10%"
    Nodes string `json:"nodes"`

    // Schedule specifies when a budget begins being active (cron syntax).
    // +optional
    Schedule *string `json:"schedule,omitempty"`

    // Duration determines how long a Budget is active since each Schedule hit.
    // +optional
    Duration *metav1.Duration `json:"duration,omitempty"`

    // TopologyKey, when set, scopes this budget to individual topology domains.
    // The Nodes limit is applied per distinct value of this label key.
    // For example, "topology.kubernetes.io/zone" enforces the limit per AZ.
    // +optional
    TopologyKey string `json:"topologyKey,omitempty"`

    // Sequential, when true with TopologyKey, ensures only one topology domain
    // is disrupted at a time. Disruptions in one zone must complete before the
    // next zone begins. Requires TopologyKey to be set.
    // +optional
    Sequential bool `json:"sequential,omitempty"`
}
```

These fields are additive and fully backwards compatible. Budgets without `TopologyKey` or
`Sequential` behave exactly as before.

## Validation

- `Sequential` requires `TopologyKey` to be non-empty. This is enforced at both the CEL
  rule level on the CRD and at runtime validation:
  ```
  // +kubebuilder:validation:XValidation:message="'sequential' requires 'topologyKey'",
  //   rule="self.all(x, !has(x.sequential) || !x.sequential || (has(x.topologyKey) && x.topologyKey != ''))"
  ```
- `TopologyKey` must be a valid qualified Kubernetes label name (validated at runtime).
- `Schedule`/`Duration` constraints remain unchanged (both must be set together or both omitted).

## Algorithm

Sequential disruption is only applied to the `Drifted` disruption reason. Consolidation and
expiration continue to use the existing flat budget model.

### Zone Selection

1. Candidates arriving at `ComputeCommands` are pre-sorted oldest-drift-first (by
   `ConditionTypeDrifted.LastTransitionTime`).
2. `filterBySequentialTopology` groups candidates by NodePool and, for each NodePool with an
   active sequential budget, determines the **active zone**:
   - If any zone already has nodes `MarkedForDeletion` (in-flight disruptions), that zone
     is the active zone. In-progress work is always continued before starting a new zone.
   - If no zone has in-flight disruptions, the zone of the first (oldest-drifted) candidate
     is chosen as the active zone.
3. The budget's `Nodes` allowance is evaluated **per zone** (using the zone's node count as
   the base for percentage calculations), minus the number of nodes already in-flight in
   that zone.
4. Only candidates from the active zone, capped at the remaining per-zone allowance, are
   returned. Candidates from all other zones are excluded for this cycle.
5. Terminating nodes (`ConditionTypeInstanceTerminating`) are excluded from zone counts to
   avoid double-counting nodes that have already left.

### Interaction with NodePool-Level Budgets

Topology-scoped budgets (`TopologyKey != ""`) are **excluded** from the NodePool-level
`GetAllowedDisruptionsByReason` calculation. This prevents them from artificially constraining
the global counter — the per-zone budget takes effect inside the Drift disruption method only.
Non-topology budgets continue to constrain the total disrupting node count across the NodePool.

## User Stories

### Rolling drift across AZs one at a time

A cluster spans `us-west-2a`, `us-west-2b`, and `us-west-2c`. After a node image update,
the operator wants to roll each zone sequentially with no more than one node disrupting at
a time per zone:

```yaml
apiVersion: karpenter.sh/v1
kind: NodePool
metadata:
  name: default
spec:
  disruption:
    budgets:
      - nodes: "1"
        topologyKey: topology.kubernetes.io/zone
        sequential: true
        reasons: [Drifted]
```

Karpenter will disrupt nodes in `us-west-2a` (oldest drift) one at a time. Once all drifted
nodes in `us-west-2a` have been replaced and no nodes in that zone are `MarkedForDeletion`,
it moves to `us-west-2b`, and then `us-west-2c`.

### Combining sequential drift with a global budget

Limit drift to business hours and roll one zone at a time during that window, while keeping
a global safety cap:

```yaml
apiVersion: karpenter.sh/v1
kind: NodePool
metadata:
  name: default
spec:
  disruption:
    budgets:
      # Only allow drift during business hours
      - nodes: "0"
        schedule: "0 17 * * mon-fri"
        duration: 16h
        reasons: [Drifted]
      # During the active window, roll one zone at a time
      - nodes: "1"
        topologyKey: topology.kubernetes.io/zone
        sequential: true
        reasons: [Drifted]
      # Consolidation can still proceed independently
      - nodes: "10%"
```

### Percentage-based per-zone budget

Allow up to 25% of nodes in the active zone to disrupt simultaneously, while still rolling
zones sequentially:

```yaml
apiVersion: karpenter.sh/v1
kind: NodePool
metadata:
  name: default
spec:
  disruption:
    budgets:
      - nodes: "25%"
        topologyKey: topology.kubernetes.io/zone
        sequential: true
        reasons: [Drifted]
```

## Limitations

- **Drift only.** Sequential topology budgets are evaluated exclusively within the Drift
  disruption path. Consolidation and expiration continue to use the flat NodePool-level budget
  model.
- **One sequential budget per NodePool.** Only the first active sequential budget for a given
  NodePool is used. If multiple sequential budgets are defined, the first active one wins.
- **Single topology key.** A budget can only specify one `TopologyKey`. Cross-topology
  ordering (e.g., zone then rack) is not supported.
- **Zone ordering is not deterministic across restarts.** The active zone is derived from
  in-flight disruptions (resuming existing work) or the oldest-drifted candidate. There is no
  persistent ordering record; if the controller restarts mid-roll with no in-flight nodes, the
  next zone chosen will be whichever has the oldest drift timestamp.
