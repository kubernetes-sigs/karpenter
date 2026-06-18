# System Workload Classification for DaemonSet-like Pods

> Draft RFC for [kubernetes-sigs/karpenter#2352](https://github.com/kubernetes-sigs/karpenter/issues/2352). The final name for this concept is intentionally unresolved; this draft uses `SystemWorkload` as a placeholder.

## Summary

Karpenter currently treats native DaemonSet pods as node-associated overhead rather than normal workload demand. This proposal introduces `SystemWorkload` as the common model for describing additional pods that should receive similar treatment, and starts with a feature-gated, cluster-scoped CRD as the access mechanism for users and providers to supply that information.

The first iteration is intentionally classification-focused. It does not introduce synthetic pod templates, proportional scaling models, or generic custom-controller template extraction. Karpenter will not infer pre-provisioning overhead for never-observed `SystemWorkload` pods in this version. Optional predictive models such as `scalingModel` for linear or ladder-scaled system workloads are left to a second phase.

## Motivation

Karpenter treats DaemonSet pods differently from normal workload pods in several paths:

1. Pending DaemonSet pods are not treated as provisionable workload demand.
2. Bound DaemonSet pods are not treated as reschedulable workload pods during disruption simulations.
3. Observed DaemonSet pod requests and limits are tracked separately as daemon overhead on `StateNode`s.
4. During provisioning, Karpenter lists DaemonSets and passes representative DaemonSet pods to the scheduler so daemon overhead is considered when choosing new capacity.

This model works for native Kubernetes DaemonSets, but both providers and end users may need to describe daemon-like or node-associated infrastructure that is not owned by a native DaemonSet:

- Providers or managed Kubernetes distributions may run node-associated infrastructure as another workload type. For example, GKE runs `kube-system/konnectivity-agent` as a Deployment/ReplicaSet, but it can behave like provider infrastructure from the cluster operator perspective.
- End users may run vCluster, which can sync DaemonSet pods into a host cluster without preserving a DaemonSet owner reference.
- End users may run projects such as OpenKruise that provide enhanced DaemonSet-like controllers.
- Platform teams may run custom controllers that create one pod per node or otherwise create pods that should not be treated as independent application demand.

When these pods are treated as normal workloads, Karpenter can make incorrect decisions. A provider-side workaround that only ignores such pods for empty-node evaluation is insufficient: after a node is deleted, the controller may create a replacement pending pod, and Karpenter may provision a new node only for that infrastructure pod. This can create delete/provision churn.

The central requirement is consistency. Karpenter should classify selected daemon-like/system pods the same way across provisioning, disruption, and cluster-state accounting.

## Goals

1. Provide a Karpenter-native mechanism to classify selected non-DaemonSet pods as system/node-associated workloads.
2. Ensure matching pending pods are not treated as normal provisionable workload demand.
3. Ensure matching bound pods are not treated as normal reschedulable workload pods during disruption simulations.
4. Track observed resources for matching bound pods separately from normal workload pod resources, similar to existing DaemonSet accounting.
5. Allow providers and end users to register system workloads declaratively, for example through provider Helm defaults, provider reconciliation, or user-created Karpenter CRs.
6. Keep the first iteration small by focusing on classification and observed overhead, while leaving synthetic pod-template overhead modeling to a future design.

## Non-Goals

1. Full DaemonSet parity in the first iteration.
2. Modeling resource overhead for a system workload before any matching pod has ever existed, unless Karpenter can derive that information from an existing built-in source such as a native DaemonSet. Optional phase-two models may cover proportional workloads such as node/core-linear or ladder-scaled provider agents.
3. Generic extraction of pod templates from arbitrary custom controller schemas.
4. Provider-specific Go hooks in the initial API. Provider extension mechanisms may be considered later if declarative CRs are insufficient.
5. Replacing native DaemonSet handling.

## Proposal

Introduce a feature-gated, cluster-scoped `SystemWorkload` CRD that classifies pods as system/node-associated workloads rather than normal application workload demand.

A `SystemWorkload` is the model Karpenter uses to classify additional daemon-like pods. The CRD is the first proposed access mechanism for supplying that model because it can be used by both providers and end users. A future provider Go hook could supply the same model directly if maintainers prefer a lower-friction provider integration.

A `SystemWorkload` tells Karpenter that matching pods should be treated similarly to DaemonSet pods for scheduling and disruption classification. The initial API intentionally does not include a pod template. Karpenter will only use resources from observed matching pods.

The API supports label-based matching, annotation-based matching, and owner-reference matching. Owner-reference matching is important because many daemon-like controllers are controller identity problems rather than label problems: users may not control labels applied by upstream charts or synced pods. Owner matching should follow controller owner chains where Karpenter can resolve the intermediate owners, so stable higher-level owners such as Deployments can be matched even when pod direct owners are rollout-specific ReplicaSets.

### API Design

Label-selector example:

```yaml
apiVersion: karpenter.sh/v1alpha1
kind: SystemWorkload
metadata:
  name: gke-konnectivity-agent
spec:
  namespaceSelector:
    matchLabels:
      kubernetes.io/metadata.name: kube-system
  podSelector:
    matchLabels:
      k8s-app: konnectivity-agent
```

Owner-reference example:

```yaml
apiVersion: karpenter.sh/v1alpha1
kind: SystemWorkload
metadata:
  name: openkruise-advanced-daemonset
spec:
  ownerReference:
    apiVersion: apps.kruise.io/v1alpha1
    kind: AdvancedDaemonSet
```

Deployment owner-chain example:

```yaml
apiVersion: karpenter.sh/v1alpha1
kind: SystemWorkload
metadata:
  name: gke-konnectivity-agent
spec:
  ownerReference:
    apiVersion: apps/v1
    kind: Deployment
    name: konnectivity-agent
    namespace: kube-system
```

More selective custom-controller example:

```yaml
apiVersion: karpenter.sh/v1alpha1
kind: SystemWorkload
metadata:
  name: kube-system-custom-agent
spec:
  ownerReference:
    apiVersion: platform.example.com/v1
    kind: NodeAgent
    name: custom-agent
    namespace: kube-system
```

Cluster Autoscaler annotation compatibility can be represented as an ordinary annotation-selector rule instead of a separate built-in classification path:

```yaml
apiVersion: karpenter.sh/v1alpha1
kind: SystemWorkload
metadata:
  name: cluster-autoscaler-daemonset-pod-annotation
spec:
  annotationSelector:
    matchExpressions:
    - key: cluster-autoscaler.kubernetes.io/daemonset-pod
      operator: Exists
```

Possible validation rules:

- At least one matching mode is required: `podSelector`, `annotationSelector`, or `ownerReference`.
- `podSelector` uses the standard `metav1.LabelSelector` shape.
- `annotationSelector` uses `metav1.LabelSelector`-like syntax over pod annotations.
- `namespaceSelector`, if set, uses the standard `metav1.LabelSelector` shape over Namespaces. A single namespace can be matched with `kubernetes.io/metadata.name`.
- Empty selectors that match every pod in every namespace should be rejected unless the API explicitly adds an escape hatch later.
- Invalid selector expressions should be rejected by CRD validation.
- `ownerReference.apiVersion` and `ownerReference.kind` are required when `ownerReference` is set; `name` and `namespace` are optional narrowers.
- Owner-reference matching follows controller owner references from the pod toward higher-level owners when Karpenter can resolve the intermediate objects. This avoids requiring unstable rollout-specific objects such as ReplicaSets in rules. If an intermediate owner cannot be read or resolved, matching falls back to the resolved portion of the chain and should surface a debug log or condition when this prevents a configured rule from matching.

### Classification Semantics

Karpenter should classify a pod as a system workload if any of the following are true:

1. The pod is owned by a native DaemonSet.
2. The pod matches a `SystemWorkload` rule by `namespaceSelector` + `podSelector` and/or `annotationSelector`.
3. The pod matches a `SystemWorkload` rule by owner reference, either directly or through the resolved controller owner chain.

This classification should be centralized so the same result is used by provisioning, disruption, and cluster state. The classifier is a pure OR across sources. If multiple sources match the same pod, the pod is still classified once; overlaps are operationally useful but not semantically conflicting.

Conceptually:

```go
func IsSystemWorkloadPod(pod *corev1.Pod, systemWorkloads []SystemWorkload) bool {
    return IsOwnedByDaemonSet(pod) ||
        MatchesSystemWorkloadSelector(pod, systemWorkloads) ||
        MatchesSystemWorkloadOwnerChain(pod, systemWorkloads)
}
```

Existing helpers such as `IsProvisionable()` and `IsReschedulable()` should not become provider-overridable. Instead, they should depend on the centralized system-workload classifier while preserving their existing non-DaemonSet checks.

```go
IsProvisionable(pod) =
    FailedToSchedule(pod) &&
    !IsScheduled(pod) &&
    !IsPreempting(pod) &&
    !IsSystemWorkloadPod(pod) &&
    !IsOwnedByNode(pod)
```

```go
IsReschedulable(pod) =
    (IsActive(pod) || (IsOwnedByStatefulSet(pod) && IsTerminating(pod))) &&
    !IsSystemWorkloadPod(pod) &&
    !IsOwnedByNode(pod)
```

This keeps lifecycle semantics in Karpenter core while making the classifier extensible through declarative rules.

### Implementation Architecture

The classifier cannot live only as a stateless helper in `pkg/utils/pod`, because it depends on `SystemWorkload` objects, namespace labels, owner-chain resolution, and the feature gate. Instead, Karpenter should introduce a small classification component that is constructed from shared informer/cache state and injected into the controllers that need it.

At minimum, the classifier should be used by:

- provisioning, when listing pending pods and building the scheduler input;
- disruption, when determining reschedulable pods for a candidate;
- PDB/reschedulability helpers that currently call `pod.IsReschedulable()`;
- cluster state, when accounting bound pod requests and limits.

`pod.IsProvisionable()` and `pod.IsReschedulable()` can keep the core pod-lifecycle checks, but the DaemonSet exclusion should move behind the injected classifier at call sites that need `SystemWorkload` awareness.

When a `SystemWorkload`, matched Namespace, or resolved owner object changes, Karpenter should invalidate affected classification state and reclassify existing pods. For cluster-state accounting, that means removing pods from their previous accounting bucket and adding them to the new one, rather than waiting for pod churn. Owner-chain traversal should be bounded to controller owners, use cached objects where possible, and document any RBAC or watch requirements for non-core owner types.

### Provisioning Behavior

For pending pods that match a `SystemWorkload`, Karpenter should not treat the pod as workload demand that triggers new capacity by itself. This prevents infrastructure pods from causing provision-only-for-me churn after an empty or underutilized node is disrupted.

In the initial version, `SystemWorkload` does not add synthetic overhead during provisioning. Karpenter continues to calculate native DaemonSet overhead from DaemonSet objects and cached observed DaemonSet pods. For `SystemWorkload` rules, overhead is observed from matching pods that already exist.

There are two observed cases:

1. Bound matching pods contribute to system workload accounting on their current node.
2. Pending matching pods do not trigger scale-out alone, but if a provisioning batch is already triggered by normal workload pods, Karpenter may include the matching pending system pods in the scheduling simulation so their actual resource requests, host ports, and scheduling constraints are considered when choosing new capacity.

This opportunistic pending-pod modeling uses the actual admitted pod spec and avoids introducing templates in the first iteration. It still does not provide full DaemonSet parity: Karpenter cannot model a system workload that has not produced a pod yet, and it cannot infer per-node or cluster-proportional overhead for future NodeClaims without a template, controller-specific discovery, or provider-supplied model.

A second phase may add optional predictive inputs, such as a `scalingModel` for node/core-linear workloads like kube-dns or ladder-scaled provider agents like konnectivity-agent. Those models should feed the same classifier and scheduling paths, but they are intentionally outside this classification-first API so the initial proposal does not require Karpenter to standardize arbitrary autoscaler behavior.

### Disruption Behavior

Bound pods matching a `SystemWorkload` should be excluded from the set of normal reschedulable workload pods in disruption simulations, consistent with native DaemonSet pods.

This affects consolidation and other disruption paths that currently use `IsReschedulable()` or derived helpers. The goal is that a node running only matching system workload pods can be considered empty from the perspective of workload rescheduling, subject to existing disruption controls, PDB behavior, do-not-disrupt annotations, and drain semantics.

This proposal does not change drainability or eviction semantics. Native DaemonSet pods are still present on nodes and can still participate in drain/PDB/do-not-disrupt behavior where Karpenter already considers them. `SystemWorkload` pods should follow the same principle: they are excluded from workload rescheduling demand, not made invisible to node termination mechanics.

### Cluster State and Observed Overhead

When Karpenter observes a bound pod matching a `SystemWorkload`, it should account that pod's requests and limits in system workload accounting rather than normal workload pod accounting. This mirrors the behavior currently implemented with `StateNode.daemonSetRequests` and `StateNode.daemonSetLimits`.

The implementation can start by feeding these pods into the same internal accounting path as DaemonSet pods, but user-facing metrics and documentation should avoid implying that all counted pods are native DaemonSet pods. The preferred long-term naming is a generalized "system workload" or "node overhead workload" bucket, with DaemonSets treated as one source of that bucket.

When `SystemWorkload` objects are created, updated, or deleted, Karpenter should reclassify already-bound pods. Otherwise pods previously accounted as normal workload pods may remain in the wrong accounting bucket until pod churn.

### Provider and User Integration

`SystemWorkload` is the information Karpenter needs; the CRD is the initial mechanism for providing it. Both providers and end users can use this mechanism: providers can supply rules for managed infrastructure such as connectivity agents, while users can supply rules for vCluster, OpenKruise, or custom node-agent controllers.

Provider-created CRs, whether installed by Helm or reconciled by a provider controller, are a pragmatic first step rather than the only possible provider integration. They avoid adding both a new CRD and a provider hook in the first iteration. If reviewers prefer a first-class provider integration immediately, this design can include an optional provider hook that returns `SystemWorkload`-like specs into the same classifier. The design question is about access to the model (CRD, provider hook, or both), not about creating separate classification semantics.

### Feature Gate

The new behavior should be guarded by a feature gate, tentatively named `SystemWorkloads`.

When disabled:

- Karpenter preserves current DaemonSet behavior.
- `SystemWorkload` objects, if present, are ignored.
- Pending pods matching `SystemWorkload` rules are treated according to current logic.

When enabled:

- Karpenter watches and evaluates `SystemWorkload` objects.
- Matching pods are excluded from provisionable and reschedulable workload sets.
- Matching bound pods contribute to observed system/daemon overhead accounting.

All non-DaemonSet classification, including annotation-based rules configured through `SystemWorkload`, remains behind this single rollout switch.

## Observability

`SystemWorkload` should expose status to help users understand whether rules are accepted. High-churn pod match counts and pod identities should generally be metrics or logs rather than status fields to avoid writing status on every pod reschedule.

Possible status fields:

```yaml
status:
  conditions:
  - type: Ready
    status: "True"
    reason: SelectorAccepted
    observedGeneration: 1
  - type: SelectorTooBroad
    status: "False"
    observedGeneration: 1
```

Possible metrics/logging:

- `karpenter_system_workload_matched_pods{name="..."}`
- `karpenter_system_workload_skipped_pending_pods{name="..."}` for pending pods that did not trigger provisioning because they matched a rule
- classifier debug logs or events such as `pod skipped due to SystemWorkload rule <name>`
- warnings when a rule matches a large fraction of cluster pods or overlaps with native DaemonSet-owned pods

Docs should explicitly warn that matching normal application Deployments means those pods may not trigger scale-out. At minimum, Karpenter should make the classification reason discoverable during debugging without storing high-cardinality pod lists in CR status.

## API Lifecycle and Graduation

`SystemWorkload` should start as `karpenter.sh/v1alpha1` behind a default-off feature gate. Before beta graduation, the project should resolve:

1. final naming for the concept and API kind;
2. whether selector-only + owner-reference classification is sufficient, or whether pre-provisioning overhead requires templates/template references or optional scaling models;
3. the public metric names for system workload accounting;
4. provider default ownership and opt-out expectations;
5. the watch, cache, and RBAC expectations for owner-chain traversal, especially for non-built-in controller types.

## Alternatives Considered

### Built-in support only for `cluster-autoscaler.kubernetes.io/daemonset-pod`

This is the smallest compatibility change, but it is not expressive enough for provider-specific or cluster-specific infrastructure. It also does not provide an auditable Karpenter-native configuration surface.

The annotation may still be useful as a compatibility pattern, but it can be represented through a `SystemWorkload` annotation selector rather than as a separate hard-coded classification source.

### Provider Go Interface

An optional provider interface could let cloud providers return `SystemWorkload` specs directly to Karpenter core.

Pros:

- Good for provider-specific logic.
- Allows dynamic provider behavior.
- Gives providers a convenient integration point without requiring them to manage CR lifecycle.

Cons:

- Hidden from users unless providers add separate observability.
- Creates provider-specific behavior that may be harder to reason about.
- Risks multiple sources of truth unless the hook feeds the same internal model as CR-backed rules.

This draft starts with the CRD because it works for both providers and end users. The scope could be expanded to include a provider hook in the initial implementation if maintainers want an immediate provider-facing integration. In that case, the hook should return `SystemWorkload`-like specs into the same classifier rather than introducing separate semantics.

### Generic Duck-Typed Controller Discovery

Karpenter could accept rules describing custom controller GVKs and paths to selectors/templates, for example OpenKruise `AdvancedDaemonSet`.

Pros:

- Powerful.
- Can support many custom controllers without provider-specific code.

Cons:

- Requires dynamic watches and RBAC.
- Requires path extraction and schema/error handling.
- Custom controllers may not have true DaemonSet semantics even if they expose a pod template.
- More difficult to validate and debug.

This is a promising future direction but is intentionally deferred.

### Pod Template in `SystemWorkload`

`SystemWorkload` could include a pod template so Karpenter can model future overhead before pods exist.

Pros:

- Solves the provisioning overhead problem more completely.
- Makes non-DaemonSet system workloads closer to native DaemonSet behavior.

Cons:

- Increases API complexity significantly.
- Risks users copying stale or incomplete pod specs.
- Raises questions about admission/defaulting, LimitRanges, drift from real controllers, ownership, and conflicts.

This proposal avoids templates in the first iteration and focuses on classification plus observed overhead.

## Backward Compatibility

When the feature gate is disabled, Karpenter preserves current behavior.

When the feature gate is enabled, existing native DaemonSet behavior remains unchanged. The new behavior applies only to pods matched by `SystemWorkload` rules.

The primary compatibility risk is operator misconfiguration: an overly broad rule can cause Karpenter to ignore application pods as provisionable or reschedulable workload demand, which can starve normal workloads of scale-out capacity. The API should mitigate this through validation, status conditions for suspiciously broad rules, metrics for skipped pending pods, logs/events that name the matching rule, documentation, and a default-off feature gate.

## Rollout Plan

1. Add the `SystemWorkloads` feature gate, default off.
2. Add the `SystemWorkload` `v1alpha1` CRD.
3. Implement centralized classification for native DaemonSets and `SystemWorkload` rules.
4. Wire the classifier into provisioning, disruption, PDB/reschedulability helpers, and cluster-state accounting.
5. Add metrics/logs/status conditions for rule acceptance and pod classification.
6. Document provider guidance for supplying default `SystemWorkload` rules.
7. Gather user/provider feedback before considering templates, template references, optional scaling models for proportional system workloads, or duck-typed controller discovery. A provider Go hook may be included earlier if maintainers want a first-class provider integration in addition to CR-backed rules.

## Testing Strategy

Unit tests should cover:

- pending matching pods do not trigger provisioning;
- bound matching pods are not reschedulable workload pods;
- owner-reference matching works for configured GVKs, including controller owner chains such as Pod -> ReplicaSet -> Deployment;
- unresolved owner-chain intermediates fail safely and produce useful diagnostics;
- label + namespace selector matching works with Kubernetes-style selectors;
- observed matching pods contribute to system/daemon resource accounting;
- broad/invalid selectors are rejected;
- feature gate disabled preserves current behavior;
- reclassification happens when `SystemWorkload` objects or matched Namespaces are created, updated, or deleted;
- pending matching system pods do not trigger provisioning alone but can be modeled when normal workload pods trigger a provisioning batch.

Integration or simulation tests should cover:

- a node that only runs a matching system workload pod can be considered empty from workload-rescheduling perspective;
- a matching pending system workload pod does not cause Karpenter to launch a node by itself;
- a provider-installed default rule can be overridden or disabled by the user.

## Future Enhancements

Future versions may add:

1. explicit pod templates or template references for pre-provisioning overhead modeling;
2. optional `scalingModel` inputs for cluster-proportional system workloads, including node/core-linear and ladder-based models;
3. duck-typed discovery of custom controller resources that expose selectors and pod templates;
4. a provider extension interface that returns `SystemWorkload`-like specs directly into the same classifier, if not included in the initial implementation;
5. richer status or metrics once real-world debugging needs are understood.

## Limitations

1. `SystemWorkload` selectors and owner-reference rules alone cannot model overhead for pods that do not exist yet.
2. If a system workload has never been observed on a node, Karpenter cannot infer its resource requests from this CRD.
3. Classification-only rules do not model cluster-proportional scaling behavior, such as node/core-linear kube-dns replicas or ladder-scaled konnectivity-agent replicas. Those require optional phase-two scaling models or provider-supplied predictive inputs.
4. Incorrect rules can cause application pods to be ignored by provisioning or disruption simulations. Validation, metrics, logs, and status conditions should help users detect broad or unused rules.
5. Existing metrics and internal names that reference DaemonSets may become less precise if reused for generalized system workload accounting.
6. Owner-reference traversal depends on Karpenter being able to resolve intermediate owner objects. Rules may fail to match through an owner chain if an intermediate owner type is not watched, cached, or readable under Karpenter's RBAC.

## Open Questions

1. Naming: should the concept be called `SystemWorkload`, `NodeAssociatedWorkload`, `InfrastructureWorkload`, `DaemonWorkload`, or something else?
2. Should matching pods be accounted under existing daemon metrics, or should metrics be renamed/generalized?
3. Should a future version add explicit pod templates, template references, duck-typed controller discovery, or optional `scalingModel` inputs for pre-provisioning overhead modeling?
4. Should providers eventually have an optional extension interface to supply system workload selectors or predictive scaling models directly to Karpenter core, or should providers continue to create `SystemWorkload` CRs?
5. How should Karpenter protect users from overly broad selectors that accidentally classify application pods as system workloads?
6. How broad should owner-chain traversal be, and which owner types should Karpenter watch or resolve by default?
7. Which proportional scaling shapes, if any, should be standardized in a phase-two API, such as node/core-linear and ladder-based replica models?

## References

- [kubernetes-sigs/karpenter#2352](https://github.com/kubernetes-sigs/karpenter/issues/2352)
- [Cluster Autoscaler daemonset pod annotation](https://github.com/kubernetes/autoscaler/blob/563f074dd1244de9cb8c3c7997a095775244c985/cluster-autoscaler/utils/pod/pod.go#L31-L42)
- [OpenKruise AdvancedDaemonSet](https://openkruise.io/docs/user-manuals/advanceddaemonset)
