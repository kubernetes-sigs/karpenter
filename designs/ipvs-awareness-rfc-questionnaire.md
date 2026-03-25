# Karpenter RFC Questionnaire — In-Place Pod Vertical Scaling Awareness

## SECTION 1: Title & Metadata

- **RFC Title:** In-Place Pod Vertical Scaling (IPVS) Awareness for Karpenter
- **Authors:** jonesflp
- **Status:** Draft
- **Created Date:** 2026-03-24
- **Last Updated:** 2026-03-24
- **Reviewers:** Karpenter maintainers, SIG-Autoscaling
- **Tracking Issue/PR:** TBD

## SECTION 2: Summary

- **One-liner:** This RFC proposes adding IPVS awareness to Karpenter so it makes correct capacity decisions when pod resource requests are mutable.
- **What changes:** Karpenter will track effective pod resources as max(spec.requests, allocatedResources, peak annotations), protect nodes with actively resizing pods from consolidation, use peak resource envelopes for provisioning, and optionally defer provisioning for pods expected to scale down.
- **Who benefits:** Cluster operators running workloads that use Kubernetes InPlacePodVerticalScaling (alpha since 1.27).

## SECTION 3: Motivation

- **Problem statement:** Karpenter assumes pod resource requests are immutable. When pods use IPVS to reduce requests at steady state, Karpenter sees underutilization, evicts pods, new pods start with high startup requests requiring larger nodes, then scale down again — creating a destructive consolidation loop.
- **Who is affected:** Cluster operators using VPA in in-place mode, workloads with startup-heavy resource profiles, any team adopting IPVS.
- **Impact:** Without this change, IPVS adoption with Karpenter causes continuous pod churn, wasted compute, and degraded workload stability. Operators must disable consolidation entirely to work around it.
- **User stories:**
  - As a cluster operator, I want Karpenter to correctly track pod resource usage when pods resize in place, so node utilization reflects actual consumption.
  - As a workload owner, I want to annotate my pods with peak resource requirements so Karpenter provisions nodes large enough for the full pod lifecycle.
  - As a cluster operator, I want Karpenter to avoid consolidating nodes during active pod resizes so workloads aren't disrupted.

## SECTION 4: Goals and Non-Goals

- **Goals:**
  1. Track effective pod resources as max(spec.requests, allocatedResources, peak annotations) when IPVS gate is enabled
  2. Exclude nodes with actively resizing pods from consolidation candidacy
  3. Introduce a configurable grace period after resize completion before reconsidering consolidation
  4. Use peak resource envelopes for instance type selection during provisioning
  5. Support steady-state annotations to defer provisioning for pods expected to scale down
  6. Emit metrics, events, and debug logs for all IPVS-related decisions
- **Non-Goals:**
  1. Automatically detecting peak resource requirements (users must annotate pods)
  2. Modifying pod resource requests (Karpenter only reads, never writes pod specs)
  3. Integrating with VPA controllers directly (annotations are the interface)
  4. Supporting IPVS for init containers or ephemeral containers
- **Future Goals:**
  1. Namespace-level default peak annotations
  2. Automatic peak detection from historical metrics
  3. Integration with Kubernetes VPA recommendation API

## SECTION 5: Proposal / Design

- **High-level approach:** Add a new `InPlacePodVerticalScaling` feature gate (default: false) that enables IPVS-aware resource computation across four subsystems: state tracking, consolidation protection, scheduling simulation, and provisioning patience.
- **Detailed design:** See design document. Key components:
  - `IPVSAwareRequestsForPod` function computes max(spec, allocated, peak) per resource type
  - `ShouldDisrupt` checks for active resize and grace period before consolidation
  - Scheduling simulator uses peak envelope for pod fit evaluation
  - Provisioner defers pods with steady-state annotations during patience window
  - Four new pod annotations: peak-cpu, peak-memory, steady-state-cpu, steady-state-memory
  - One new NodePool field: `ipvsConsolidationGracePeriod` (default 5m)
  - One new CLI flag: `--ipvs-patience-duration` (default 60s)
