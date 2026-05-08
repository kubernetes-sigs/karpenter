# RFC: Configurable MaxNodeReadyTime

## Motivation

Karpenter currently assumes nodes will reach the **Ready** state (and clear any startup taints) within a fixed time (roughly the same ~15-minute window used for registration). This can be problematic for workloads with lengthy startup procedures.  For example, GPU nodes often spend several minutes installing drivers at boot, and Windows nodes may run extended bootstrapping and security scans before becoming ready.  If the node takes longer than the default timeout to become Ready (and remove its startup taints), Karpenter today will terminate the node as “unhealthy,” even though it might only need a bit more time to initialize fully. 

Use cases where a longer readiness timeout is needed include: 

- **GPU driver installation** during node bootstrap (drivers and CUDA libraries can take extra time to install and initialize).  
- **Windows node initialization** (Windows AMIs often run substantial setup tasks on first boot).  
- **Network plugin or CNI startup taints** (e.g. waiting for a CNI DaemonSet to be ready and remove its startup taint).  
- **Large container images or pre-pulled archives** that delay node readiness.  

These scenarios motivate making the “time-to-ready” threshold configurable, rather than hardcoding it. 

## Non-Goal

We are *not* proposing any specific cloud-provider defaults for this timeout.  That is, we will not bake in image-specific or provider-specific default values.  Instead, we focus on allowing end-users to configure their own timeout policies at the NodePool or NodeClass level.  (Future work may consider cluster-level or cloud-default flags, but that is out of scope for this RFC.)  

## Configuration

### Option 1 (Preferred): NodePool-level `maxNodeReadyTime`

We introduce a new field `maxNodeReadyTime` on the NodePool spec (in the `.spec.template.spec` section) and propagate this value into each new NodeClaim.  The NodeClaim liveness controller will use `maxNodeReadyTime` to determine how long to wait for the node to reach **Ready** and remove its startup taints before declaring the node unresponsive.  For example:

```yaml
apiVersion: karpenter.sh/v1
kind: NodePool
metadata:
  name: example-nodepool
spec:
  template:
    metadata: {}
    spec:
      expireAfter: 144h0m0s
      maxNodeReadyTime: 30m  # New: how long to wait for node to become Ready
```

Key points for this option: 

- **Propagation to NodeClaim:**  When Karpenter creates a NodeClaim for this NodePool, it copies the `maxNodeReadyTime` into the NodeClaim’s spec. Even if the NodePool’s field is later updated, the NodeClaim continues using the original value it was created with. This makes it clear that each NodeClaim uses the timeout it was given at creation.  
- **Liveness check:** The NodeClaim lifecycle (liveness) controller will use `nodeClaim.spec.maxNodeReadyTime` when evaluating readiness. If the node does not become Ready (and clear startup taints) within that duration, the NodeClaim is marked failed and the instance is terminated. Importantly, once the node successfully becomes Ready, the timeout has been met and is no longer relevant.  
- **Drift detection:** Because `maxNodeReadyTime` only affects the initial node setup, it is *not* used in drift (ongoing health) evaluation after the node is Ready. In other words, it does not influence decisions unrelated to the startup phase.  
- **Default value:** We will default `maxNodeReadyTime` to 15 minutes to preserve current behavior unless overridden.  

#### Concerns

None identified specifically for this option. It simply makes an existing hardcoded value configurable.

### Option 2: NodeClass-level `maxNodeReadyTime`

Alternatively, we could add a `maxNodeReadyTime` field to the NodeClass spec and propagate it into the NodeClaim. This allows users to set timeouts based on the combination of instance type, AMI, or other NodeClass parameters. For example, a NodeClass for GPU nodes might set `maxNodeReadyTime: 30m`, while a regular Linux NodeClass leaves it at the default. Otherwise, the behavior is similar to Option 1:

- The default readiness timeout remains 15 minutes.  
- If a NodeClass includes a custom `maxNodeReadyTime`, new NodeClaims created using that class will use the specified TTL.  
- When evaluating a NodeClaim, the controller checks if `nodeClaim.spec.maxNodeReadyTime` is set (from the NodeClass). If so, it uses that; otherwise it falls back to the 15m default.  

This option exposes the timeout as a configurable parameter in NodeClass, but does not introduce any preset values beyond the default. 

### Option 3: NodeOverlay

One could imagine using a NodeOverlay to adjust the readiness timeout for certain instances (e.g. by image or label). However, readiness delays are often due to the node’s AMI or configuration rather than the instance type alone. Since NodeOverlay typically matches on instance-type labels (which won’t capture different images or startup procedures), it is not an effective mechanism for this setting. For example, even two nodes of the same instance type might require different timeouts if they use different OS images or bootstrap scripts. Therefore NodeOverlay is not a practical solution for varying the “time-to-ready” threshold. 

## Future Investigation/Discussion

### Cluster-level Default vs NodePool Override

Cluster Autoscaler provides a cluster-wide startup time default, but many users still wanted per-nodegroup (nodepool) overrides. Similarly, we should consider allowing a global readiness timeout setting in Karpenter, with the ability to override it per NodePool or NodeClass. For instance, Karpenter could introduce a top-level default (e.g. in the Provisioner or as a CLI flag) for `maxNodeReadyTime`, and NodePools without an explicit setting would inherit this value. This would let cluster or cloud administrators set a sensible baseline (perhaps different for AWS vs Azure vs other environments) while still letting individual NodePools override it. Note that implementing a cluster-level flag is beyond the scope of this RFC, but it is worth investigating. Karpenter currently has no built-in mechanism for cloud providers or cluster maintainers to inject default timeouts, so this might require a new global configuration field in a future release.

### User Stories

- **Varied startup times:** Different NodePools may have vastly different initialization requirements. For example, one team might use specialized GPU nodes that run lengthy setup scripts before they can schedule pods. They might set `maxNodeReadyTime` to 45m so those tasks can complete. Another NodePool for lightweight service nodes might use a much shorter timeout (e.g. 5m) to quickly recycle any nodes that hang during boot. Supporting a configurable timeout accommodates both scenarios.  
- **Diverse providers/environments:** Some environments (or regions) may have slower startup due to network or hardware differences. A fixed 15-minute window might be fine in most cases (e.g. on-demand AWS), but too short in others (e.g. certain spot instances or on-premise clouds). Making the timeout configurable allows each cluster to tune it to its own conditions.  
- **External delays:** A node’s readiness can be delayed by factors outside of Karpenter’s control. For example, if the control-plane API server is at capacity, the kubelet’s "ready" signal might be delayed even though the VM is up and running. In such a case, automatically deleting the node only adds churn and extra load on the API server. By increasing the readiness timeout, users give the node more time to become healthy without needless replacements.  
- **Avoiding churn:** For slow-but-functional setups (large container pulls, variable network latency, etc.), a longer timeout can avoid the “delete and recreate” cycle. Some users have observed scenarios where adding time allowed a node to finish init tasks, whereas immediate GC would just create another equally-slow node. Allowing users to adjust the timeout helps stabilize scale-up behavior.  

These stories reinforce the need for flexible timeouts during node initialization.

### Cloud Provider Overrides (Out of Scope)

Finally, note that we discussed the idea of cloud-provider-specific overrides for the readiness timeout (similar to how some clouds might recommend different values). This is out of scope for now, but in principle one could imagine the cloud provider’s Karpenter integration setting a recommended default or even writing into the NodeClaim. The preferred approach here is to let the user explicitly configure the desired timeout (at the cluster or NodePool level) rather than hiding it. 

In summary, adding a **`maxNodeReadyTime`** configuration lets Karpenter users control how long the system waits for a node to become Ready (and shed startup taints) before giving up. This makes Karpenter more flexible for diverse use cases and avoids unnecessary node churn when nodes just need more time to initialize.