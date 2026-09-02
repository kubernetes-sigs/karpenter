# Node Repair Veto

A mechanism for users to veto (block) Karpenter's node repair.

## Motivation

[RFC #3192](https://github.com/kubernetes-sigs/karpenter/pull/3192) makes node repair a *voluntary* disruption method and commits to the principle that users must be able to veto it. It deliberately does not design the veto. This RFC is that design.

Node repair terminates and replaces nodes Karpenter believes are unhealthy.  Today node repair is an *involuntary* disruption method meaning users have no way to veto it. That is a problem whenever the "unhealthy" signal is wrong or the operator knows better than the signal.

The community has also expressed a need to be able to control node repair:
- [kubernetes-sigs/karpenter#2424](https://github.com/kubernetes-sigs/karpenter/issues/2424) asks to be able to veto node repair on certain NodePools
- The node repair mega issue [#750](https://github.com/kubernetes-sigs/karpenter/issues/750) includes multiple operators asking to preserve specific nodes from auto-removal for debugging

### Use Cases

A veto for repair is useful to cluster operators in various situations.
- A **false positive health signal** could flag nodes as unhealthy which causes repair to terminate perfectly good nodes. An operator who knows the signal is a false positive has no lever to stop it. In such cases the only recourse available today is a blunt one: disabling node repair entirely on the cluster.
- A fault the operator knows is **benign or transient** (in the future this could go in a user-configurable `RepairPolicy` as defined in [RFC #3192](https://github.com/kubernetes-sigs/karpenter/pull/3192)).
- A node the operator is **actively debugging** and wants preserved for forensics rather than auto-removed.

### Non-Goals

The veto is one lever but the following adjacent needs are served by other mechanisms:

| Need | Mechanism | Why it's Out of Scope |
| --- | --- | --- |
| **Availability**: "*Don't let repair take down too many pods at once*" | PDBs | A veto should not be used to maintain availability. |
| **Churn**: "*Don't let repair disrupt too many nodes at once*" | Disruption budgets (with `reasons: ["Unhealthy"]`) | A veto should not be used to reduce churn. Additionally, blanket-annotating nodes to prevent churn will also disable legitimate repair. |
| **Graceful Shutdown**: "*Let my pod checkpoint/finish before you take it*" | Graceful drain + pod `terminationGracePeriodSeconds` | This is about *how* repair acts once it starts, not *whether* it acts (which is the veto). |

## Background
Per [RFC #3192](https://github.com/kubernetes-sigs/karpenter/pull/3192), every disruption is described by two orthogonal axes:

- Axis 1: Voluntary vs Involuntary
- Axis 2: Graceful vs Forceful

Axis 1 is relevant to us in this RFC. It describes *whether* Karpenter is allowed to perform the disruption.

- **Voluntary:** Disruption *can be blocked* indefinitely by a veto or an exhausted budget.
- **Involuntary**: The disruption *cannot be blocked* by a veto or an exhausted budget.

**Repair today is an involuntary disruption method**. However as shown by the use cases and the other arguments made in [RFC #3192](https://github.com/kubernetes-sigs/karpenter/pull/3192), **repair should be made a voluntary disruption method**.

## Proposal

### Proposed Spec

Introduce a new `karpenter.sh/do-not-repair` annotation at the node scope. `do-not-repair` will work for repair similar to how `do-not-disrupt` currently works for drift at the node scope.

- Key: `karpenter.sh/do-not-repair`
- Allowed values: `Boolean` 
- A hard veto that blocks *all* repair on this node indefinitely until the annotation is removed. This is the desired escape hatch for a false-positive situation.

```yaml
apiVersion: v1
kind: Node
metadata:
  annotations:
    karpenter.sh/do-not-repair: "true"
```

### How It Works

`do-not-repair` reuses the same machinery `do-not-disrupt` uses today. The flowcharts below show the veto mechanics that `do-not-repair` follows (it is the behavior `do-not-disrupt` already has for drift, `do-not-repair` applies it to repair).

![Node scope `do-not-repair` flowchart](./images/node-repair-veto/node-veto-do-not-repair-flowchart.svg)

### Interaction with Existing Features

- **`do-not-disrupt`**: `do-not-repair` is independent of `do-not-disrupt`. `do-not-disrupt` does not block repair and `do-not-repair` does not block consolidation or drift.
- **Disruption budgets**: Complementary. NodePool scope rate control for repair is expressed as a budget on repair. That bounds *how many* nodes repair touches at once while the veto controls *whether* repair touches a given node.
- **PDBs**: Complementary. PDBs are the availability guarantee once repair starts to act.

### Observability

Once repair is integrated into the disruption pipeline it will start emitting a `DisruptionBlocked` event on the Node and NodeClaim when it is suppressed. So `do-not-repair` blocking repair will be visible.

### Edge Cases

- **`do-not-repair` applied to a Node is indefinite**: This means repair is blocked on the node as long as the annotation is there. A genuinely unhealthy node that was vetoed and forgotten will not be repaired. A `DisruptionBlocked` event surfaces on every reconcile when this is the case. This matches how `do-not-disrupt` behaves today for drift.

## Options Considered

Once repair is voluntary how does a user opt a specific node or workload out of it? The following shapes were considered.

### Option A: New `karpenter.sh/do-not-repair` (Recommended)

#### Pros

- **No breaking change required**: It doesn't require flags beyond the overall node repair flag.
- **Clean intent separation**: It decouples disruption blockers based on the state of the node. This allows a user to block disruption on nodes that are healthy while letting Karpenter fix unhealthy nodes (in this case they would apply only `do-not-repair`). 

  - **Node is healthy** (consolidation, drift): Pods are running. `do-not-disrupt` can be used to block disruption methods that operate on a healthy node.
  - **Node is probably unhealthy** (repair): Pods are probably not running. `do-not-repair` can be used to block the disruption method that operates on an unhealthy node

#### Cons

- **Additional annotation for users**: Users blocking disruption on a node must know to set both if they want to block all disruption. A user who sets `do-not-disrupt` could be surprised to see their nodes still being disrupted by repair.
- **Additional annotation to maintain**: Any future changes we make to how disruption methods are vetoed must be done to both `do-not-disrupt` and `do-not-repair`.

### Option B: Reuse `karpenter.sh/do-not-disrupt`

`do-not-disrupt` would also apply to repair in addition to its existing behaviour.

#### Pros

- **Consistency**: Currently, eventual disruption methods are covered by `do-not-disrupt`. Once repair is eventual it should fall under `do-not-disrupt`'s coverage by the class definition.
- **Free coverage**: Any updates made to `do-not-disrupt` in the future will automatically cover repair.
- **Simpler user experience**: `do-not-disrupt` becomes the singular "leave this node alone" annotation for users.
- **Reduces code complexity**: We would simply add repair as an eventual method covered by `do-not-disrupt`

#### Cons

- **Breaking change**: Making `do-not-disrupt` apply to repair is a breaking change. This is mitigable with a feature flag however it would be a *major* change in behaviour that would require a long migration process.
- **Intent conflation**: Repair is arguably a different class of disruption compared to existing eventual disruption methods like drift. Existing methods operate on healthy nodes with running pods. In this case `do-not-disrupt` serves to block a discretionary action that will cause a disruption in a pod's work (we move from a pod working state to a pod not-working state). Repair operates on a probably-unhealthy node with pods that are probably not running. So in most cases repair is acting on a pod that is not doing any work and it is acting to get the pod to start doing work (we move from a pod not-working state to a pod working state).
- **Granularity**: Users can either block all disruptions methods or none with no in-between. Blocking everything but repair makes sense since repair works differently (see the "Intent conflation" point above)

### Option C: Per-Reason Suppression with `karpenter.sh/do-not-disrupt`

Extend `do-not-disrupt` to allow suppressing each disruption reason independently. This would require JSON in the annotation value. In a manifest the annotation value would look like:

```yaml
karpenter.sh/do-not-disrupt: '{
    "repair": {"enabled": true, "duration": "5m"},
    "drift": {"enabled": true, "duration": "30m"},
    "consolidation": {"enabled": false}
}'
```

#### Pros

- **No *immediate* breaking change required**: Plain `do-not-disrupt`'s functionality can be kept as-is for now and users who want to suppress repair can use the structured annotation value. *However* at some point we might want to include repair in plain `do-not-disrupt` and that would be a breaking change. This is based on the rationale that the long term user experience would be confusing if plain `do-not-disrupt` didn't include all disruption methods.
- **Granularity**: This option has the most granularity since it allows users to independently control disruption blocking for each reason.
- **Consistency**: Same as Option B.
- **Free coverage**: Same as Option B.
- **Allowed by K8s API conventions**: The [API conventions](https://github.com/kubernetes/community/blob/main/contributors/devel/sig-architecture/api-conventions.md) state

> Annotations may carry arbitrary payloads, including JSON documents.

#### Cons

- **Potential footgun**: The veto is meant to be used to pause repair during an emergency. Cluster operators are often applying the annotation by running `kubectl annotate`. Any mistake they make in the annotation is not caught when it is applied, and disruption methods they thought would be blocked are actually not. By making the annotation value a big structured field rather than a simple boolean/duration we significantly increase the chances of such mistakes.
- **Might eventually require a breaking change**: Plain `do-not-disrupt` covering everything except repair is fine for now. But as mentioned above, eventually we might want to change this and would need a breaking change.

### Option D: Per-Reason Suppression with a field on a CRD

If the per-reason suppression can exist in a CRD then it would solve most of the issues with Option C. We would have `do-not-disrupt` as a boolean annotation and the configuration on a CRD (rather than the structured annotation value mentioned above).

#### Pros

- **API server validation**: API server can validate inputs when users apply them, since they exist in a CRD. This means users get an immediate rejection for invalid inputs rather than a silent failure. None of the annotation methods (including what we currently do with `do-not-disrupt`) have this available.
- **No *immediate* breaking change required**: Similar to Option C, plain `do-not-disrupt`'s functionality can be kept as-is for now and users who want to suppress repair can use the CRD field. But again, at some point we need a breaking change.
- **Granularity**: Same as Option C.
- **Consistency**: Same as Option B.
- **Free coverage**: Same as Option B.

#### Cons

- **Where should this go**: The actual CRD on which this would exist is unclear and all the options have issues.
    - **NodePool CRD**: This would only allow configuring disruption on the NodePool scope. We wouldn't be able to stop disruption for a single node or subset of nodes.
    - **NodeClaim CRD**: Currently this isn't a user-facing CRD and the Karpenter docs even explicitly mention that it is [immutable](https://karpenter.sh/docs/concepts/nodeclaims/).
    - **New NodePolicy CRD**: There has been discussion of separating out the node policy (such as TGP) from the NodePool CRD into its own CRD. Node scope per-reason suppression could live here but this is a long term endeavour.

- **Would eventually require a breaking change**: Same as Option C.

## Why Option A

Options A and C are the only options that survive the following conditions.

- **No immediate breaking changes**: We don't want to depend on a breaking change to graduate node repair to beta. A breaking change would require additional flags, user confusion, and a long migration process. This rules out Option B. Options C and D don't require an immediate breaking change to graduate node repair so they aren't ruled out under this point.
- **Granularity**: We want enough granularity to allow users to veto discretionary disruption without vetoing repair. Since repair acts on a probably-unhealthy node users would want to allow Karpenter to fix it. This rules out Option B.
- **Not dependent on long-term endeavours**: We don't want to depend on the completion of long-term endeavours to graduate node repair to beta. This rules out Option D.

**Why not C**

- **More typing means more errors:** The more there is to type the greater the chance of errors. [ALB #3020](https://github.com/kubernetes-sigs/aws-load-balancer-controller/issues/3020#issuecomment-1423367942) shows us that typos in annotations can be a genuine issue. In that case the annotation name was mistyped but it applies equally to the value.
- **String quoting and escaping mistakes**: The JSON string in the annotation value is passed through either YAML (when the annotation is defined in a manifest) or shell (when annotation is applied via `kubectl annotate`). YAML and shell have their own string quoting and escaping which increases the likelihood of errors. [ALB #3910](https://github.com/kubernetes-sigs/aws-load-balancer-controller/issues/3910) shows us that these types of mistakes can bite users (in that case the user had JSON in a YAML string with Helm templating).
- **Entire config needs to be replaced**: During an emergency when a cluster operator is applying the annotation with `kubectl annotate` the entire `do-not-disrupt` config is replaced since `annotate` can only replace the annotation value with a new string. It does not know the value is JSON. This further increases the chances of an error. For example let's say the config already blocks drift and consolidation and now the operator wants to block repair too. In a hurry they might do (abridged command) `annotate 'karpenter.sh/do-not-disrupt={"repair":{"enabled":true}}'`. Now repair is blocked but drift and consolidation are no longer blocked since the config only has repair!

Option C isn't ruled out by any hard constraint but rather for its potential to cause silent errors or unexpected behaviour. With this, Option A is the option left and the one we recommend.

## Backward Compatibility

Fully additive. `do-not-repair` is a new annotation and it changes no existing behavior.

## Graduation Criteria

`do-not-repair` ships with node repair and is gated by the existing `NodeRepair` feature gate. It will graduate alongside node repair.