- **Configuration:**
  - Feature gate: `--feature-gates=InPlacePodVerticalScaling=true`
  - Grace period: `nodePool.spec.disruption.ipvsConsolidationGracePeriod: 5m`
  - Patience duration: `--ipvs-patience-duration=60s`
- **Examples:** Pod with peak annotations:
  ```yaml
  metadata:
    annotations:
      karpenter.sh/peak-cpu: "2"
      karpenter.sh/peak-memory: "4Gi"
  spec:
    containers:
    - resources:
        requests:
          cpu: 500m
          memory: 1Gi
  ```

## SECTION 6: Alternatives Considered

- **Alternative 1: VPA integration via webhook** — Intercept VPA recommendations and adjust Karpenter's view. Rejected: too tightly coupled to VPA, doesn't handle non-VPA IPVS use cases.
- **Alternative 2: Always use allocatedResources** — Track only allocatedResources instead of spec.requests. Rejected: doesn't account for peak requirements or future scale-ups, only prevents underestimation during active resize.
- **Alternative 3: Disable consolidation for IPVS pods** — Simply skip consolidation for any pod with resize activity. Rejected: too coarse-grained, prevents legitimate consolidation after resize stabilizes.
- **Do nothing:** IPVS adoption with Karpenter causes a destructive consolidation loop. Operators must disable consolidation entirely.

## SECTION 7: Risks and Mitigations

- **Risk 1:** Peak annotations may be stale or incorrect, leading to over-provisioning. Mitigation: annotations are optional and operator-controlled; validation logs warnings for invalid values.
- **Risk 2:** Grace period may delay legitimate consolidation. Mitigation: configurable per NodePool, default 5m is conservative but tunable.
- **Risk 3:** Patience mechanism may delay provisioning for pods that won't actually scale down. Mitigation: patience has a short default (60s) and only applies to pods with explicit steady-state annotations.
- **Backward compatibility:** All behavior gated behind feature gate (default: false). When disabled, zero behavioral change.
- **Security implications:** None — annotations are read-only, no new RBAC requirements.
- **Performance implications:** Minimal — one additional max() computation per pod per reconciliation when gate is enabled.

## SECTION 8: Testing and Validation

- **Unit tests:** Feature gate parsing, annotation parsing (valid/invalid), resize status handling, grace period defaults/custom, steady-state validation
- **Property-based tests:** 11 properties validated with rapid library (100+ iterations each) covering resource computation, consolidation protection, scheduling simulation, provisioning, and metrics
- **Integration tests:** Consolidation with IPVS pods (active resize, grace period, expiry), provisioning with peak annotations, provisioning patience with steady-state annotations, feature gate disabled preserves behavior
- **Edge cases:** Invalid annotations, nil ContainerStatuses, multiple pods resizing on same node, steady-state exceeding spec, patience expiration
- **Metrics:** `karpenter_ipvs_resource_adjustment_total{resource_type}`, `karpenter_ipvs_consolidation_deferred_total{reason}`

## SECTION 9: Migration and Rollout Plan

- **Feature gating:** Behind `InPlacePodVerticalScaling` feature gate, default `false`
- **Rollout phases:** 1) Alpha: opt-in via feature gate (current) → 2) Beta: default-on after community feedback → 3) GA: mandatory, gate removed
- **Migration steps:** Enable feature gate, add peak/steady-state annotations to workloads as needed, configure grace period per NodePool if desired
- **Rollback plan:** Disable feature gate — all IPVS behavior immediately stops, Karpenter reverts to spec-only resource tracking
- **Documentation:** Feature gate reference, annotation reference, NodePool disruption config, troubleshooting guide for consolidation loop

## SECTION 10: Open Questions

1. Should peak annotations be set at the namespace level as defaults? [For SIG-Autoscaling]
2. Should the grace period be per-pod rather than per-node? (Current: per-node based on most recent resize) [For Karpenter maintainers]
3. Should we integrate with the VPA recommendation API directly instead of/in addition to annotations? [For SIG-Autoscaling]
4. What is the right default patience duration — 60s may be too short for slow-starting workloads? [For end users]
5. Should we support `Deferred` resize status as an active resize state? (Currently only `Proposed` and `InProgress` are treated as active) [For Karpenter maintainers]
