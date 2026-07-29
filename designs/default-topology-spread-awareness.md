# **Default Pod Topology Spread Awareness**

## Summary

Make Karpenter's scheduling simulation aware of cluster-level default `topologySpreadConstraints` configured on the `kube-scheduler`, so that Karpenter provisions capacity consistent with the spread the scheduler will actually enforce on pods that declare no constraints of their own.

## **Motivation**

Karpenter ships its own compiled-in scheduling simulation. When it decides which pods fit on which (real, in-flight, or hypothetical) nodes, it reads topology spread constraints *exclusively* from a pod's own `pod.Spec.TopologySpreadConstraints` (`pkg/controllers/provisioning/scheduling/topology.go`, `newForTopologies`). If a pod declares none, Karpenter treats it as having no spread requirement at all.

The `kube-scheduler`'s `PodTopologySpread` plugin does not behave that way. It lets a cluster operator configure `defaultConstraints` that apply to *any* pod that does not declare its own `topologySpreadConstraints`. On a cluster with such a default, the scheduler enforces spreading on an unconstrained pod while Karpenter — unaware the default exists — assumes the pod can go anywhere. The two disagree, and Karpenter provisions the wrong capacity: it packs the pod onto existing nodes in a domain the scheduler will refuse, and never launches a node in the domain the scheduler actually needs. With `whenUnsatisfiable: DoNotSchedule`, the result is a pod stuck `Pending` indefinitely with no recovery path, because the node that would satisfy the constraint never gets created.

### **Prior Art / Related Issues**

The gap and the demand for closing it are both established in upstream:

- [kubernetes-sigs/karpenter#1197](https://github.com/kubernetes-sigs/karpenter/issues/1197) ("how does Karpenter play with the default `PodTopologySpread` part of `KubeSchedulerConfiguration`?") asks exactly the three questions this RFC answers — does Karpenter have a built-in default, does it honor the scheduler's *internal* default constraints, and does it honor a *customized* cluster-level default. A user confirms Karpenter does **not** respect cluster-level spreading; the issue was closed only as stale, not resolved.
- [aws/karpenter-provider-aws#7062](https://github.com/aws/karpenter-provider-aws/issues/7062) ("Allow configuring `defaultConstraints` at the karpenter spec level") is a standing `feature` request for precisely this behavior — falling back to configured default constraints when a pod declares none — driven by the pain of having to add `topologySpreadConstraints` to every workload. Community triage left it as "needs more research to determine what this would look like"; this RFC is that design.
- [kubernetes-sigs/karpenter#74](https://github.com/kubernetes-sigs/karpenter/issues/74) ("Better Scheduling Default Behavior", `triage/accepted`) is the broader, accepted idea of a Karpenter-owned surface for default scheduling rules (a `SchedulingTemplate`). Default topology spread is one concrete slice of that space; this RFC deliberately scopes to the scheduler-mirroring slice rather than a general pod-defaulting webhook.

A note on the scheduler's own defaults surfaced in #1197: `kube-scheduler` ships *internal* default constraints (a `hostname`/`zone` `ScheduleAnyway` spread) that apply unless an operator overrides `defaultConstraints` or disables them. This RFC covers only the *operator-configured* `defaultConstraints`; whether Karpenter should also mirror the scheduler's built-in internal defaults is called out in [Open Questions](#open-questions).

### **Use Cases**

1. **Cluster-wide zone spread (`DoNotSchedule`).** A platform team sets a cluster-level default of `maxSkew: 1` across `topology.kubernetes.io/zone` with `whenUnsatisfiable: DoNotSchedule`, so that *every* workload is zone-balanced without each team having to declare it. A new Deployment ships with no `topologySpreadConstraints`. The scheduler enforces the default and the pods go `Pending` because the current nodes are all in one zone. Karpenter, unaware of the default, sees an unconstrained pod, tries to pack it onto existing capacity in the wrong zone, and never launches a node in the zone the scheduler needs — the pods are stuck with no recovery path.
2. **Debuggability of an invisible constraint.** Because a default constraint lives in scheduler config rather than the pod spec, a workload owner who hits the `Pending` failure in use case 1 sees a pod with no `topologySpreadConstraints` and no obvious reason it won't schedule. This is the same failure the real scheduler produces today for a defaulted pod, so Karpenter honoring the default at least makes its provisioning consistent with the scheduler; improving the failure *messaging* so the cluster default is attributable is called out as future work (see [Observability](#observability)).

### **Non-Goals**

- **Reading `kube-scheduler`'s live `KubeSchedulerConfiguration` file/ConfigMap directly.** Karpenter does not run in the control plane and, in managed offerings, cannot read the scheduler's on-disk config. This RFC delivers the configuration to Karpenter through its own config value; keeping the two in sync is the operator's (or, in an integrated offering, the platform's) responsibility. See [Alternatives](#alternatives-considered).
- **Other cluster-wide `kube-scheduler` configurations.** This RFC covers only the `PodTopologySpread` default constraints. Other scheduler settings that Karpenter could eventually mirror — such as the `NodeResourcesFit` scoring strategy (bin-packing vs spreading), or any cluster-level scheduler config in the same category — are out of scope here. The `SchedulerConfiguration` type is deliberately structured so such settings can be added as additional fields later without a new config surface; each would be scoped and designed on its own.
- **Per-pod scheduler config.** Only cluster-level default constraints are in scope. Per-pod `topologySpreadConstraints` are already honored.

## **Proposal**

Introduce a **structured scheduler configuration value**, supplied to the controller via a single `SCHEDULER_CONFIG` env var (with a matching `--scheduler-config` flag), that tells Karpenter the parts of the cluster's scheduler configuration it must mirror. For this RFC that is the `PodTopologySpread` default constraints; the value is deliberately structured so future scheduler-mirroring settings slot in as additional fields without a new flag or env var each time.

The value is a YAML/JSON document describing a Karpenter-owned config type, carried as the string value of the env var. It is delivered through the existing options mechanism rather than a mounted config file because it matches how Karpenter is configured today. All controller configuration flows through flags/env (`options.go`), parsed once at startup. Karpenter previously had a dynamically-read config object — the `karpenter-global-settings` ConfigMap — and **deliberately removed it** (deprecated at v1beta1, dropped in v0.33.0) in favor of env/flags. A new env var is consistent with that decision; a mounted ConfigMap/file would cut against it. See [Alternative 1](#alternative-1-load-the-config-from-a-file).

**This is a new encoding pattern for Karpenter, and a deliberate one.** Every Karpenter option today is a scalar (string/int/bool/duration) or a simple delimited string — the closest existing case is `--feature-gates`, which packs structure into a `key=value,key=value` **map string** parsed with `k8s.io/component-base/cli/flag`. No option is a YAML/JSON document. Topology spread constraints do not reduce cleanly to flat `key=value` pairs, so a serialized document is the natural encoding, and it extends the existing "structured-value-in-a-string-flag" precedent (`feature-gates`) from a map string to a full document.

Structuring the *value* (rather than adding one flat scalar per setting) preserves extensibility: the config grows by adding fields to the schema, not by adding new env vars. The type is Karpenter's *own* — it mirrors the *shape* of the relevant `kube-scheduler` fields so values are near-copy-paste, but it is not the upstream `KubeSchedulerConfiguration` schema, so Karpenter takes no dependency on that versioned API. See [Alternative 2](#alternative-2-accept-the-upstream-kubeschedulerconfiguration-schema-directly) for the verbatim-copy variant and why it is not the primary proposal.

When `SCHEDULER_CONFIG` is unset, behavior is exactly today's — the change is opt-in and backward-compatible.

### **Proposed Spec**

Controller flag / env var (mirroring `options.go`):

```
--scheduler-config string
    A YAML/JSON document configuring the parts of the cluster's kube-scheduler
    behavior that Karpenter must mirror during scheduling simulation. Empty means
    no scheduler-config overrides.
    (env SCHEDULER_CONFIG) (default "")<highlight t="yellow-2">
</highlight>
```

The value is a plain YAML block describing a Karpenter-owned config type — no `apiVersion` or `kind` header. For this RFC it carries a single section (shown as YAML; the env var holds this document as a string) :

```yaml
podTopologySpread:
  # Mirrors kube-scheduler's PodTopologySpread plugin `defaultConstraints`.
  # Applied during simulation to any pod that declares no
  # topologySpreadConstraints of its own.
  defaultConstraints:
    - maxSkew: 1
      topologyKey: topology.kubernetes.io/zone
      whenUnsatisfiable: ScheduleAnyway
    - maxSkew: 3
      topologyKey: kubernetes.io/hostname
      whenUnsatisfiable: ScheduleAnyway<highlight t="yellow-2">
</highlight>
```

The `defaultConstraints` field is intentionally the same shape as the scheduler's, so an operator lifts that fragment out of their `KubeSchedulerConfiguration` (`profiles[].pluginConfig[name: PodTopologySpread].args.defaultConstraints`) and pastes it under `podTopologySpread`.

**Extensibility (illustrative, not in scope for this RFC).** A future scheduler-mirroring setting — for example a `NodeResourcesFit` scoring strategy — would be added as a sibling field, requiring no new flag/env var and no change to how the config is transported:

```yaml
podTopologySpread:
  defaultConstraints: [ ... ]
nodeResourcesFit:            # <-- hypothetical future extension
  scoringStrategy:
    type: LeastAllocated<highlight t="yellow-2">
</highlight>
```

The Go type mirrors this structure:

```go
type SchedulerConfiguration struct {
    PodTopologySpread *PodTopologySpreadConfig `json:"podTopologySpread,omitempty"`
    // future: NodeResourcesFit *NodeResourcesFitConfig `json:"nodeResourcesFit,omitempty"`
}

type PodTopologySpreadConfig struct {
    DefaultConstraints []corev1.TopologySpreadConstraint `json:"defaultConstraints,omitempty"`
}

```

The value is decoded and validated once during operator startup (in `Options.Parse`, the same place `--feature-gates` is parsed from its string form), and the resulting typed value is carried via context so the scheduler can reach it — the same injection path as `preference-policy`.

### **How It Works**

Today, `Topology.newForTopologies` (`topology.go`) iterates `p.Spec.TopologySpreadConstraints` and builds a `TopologyGroup` (`topologygroup.go`) for each. If the pod declares none, it produces nothing and the pod is treated as unconstrained.

The change adds a defaulting step that mirrors the scheduler's `PodTopologySpread` plugin: when a pod declares no `topologySpreadConstraints` of its own, the configured `podTopologySpread.defaultConstraints` are **injected into the pod's `Spec.TopologySpreadConstraints` on the scheduling-time pod copy**, once at pod ingestion (before the first `topology.Update`). From that point on the defaulted constraints are indistinguishable from per-pod ones, so all existing machinery — `TopologyGroup` synthesis in `newForTopologies`, `ScheduleAnyway` relaxation, consolidation — flows through the same per-pod path with no new logic. Matching the plugin's semantics:

- **Applies only when the pod declares no constraints of its own.** A pod with any `topologySpreadConstraints` uses those and the cluster default is not applied — the same all-or-nothing rule the scheduler's plugin uses (the default set is applied only when the pod's own list is empty).
- **`whenUnsatisfiable` is honored exactly as for per-pod constraints today.** Under the default `preference-policy: Respect`, both `DoNotSchedule` and `ScheduleAnyway` become hard `TopologyGroup`s that are then relaxed one at a time during preference relaxation (`preferences.go`, `removeTopologySpreadScheduleAnyway`); under `Ignore`, `ScheduleAnyway` defaults are dropped. This means a synthesized `ScheduleAnyway` default rides the exact same relaxation path that per-pod `ScheduleAnyway` constraints already use — no new relaxation logic.
- **`maxSkew`, `minDomains`, and `topologyKey` handling are unchanged.** The synthesized constraints feed the existing `TopologyGroup` machinery, including the `hostname` special case (`domainMinCount` returns 0 for `kubernetes.io/hostname`, since Karpenter can always create a new node/domain).

Injection happens **at pod ingestion, not inside `newForTopologies`**, and this placement is deliberate. Preference relaxation works by mutating the pod copy's `Spec` — `removeTopologySpreadScheduleAnyway` (`preferences.go`) deletes a `ScheduleAnyway` entry from `Spec.TopologySpreadConstraints`, and the scheduler then calls `topology.Update` to re-derive groups from the mutated spec. If the defaults were synthesized inside `newForTopologies` (from a value held on `Topology`) rather than written to the spec, relaxation would delete nothing (the spec is empty) and every `Update` would re-synthesize the same group — so a `ScheduleAnyway` default could never be relaxed and would behave like a hard `DoNotSchedule`. Injecting into the spec once, upstream of the relax loop, makes relaxation delete the entry for real and keeps it deleted on subsequent iterations. The injected constraints exist only on the scheduling-time pod copy; Karpenter never writes them back to the API server (it emits NodeClaims), and the real scheduler applies the same default itself at bind time, so there is no double-application. The `SchedulerConfiguration` value is decoded and validated once at startup and carried via context, reaching the ingestion step the same way `preference-policy` does today.

### **Interaction with Existing Features**

- **`preference-policy`.** Synthesized `ScheduleAnyway` defaults participate in the existing preference-relaxation flow identically to per-pod `ScheduleAnyway` constraints. `Ignore` drops them; `Respect` treats them as hard-then-relaxed.
- **Consolidation.** The default constraints must be applied in the consolidation scheduling simulation as well, not just provisioning — otherwise consolidation could re-pack pods into a distribution the scheduler would reject, re-creating the divergence after the fact. Because injection happens at the shared pod-ingestion step, any scheduling simulation (provisioning or consolidation) picks up the defaults automatically.
- **Drift.** Changing the option affects future provisioning and consolidation simulations; whether an existing node should be considered drifted when the default changes is an [open question](#open-questions).
- **Per-pod constraints.** Unchanged and take precedence — a pod that declares its own spread is never subject to the cluster default.

### **Observability**

- A one-time startup log line records the parsed scheduler configuration (including the default topology spread constraints).
- Scheduling failures use the **existing** `FailedScheduling` event and topology error path unchanged. Because a defaulted constraint is injected onto the pod copy's spec, an unschedulable default surfaces through the same `topologyError` (`topology.go`) that a per-pod constraint would — no new event or reason is added by this change. The tradeoff is that such an event does not distinguish "failed due to a cluster default" from "failed due to the pod's own constraint," which is a real debugging hazard for cluster-level defaults (use case 2): the constraint is invisible in the pod spec, so the operator sees a topology failure on a pod whose manifest declares no spread. Improving that attribution is deliberately **out of scope** here — it would require tracking which constraints were injected — and is left as a potential future improvement to Karpenter's scheduling-failure messaging generally.
- No new status conditions are required.

### **Edge Cases**

- **Pod partially constrained.** Per the scheduler's plugin semantics, defaults are applied only when the pod's own constraint list is *entirely* empty. A pod that declares even one `topologySpreadConstraint` uses only its own set; the cluster default is not merged in per-key.
- **`DoNotSchedule` default that cannot be satisfied.** Mirrors a per-pod `DoNotSchedule` today: Karpenter launches nodes in the required domains, or the pod fails scheduling with a `topologyError`. This is intended — and is why an operator applying a cluster-wide default should generally prefer `ScheduleAnyway` unless strict spreading is truly required.
- **`hostname` topology key.** Handled by the existing special case (`domainMinCount` returns 0 for `hostname`) because Karpenter can always create a new node/domain — no change.
- **Malformed config value.** A `SCHEDULER_CONFIG` value that fails to decode or carries an unknown field fails fast in `Options.Parse` at startup (the operator does not come up), so misconfiguration surfaces immediately rather than at scheduling time. An empty or absent value is valid and means "no overrides."

## **Alternatives Considered**

### **Alternative 1: Load the config from a file (`--scheduler-config=<path>`)**

Keep the same `SchedulerConfiguration` type, but supply it as a mounted file path rather than an env var value — the Kubernetes-idiomatic "component configuration" shape that `kube-scheduler` (`--config` / `KubeSchedulerConfiguration`) and `kubelet` (`--config` / `KubeletConfiguration`) use.

**Pro:** cleaner authoring for a hand-managed OSS install — an operator writes a readable YAML file and points the controller at it, rather than embedding a document in an env var. It is also the closest analog to how the scheduler itself is configured.

**Con — and why it is not the primary proposal:** a file has to come from somewhere. Delivering per-cluster config as a file means mounting it from a ConfigMap (or projecting it), plus a producer that writes the file's contents. That is exactly the kind of dynamically-sourced, mounted config Karpenter moved *away* from when it removed the `karpenter-global-settings` ConfigMap (see Proposal). It also adds real machinery for integrated/managed operators that today project all controller config as env vars and have no file/ConfigMap-mount path. The env-var value keeps the identical schema and decoding while fitting the existing configuration model. If a future need arises, accepting *both* a file path and an inline value is a small, backward-compatible addition — the schema does not change.

### **Alternative 2: Accept the upstream `KubeSchedulerConfiguration` schema directly**

Instead of Karpenter defining its own config type, accept the actual `KubeSchedulerConfiguration` YAML and reach into `profiles[].pluginConfig[]` to pull out the sections Karpenter understands (`PodTopologySpread.defaultConstraints` today, others later), ignoring the rest.

**Pro:** the operator copies their scheduler config *verbatim* — the best possible copy-paste experience — and it is maximally future-proof, since a new plugin config the operator already has is picked up with no Karpenter type change.

**Con — and why it is not the primary proposal:** it **couples Karpenter to the versioned `KubeSchedulerConfiguration` schema** (`kubescheduler.config.k8s.io`, which has churned across `v1beta1`/`v1beta3`/`v1`). Karpenter would have to vendor and track that API, decode the version the operator supplies, and keep pace as upstream evolves it — a maintenance and compatibility burden for the sake of a handful of fields. Karpenter also does not otherwise depend on scheduler component config, so this would be a novel, heavy dependency. The chosen approach (a Karpenter-owned type that *mirrors the shape* of the relevant fields) keeps the copy-paste experience for the fields that matter while letting Karpenter own its own versioning. This variant remains a reasonable future direction if the set of mirrored fields grows large enough that maintaining a parallel type becomes the larger cost.

### **Alternative 3: Discover the defaults from an in-cluster resource**

Rather than have the operator supply the constraints at all, have Karpenter read them from whatever the cluster already exposes — the idea being "don't make the operator duplicate config that's already in the cluster." Rejected because the scheduler's default topology spread configuration is **not reliably readable from anywhere Karpenter runs**:

- **It is not a served API resource.** `KubeSchedulerConfiguration` (which holds the `PodTopologySpread` plugin's `defaultConstraints`) is a *component config* type decoded from the scheduler's `--config` file at startup. It is not registered as an API object — there is no discovery endpoint, no `kubectl get`, and no standard ConfigMap the API server exposes for it. In a typical kubeadm setup it is a host-path file backing a static pod, not an in-cluster object.
- **It is inaccessible in managed control planes.** kube-scheduler runs in the managed control plane on EKS/GKE/etc., and the data plane where Karpenter runs cannot read its config at all — the same reason [Alternative 2](#alternative-2-accept-the-upstream-kubeschedulerconfiguration-schema-directly) is not viable via live reads.
- **It is not recorded onto scheduled pods.** The scheduler applies `defaultConstraints` in-memory during scheduling and does not write them back to `pod.spec.topologySpreadConstraints`; a defaulted pod still shows an empty constraint list. So Karpenter cannot infer the defaults by observing pods (the same "invisible constraint" property described in use case 2), nor reliably reverse-engineer them from observed placement.
- **`defaultingType: System` has no literal list to read.** When the plugin uses `System` defaulting rather than an explicit `List`, the effective constraints are Kubernetes' built-in derived set, not a value present in the config — so "read the constraints" is not even a single well-defined thing to read (see [Open Questions](#open-questions)).

Because there is no authoritative, API-served, managed-control-plane-accessible representation of the scheduler's default topology spread, the configuration must be supplied to Karpenter explicitly, with keeping it in sync being the operator's (or platform's) responsibility.

### **Alternative 4: A per-setting scalar env var / flag**

Add a single flag such as `--default-topology-spread-constraints` whose value is a serialized list of constraints (the `--feature-gates` precedent). Rejected as the primary surface because it does not scale: every additional scheduler-mirroring setting needs its own new flag/env var. A structured `SchedulerConfiguration` groups related settings under one value and grows by adding fields.

### **Alternative 5: A watched ConfigMap or cluster-scoped config CRD**

Deliver the config through a Kubernetes object Karpenter watches — a ConfigMap or a new singleton config CRD — so changes reconcile live without a controller restart.

A watched object is the most Karpenter-native shape (NodePool / NodeClass / NodeOverlay are all watched CRDs), it gives live reload with no pod restart, and it is `kubectl get`-visible and RBAC-controllable — better debuggability than an opaque env var on the controller pod.

It is nonetheless rejected as the primary surface for a single, decisive reason: **it reverses a settled upstream decision.** Karpenter previously had exactly this — the `karpenter-global-settings` ConfigMap — and **deliberately removed it** (deprecated at v1beta1, dropped in v0.33.0) in favor of env/flag configuration, explicitly moving away from dynamically-reloaded global config. Re-introducing a watched global-config object cuts against that direction, and restart-on-change is the documented, expected model for Karpenter controller configuration today. The env-var surface stays consistent with that model.

### **Alternative 6: Expose the default as NodePool CRD fields**

Add `defaultTopologySpreadConstraints` to `NodePoolSpec` (peer to `Disruption`/`Weight`), following the `Disruption.ConsolidationPolicy` precedent. Rejected as the *primary* surface because this mirrors a *cluster-scoped* `kube-scheduler` setting — a single scheduler config governs the whole cluster, so per-NodePool values could contradict each other and the actual scheduler. A controller-level value matches the true scope. A NodePool-level override could be a future addition if per-pool demand appears.

## **Backward Compatibility**

Fully backward compatible. When `SCHEDULER_CONFIG` is unset (or omits `podTopologySpread`), pods without explicit constraints are treated exactly as today. No existing YAML, NodePool, or pod spec changes. The surface is opt-in.

## **Graduation Criteria**

No feature gate. The surface is opt-in through `SCHEDULER_CONFIG` and defaults to today's behavior (no value, or no `podTopologySpread` section, is a no-op), so a user who sets nothing sees no change and there is no default-on risk to stage through alpha/beta. The change ships enabled. Because the config carries no `apiVersion`, the schema is evolved additively — new fields are added as optional, matching how Karpenter's existing flag/env options grow — rather than through explicit versioned conversions.

The validation bar before merge is that default-topology-spread simulation is validated against `kube-scheduler`'s `PodTopologySpread` plugin across the cases that matter: `zone` and `hostname` topology keys, `ScheduleAnyway` and `DoNotSchedule`, `minDomains`, and the partially-constrained pod (default *not* applied).

## **Open Questions**

1. **Drift hash.** Should a change to the scheduler config's `defaultConstraints` be reflected in the NodePool/NodeClaim drift hash so existing nodes are re-evaluated, or is applying it to new provisioning and consolidation only the right scope?
2. **Config versioning.** This RFC omits an `apiVersion`/`kind` header and evolves the schema additively (new optional fields), consistent with Karpenter's flag/env options. Is additive-only evolution sufficient, or should the value carry an `apiVersion` (and, if so, its own config group like `kube-scheduler`'s `kubescheduler.config.k8s.io`) before any change that can't be made additively — e.g. renaming or restructuring a field?
3. **Scheduler internal defaults.** `kube-scheduler` applies built-in *internal* default constraints (`hostname`/`zone`, `ScheduleAnyway`) when an operator sets no `defaultConstraints`. Should Karpenter mirror those built-in defaults out of the box (so it agrees with a default-configured scheduler even when nothing is set), or only honor explicitly-configured constraints as this RFC proposes? Mirroring them would change behavior for clusters that set nothing, so it is left as an explicit decision rather than folded into this opt-in change.