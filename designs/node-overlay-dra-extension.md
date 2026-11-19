# NodeOverlay DRA Extension

## Motivation

Karpenter must know what resources a node will have before it exists. For DRA devices that comes from `cloudprovider.InstanceType.DynamicResources`, which carries the `ResourceSliceTemplates` a driver will publish once the node registers, plus the `AttributeBindings` naming devices that will share a runtime-resolved attribute value. Only cloud providers can populate it, so support for any given DRA driver is gated on a provider integrating it.

Closing that kind of gap is what NodeOverlay is for. It holds the assumptions a provider cannot derive because they originate outside it, like negotiated pricing, extended resources from device plugins, and HugePages, which [node-overlay.md](./node-overlay.md) singles out because they are allocated at boot, hardware-dependent, and vary "even within identical instance types." A static MIG layout is the same kind of fact, configured out of band at boot and differing between two NodePools on one instance type. Device layouts belong where that class of configuration already lives.

This RFC carries them there with a `DynamicResourceTemplate` CRD that describes a device layout, referenced by a NodeOverlay to declare that the instance types it matches will publish that layout. Templates have several producers. A provider controller can compile its own typed CRD into one, a provider can emit them directly, or a user can write one by hand, which suits the in-house and niche drivers no provider has integrated, where a slice is often a handful of devices with no bindings. Such a driver is adopted as soon as it exists rather than on a provider's release cycle, and a provider can add native support later without breaking the configuration. A provider does not strictly need the CRD at all, which is [rejected below](#a-provider-deriving-layouts-in-memory-with-no-crd).

[PR #2559](https://github.com/kubernetes-sigs/karpenter/pull/2559) first proposed embedding templates in NodeOverlay and explored the API space in depth. It was tabled pending a more mature driver ecosystem, and this RFC builds on that work. Since then the integration in [dra-scheduling.md](./dra-scheduling.md) landed, so the API can be derived from a concrete internal representation, and static MIG gives a concrete driver to design against. See also [#2523](https://github.com/kubernetes-sigs/karpenter/issues/2523).

### Use Cases

#### A device layout configured out-of-band at boot

Some drivers can partition a physical card into smaller logical devices, configured by the cluster administrator at boot. NVIDIA's static MIG mode is the concrete example.

An 8-GPU H100 instance publishes 8 whole-GPU devices by default, and that is what the cloud provider reports. Configuring static MIG through userData (say a `3g.40gb`, a `2g.20gb`, and a `1g.10gb` partition per card) yields 24 devices with different attributes and capacities instead. The provider cannot infer this. It is an administrator decision in a field the provider passes through opaquely, and two NodePools on one instance type may partition differently or not at all.

A pod requesting a partition selects on the profile rather than asking for a whole GPU:

```yaml
kind: ResourceClaim
apiVersion: resource.k8s.io/v1
spec:
  devices:
    requests:
      - name: mig-partition
        exactly:
          deviceClassName: gpu.nvidia.com
          selectors:
            - cel:
                expression: device.attributes["gpu.nvidia.com"].profile == "2g.20gb"
```

```
p5.48xlarge, static MIG configured via userData
  Cloud provider reports:  8 whole-GPU devices, no profile attribute
  Driver will publish:    24 MIG partitions (3g.40gb, 2g.20gb, 1g.10gb per card)
  Claim selects:          profile == "2g.20gb"

  No candidate instance type matches -> pod is never provisioned,
  even though the node that would launch has 8 matching partitions.
```

#### Inter-device topology alignment

A `matchAttribute` constraint requires a set of devices to share an attribute value. Co-locating an accelerator and a NIC on one PCIe root complex is the standard case, where a claim requests a device from `gpu.nvidia.com` and one from `rdma.nvidia.com`, constrained on `resource.kubernetes.io/pcieRoot`. The values are only knowable once the node boots, so a template declares which devices will share a value rather than what it is.

This is mostly a cloud provider concern, since PCIe topology is a property of the instance type. It constrains the API regardless, because `cloudprovider.AttributeBinding` identifies devices by fully qualified `DeviceID`, so bindings already span drivers and the CRD must express that. Hence `attributeBindings` sits above `resourceSliceTemplates`, and [Conflict Resolution](#conflict-resolution) is atomic rather than per driver.

### Non-Goals

- Non-node-local devices and relative topology requirements. Templates are node-local, and the slices carrying them return `nil` from `NodeSelector()` and `false` from `AllNodes()` unconditionally ([pkg/scheduling/dynamicresources/types.go](../pkg/scheduling/dynamicresources/types.go)). Future work, gated on the allocator lifting its non-node-local in-flight device exclusion.
- Acting on a template that misdescribes reality. Karpenter reports divergence between a template and what the driver publishes (see [Detecting Divergence](#detecting-divergence)), but does not block initialization, taint the node, or reconcile the difference.
- Cloud provider configuration surfaces. A provider may want a typed CRD that validates a MIG layout against the profiles a GPU model supports and generates a `DynamicResourceTemplate` as output. This RFC specifies the primitive such an object would target.
- Changes to the upstream ResourceSlice schema.

## Proposal

### Proposed Spec

A new cluster-scoped `DynamicResourceTemplate` CRD describes a device layout. NodeOverlay gains an optional `dynamicResourcesRef` pointing at one.

The CRD maps onto `cloudprovider.DynamicResources`, so the controller populates a struct the scheduler already reads. The mapping is close but not a mirror. The internal string handles become plain strings, `Pool.Name` flattens to `poolName`, and fully qualified `DeviceID`s in bindings become name references.

```yaml
apiVersion: karpenter.sh/v1alpha1
kind: DynamicResourceTemplate
metadata:
  name: h100-3g-2g-1g
spec:
  resourceSliceTemplates:
    - driver: gpu.nvidia.com
      poolName: h100-mig
      devices:
        - name: gpu-0-mig-3g40gb
          attributes:
            productName: {string: "NVIDIA H100 80GB HBM3"}
            type: {string: "mig"}
            architecture: {string: "Hopper"}
            profile: {string: "3g.40gb"}
            cudaComputeCapability: {version: "9.0.0"}
            # Static per card index on this instance type, so it is declarable.
            # pcieRoot is not, and is bound below.
            resource.kubernetes.io/numaNode: {int: 0}
          capacity:
            memory: {value: "40448Mi"}
            multiprocessors: {value: "60"}
        - name: gpu-0-mig-2g20gb
          attributes:
            profile: {string: "2g.20gb"}
            # ... remaining attributes identical to gpu-0-mig-3g40gb
          capacity:
            memory: {value: "20096Mi"}
            multiprocessors: {value: "32"}
        # ... gpu-0-mig-1g10gb, then gpu-1 through gpu-7: 21 more devices, differing
        # only in profile-invariant topology. gpu-4 onward are numaNode 1.
  attributeBindings:
    # parentUUID and pcieRoot are both per physical card and neither is known before
    # boot, so each card's partitions bind separately for each.
    - attribute: gpu.nvidia.com/parentUUID
      devices: [gpu-0-mig-3g40gb, gpu-0-mig-2g20gb, gpu-0-mig-1g10gb]
    - attribute: resource.kubernetes.io/pcieRoot
      devices: [gpu-0-mig-3g40gb, gpu-0-mig-2g20gb, gpu-0-mig-1g10gb]
    # ... the same two bindings for gpu-1 through gpu-7, plus driverVersion and
    # cudaDriverVersion bindings spanning all 24 devices.
```

```yaml
apiVersion: karpenter.sh/v1alpha1
kind: NodeOverlay
metadata:
  name: h100-static-mig
spec:
  weight: 100
  requirements:
    # Scope to pools carrying the matching MIG userData, via a label on the NodePool
    # template set next to that userData.
    - key: mig-profile
      operator: In
      values: ["3g-2g-1g"]
    # Bound the device count: this layout is 3 partitions on each of 8 cards.
    - key: node.kubernetes.io/instance-type
      operator: In
      values: ["p5.48xlarge"]
  dynamicResourcesRef:
    name: h100-3g-2g-1g
```

The overlay selects on a label, not a NodePool name, because userData lives on the NodeClass. Listing NodePools by name breaks when two share one, and repointing a `nodeClassRef` would leave the overlay applying to nodes that no longer partition. Selecting on the NodeClass is impossible, since `WellKnownLabels` has no key for it, but overlay requirements match `spec.template.labels`, so a label next to the userData couples the overlay to what determines the layout.

`attributeBindings` sits above `resourceSliceTemplates` rather than inside a template, which is what lets one binding span two drivers. A binding's `attribute` must be fully qualified, matching upstream, where `matchAttribute` is a `FullyQualifiedName` that has to carry a domain. Device attributes may be bare, since they are implicitly scoped to their slice's driver, but a binding spanning drivers has no domain to imply.

Device names must be unique across the whole object, not just within a slice as upstream requires, which makes a binding a plain list of names with no per-reference qualification.

Written out, this layout is 24 devices and 18 bindings. That is three partitions per card, a `parentUUID` and a `pcieRoot` binding per card, and two node-wide bindings naming every device.

#### Supported Fields

Two principles apply. A field that does not influence instance type selection is excluded, and so is one the scheduling engine cannot consume.

| Field | Supported | Rationale |
| --- | --- | --- |
| `driver` | Yes | Device identity, and the unit of override resolution. |
| `pool.name`, as `poolName` | Yes | Declares which devices share a counter budget. Device identity. |
| `pool.generation`, `pool.resourceSliceCount` | No | Tracked by the driver at runtime, nothing to declare up front. |
| `devices`, `devices[].name` | Yes | The devices, and the referent for `attributeBindings`. |
| `devices[].attributes`, `devices[].capacity` | Yes | Read by CEL selectors and `matchAttribute`. |
| `sharedCounters`, `devices[].consumesCounters` | Yes | Partitionable devices are supported. |
| `devices[].allowMultipleAllocations` | Yes | Consumable capacity is supported. |
| `nodeName`, `devices[].nodeName` | No | Not knowable before launch. |
| `nodeSelector`, `allNodes`, `perDeviceNodeSelection`, `devices[].nodeSelector`, `devices[].allNodes` | No | Templates are node-local. See [Non-Goals](#non-goals). |
| `devices[].taints` | No | Not supported by the allocator. |
| `devices[].bindsToNode`, `devices[].bindingConditions`, `devices[].bindingFailureConditions` | No | Irrelevant to instance type selection, before or after graduation. |
| `devices[].nodeAllocatableResourceMappings` | No | Not consumed by the allocator. |

`sharedCounters` and `devices` are mutually exclusive per slice, matching upstream, which sets `zeroOrOneOf` on the two fields. The allocator tolerates both on a template, but a template setting both would describe a slice no driver can publish. So a partitionable layout uses two slices in one pool:

```yaml
resourceSliceTemplates:
  - driver: gpu.nvidia.com
    poolName: h100-partitioned
    sharedCounters:
      - name: gpu-0-memory
        counters:
          memory: {value: 80Gi}
  - driver: gpu.nvidia.com
    poolName: h100-partitioned          # same pool: draws from the budget above
    devices:
      - name: gpu-0-3g40gb
        attributes:
          profile: {string: "3g.40gb"}
        consumesCounters:
          - counterSet: gpu-0-memory
            counters:
              memory: {value: 40448Mi}
```

New upstream fields are evaluated against the two principles as they appear.

#### Pool Names

`PoolKey{Driver, Pool}` is the scope of a shared counter budget ([pkg/scheduling/dynamicresources/allocationtracker.go](../pkg/scheduling/dynamicresources/allocationtracker.go)), so slices in the same pool draw from one budget while slices in different pools are independent. That grouping depends on how the driver models the hardware, so `poolName` is user-supplied.

A `poolName` colliding with a pool the driver really publishes is worth avoiding for legibility, but it is not a correctness requirement. Allocation tracking and counter budgets keep template and published devices apart, and attribute bindings must key on the template-flagged device ID so that a template binding can never apply to a published device that happens to share a driver, pool, and name.

### How It Works

Resolution and application are confined to the NodeOverlay controller, leaving the scheduler, allocator, and cloud provider interface untouched. Divergence detection below is the one piece outside it.

1. `nodeoverlay.Controller.Reconcile` resolves each overlay's `dynamicResourcesRef` and converts the referenced object into a `cloudprovider.DynamicResources`, turning its strings into the handles the internal representation uses. Binding device names are resolved to `DeviceID`s against the object's own slices.
2. The result is recorded as a third kind of update in `instanceTypeUpdate` alongside `Price` and `Capacity` in [pkg/controllers/nodeoverlay/store.go](../pkg/controllers/nodeoverlay/store.go), keyed by NodePool and instance type. Unlike `Capacity`, which merges per resource name, this is a single slot guarded by weight.
3. `internalInstanceTypeStore.apply` sets `DynamicResources` on the instance type copy it returns, merged against the cloud provider's per the rules below.
4. `templateSlicesForInstanceType` in [pkg/controllers/provisioning/scheduling/dra.go](../pkg/controllers/provisioning/scheduling/dra.go) and `BuildAttributeBindings` in [pkg/scheduling/dynamicresources/attributebindings.go](../pkg/scheduling/dynamicresources/attributebindings.go) already read off `InstanceType.DynamicResources`, so overlay-supplied devices and bindings reach the allocator by the same route as provider-supplied ones.

The controller watches `DynamicResourceTemplate` so template edits re-evaluate overlays promptly. RBAC gains `get;list;watch` on the new resource.

#### Detecting Divergence

A template promising devices the driver does not publish gives a healthy node with a permanently `Pending` pod and nothing to inspect, so Karpenter reports it.

The nodeclaim lifecycle controller owns both halves. The instance type only becomes concrete there, in `PopulateNodeClaimDetails`, and initialization already lists the node's `ResourceSlice`s to gate on driver publication. So initialization resolves the template from the overlay store using the NodeClaim's nodepool and instance-type labels, and on a difference emits a `DynamicResourceDivergence` event on the NodeClaim and increments `karpenter_nodeclaims_dynamic_resource_divergence_total`. Reading the store is a dependency that controller does not have today. Nothing is written to the NodeClaim, so the comparison uses the current template rather than the one in force at launch, which is the tradeoff for not adding a field.

Comparison is on device count per driver, not on names or attributes. A template's device and pool names are Karpenter's own, and `poolName` is deliberately chosen not to collide with the driver's, so matching on either would fire on every correctly configured node. Karpenter reports and does not block, since the node is already running.

#### Override Semantics

Overlay templates replace cloud provider templates per driver. Drivers the overlay does not mention are left as the provider reported them.

| Cloud provider declares | Overlay declares | Result |
| --- | --- | --- |
| `gpu.nvidia.com`: 8 whole GPUs<br>`rdma.nvidia.com`: 2 NICs | `gpu.nvidia.com`: 24 MIG partitions | `gpu.nvidia.com`: 24 MIG partitions<br>`rdma.nvidia.com`: 2 NICs |

Unlike `spec.capacity`, which is additive, a declared layout means "instead of" rather than "in addition to". Adding static MIG templates to the provider's whole-GPU templates would leave the simulation seeing 8 whole GPUs and 24 partitions on the same node. A provider `AttributeBinding` is dropped when replacement removes any of its devices, not only when it removes all of them. A binding asserts that a set shares a value, and keeping the surviving subset would assert something the provider never stated.

### Conflict Resolution

At most one matching overlay may contribute a `dynamicResourcesRef` per instance type. The highest weight wins. Equal weights conflict and are resolved as NodeOverlay already resolves them, by name.

Keying per driver instead, the way capacity is keyed per resource name, would let separate overlays own separate drivers for one instance type. It is rejected because bindings may span drivers, so a per-driver merge could keep one half of a binding and drop the other. The cost is that teams owning separate accelerators cannot own separate overlays for one instance type. Loosening this later is backward compatible, since no configuration that works under atomic keying breaks under per-driver keying, so it does not need deciding before alpha.

Validation reuses the existing path, so the losing overlay is dropped entirely and reports `ValidationSucceeded=False` with reason `Conflict`. Provisioning continues, so the cluster fails open even though everything that overlay contributed is dropped. The message is currently a bare `conflict with another overlay`. It should name the other overlay and the first conflicting instance type, which applies to price and capacity equally.

### Interaction with Existing Features

Consolidation and drift are inherited. Disruption reuses the scheduler through `disruption.SimulateScheduling`, so it evaluates claims through the same allocator and templates. As with the rest of NodeOverlay, changes take effect through the normal consolidation cycle and do not trigger drift. Overlay-supplied drivers populate the `karpenter.sh/requested-dra-drivers` annotation exactly as provider-supplied ones do.

Once a node initializes, its published ResourceSlices, not the template, are what scheduling against it uses. Before that, `draExistingNode.ResourceSlices` returns templates off the live overlaid instance type, so between launch and initialization a template edit changes what pods can be placed on a running node. That is the one case where a template edit reaches an existing node, and the second reason to order edits as below.

A template and the userData creating the layout must change together but disrupt differently. Editing the template disrupts nothing, while editing userData changes the NodeClass hash and drifts every node. Order the edits so the promise is never wider than reality. When widening a layout, change userData and let nodes roll before pointing an overlay at the template. When narrowing one, drop the `dynamicResourcesRef` first.

### Validation and Observability

`DynamicResourceTemplate` has no controller and no status for alpha. Most checks are declarative:

- Device name uniqueness within a slice, via `+listType=map` with `+listMapKey=name`.
- The `sharedCounters`/`devices` exclusion, one CEL rule.
- `MinItems=2` on a binding's `devices`, since `BuildAttributeBindings` silently skips shorter ones.
- `MaxItems` mirroring upstream's per-slice and per-device limits, so a template cannot describe a slice no driver could publish. The one that needs stating is `devices`, capped at 128 but at 64 as soon as any device carries taints, consumes counters, or uses list attributes.

Two checks need Go, both in the overlay controller, which already resolves the reference. Device names must be unique object-wide, and no device may appear in more than one binding for one attribute. Bindings for an attribute merge transitively, so naming a `gpu-1` partition in `gpu-0`'s `parentUUID` binding would fuse two cards into one group and offer a placement the node cannot satisfy. Two bindings sharing a device already mean the same thing as one, so rejecting the overlap costs nothing, while checking every binding against every other in CEL would risk the cost budget.

An overlay naming a missing template, or one whose bindings fail either check, reports `ValidationSucceeded=False` alongside the existing conflict detection, since `NodeOverlay.RuntimeValidate` has no API client. Deleting a referenced template is the same case, and nodes already launched are unaffected. Errors surface on the overlay rather than the template, so the message must name both, and an unreferenced template is unchecked beyond the API server.

### Template Ownership

Under provider generation a provider controller creates cluster-scoped templates while users create the overlays referencing them, so it needs to be clear who owns the object. A generated template carries `karpenter.sh/managed-by` naming the controller that produced it. Karpenter does not read the label, so this is a convention for humans and for the generating controller's own pruning, not something enforced.

That is what makes "overridable" concrete. Overriding a generated layout means copying it to a new template and repointing the overlay, not editing the original, since edits to a generated object are stomped on the next reconcile. Renaming or garbage-collecting a referenced template behaves as deleting one does, covered above.

## Alternatives Considered

### Inline templates in NodeOverlay

Slices and bindings directly under `spec`, with no new CRD. A realistic layout is roughly 24 devices of 10 attributes each, duplicated in every overlay that needs it with no reuse across NodePools, and it pushes a single object toward the size limit.

### A per-driver CRD with a list of references

One object per driver with its own bindings, and `dynamicResourcesRefs` as a list. Cleaner per-driver ownership, but bindings can no longer span drivers, and merging several references reopens conflict resolution.

### `dynamicResourcesRef` on the NodePool template

Put the reference on `NodePool.spec.template`, the same object carrying the label the overlay selects on. One less object to follow, and no chance of the label drifting from the userData it stands in for. It is rejected because a device layout is per instance type and a NodePool spans many. A NodePool permitting both an 8-GPU and a 4-GPU instance type could carry only one layout, and the appendix case would need a NodePool per layout. Overlays already scope by instance type, which is what the layout needs.

### Templated device generation

A `count` on a slice plus an index token in device names, with a `perIndex` or `all` scope on bindings, so 24 devices could be written as 3. Rejected on two independent grounds. Writing less only pays off at the scale where something is generating the layout anyway, and a generator writes out every device for free, while the hand-written cases are small enough not to need it. So the fields buy little and add a configuration dimension that has to interact with bindings, replacement, and conflict resolution. And it is not general even for hand authors, since attributes that vary per card without following from the index, `pciBusID` and `pcieRoot` and `numaNode`, are not formulas and would still be written out one by one.

### A provider deriving layouts in memory, with no CRD

The null option. `GetInstanceTypes` already receives the NodePool, so a provider can populate `DynamicResources` per NodePool directly and support static MIG with no new API at all. It is the cheapest thing that serves the motivating case, and rejected only because the layout Karpenter simulated against stays provider-internal, with nothing to inspect when a pod does not schedule, and users whose provider has not integrated a driver have no path.

### Synthesized pool names

Have Karpenter invent the pool name rather than accept one. It would have to guess which devices share a counter budget, and it hides that boundary from the user. A synthesized name also still surfaces in diagnostics as an opaque value.

### Unstructured objects, or the upstream schema unchanged

Version-decoupling buys nothing here, since these are consumed by Karpenter's own scheduler, not a separately versioned process. The upstream schema is familiar but carries required fields with no value we can know before launch, and optional fields Karpenter cannot honor. The tradeoff the concrete schema accepts is having to decide on every future upstream field one by one.

### Bound attributes declared in place on the device

A `bindingKey` on `DeviceAttribute`, mutually exclusive with a concrete value, where a shared key implies a shared runtime value. Equally expressive and more concise, but it spreads one binding across several devices, which is easier to misconfigure.

### CEL expressions in the template

Inline CEL for relative topology and bound attributes, e.g. `${boundAttribute("pcie-root-1")}`. Needs no new structural fields and points toward [KEP-5254](https://github.com/kubernetes/enhancements/pull/5391)'s CEL-based claim constraints, but is far more expressive than the requirement and cannot be understood from the fields alone.

### Additive or replace-everything override semantics

Additive double-counts devices, so static MIG cannot be expressed at all. Replacing everything is a simpler rule but forces a user overriding one driver to re-declare the rest.

## Backward Compatibility

Additive. `dynamicResourcesRef` is optional and existing NodeOverlay manifests are unaffected.

## Graduation Criteria

No new feature gate. With the `NodeOverlay` gate off the overlay store is never consulted, and `--ignore-dra-requests` defaults to true, so the field does nothing on a default install either way. Set while DRA requests are ignored it is accepted and silently does nothing, which the overlay should surface on its status rather than leaving to be discovered.

- Alpha: CRD at `v1alpha1`, off by default via the `NodeOverlay` gate.
- Beta: a driver beyond static MIG exercised end to end, one cloud provider generating a template from its own CRD, KWOK e2e coverage, and the supported-field set holding across an upstream ResourceSlice release.
- GA: DRA support must be GA first.

A gate of its own would decouple NodeOverlay's graduation from DRA's, but NodeOverlay is `v1alpha1`, so declining to carry `dynamicResourcesRef` into the next API version already gives that lever without a third switch.

## Future Work

- A status subresource and conditions on `DynamicResourceTemplate`, if validating unreferenced templates turns out to matter.
- Non-node-local devices, once the allocator supports them.

## Appendix: NodePools Spanning Multiple Instance Types

A GPU NodePool commonly permits several instance types with different accelerators, needing one template per layout and one overlay per instance type. They compose. Updates are keyed by NodePool then instance type, an overlay is skipped for instance types its requirements do not match (so two disjoint overlays may share a weight), and the allocator runs its DFS per instance type with bindings scoped to `(attribute, nodePool, instanceType)`, so divergent layouts on one NodeClaim never bleed together.

Four caveats:

- Layering a broad overlay under a specific one needs distinct weights, since conflict is only reported between equal weights, and one conflicting instance type drops the overlay everywhere it matched.
- Broad requirements over-declare. An overlay keyed on a GPU model matches every instance type carrying it regardless of count, so a 24-device template can land on a single-GPU variant. Scope on instance type when counts differ.
- Offering-level requirements do not scope the layout. Updates are stored per instance type, not per offering, so naming `karpenter.sh/capacity-type` still registers the layout for every offering of the matched types.
